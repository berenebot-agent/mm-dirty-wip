/**
 * Mammoth Mail relay client.
 *
 * Dial-out WebSocket transport shared with the Hermes connector:
 *   hello -> descriptor -> inbound/outbound frames -> inbound_ack.
 *
 * The client never exposes an inbound port; Mammoth Mail pushes mail down the
 * socket this process opened. The durable cursor lives on Mammoth Mail: an event
 * is acked only after the inbound handler resolves.
 */
import crypto from "node:crypto";
import WebSocket from "ws";
import type { MammothMailInboundEvent, RelayFrame } from "./types.js";

export type MammothMailRelayOptions = {
  baseUrl: string;
  gatewayId: string;
  secret: string;
  onInbound: (event: MammothMailInboundEvent) => Promise<void>;
  onStatus?: (status: "connected" | "disconnected" | "recovering") => void;
  log?: {
    info?: (message: string) => void;
    warn?: (message: string) => void;
  };
  /** How often an in-flight event sends an inbound_processing heartbeat. */
  heartbeatIntervalMs?: number;
  /** How long a completed buffer id is remembered across reconnects. */
  completedTtlMs?: number;
  /** Upper bound on the completed buffer-id cache. */
  maxCompleted?: number;
  /** Initial reconnect delay; tests use a short value. */
  reconnectDelayMs?: number;
  /** Test seam: replace the WebSocket constructor. */
  createSocket?: (url: string, init: { headers: Record<string, string> }) => RelaySocket;
};

/** The subset of the WebSocket surface this client uses. */
export type RelaySocket = {
  readonly readyState: number;
  send(data: string): void;
  close(code?: number, reason?: string): void;
  on(event: "open" | "message" | "close" | "error", listener: (...args: any[]) => void): unknown;
};

type PendingResult = {
  resolve: (messageId: string) => void;
  reject: (error: Error) => void;
  timer: ReturnType<typeof setTimeout>;
};

const OUTBOUND_RESULT_TIMEOUT_MS = 60_000;
const MAX_RECONNECT_DELAY_MS = 30_000;
const INITIAL_RECONNECT_DELAY_MS = 1_000;
const HEARTBEAT_INTERVAL_MS = 20_000;
const COMPLETED_TTL_MS = 10 * 60_000;
const MAX_COMPLETED = 1024;
const WS_OPEN = 1;

/** Builds the relay WebSocket URL for a Mammoth Mail base URL. */
export function websocketUrl(baseUrl: string): string {
  const url = new URL(baseUrl);
  url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
  url.pathname = "/relay";
  url.search = "";
  url.hash = "";
  return url.toString();
}

/**
 * Mints the short-lived HMAC bearer the relay upgrade expects. This matches
 * internal/hermesrelay/server.go: base64url("<gatewayId>:<exp>:<hmac-sha256>").
 */
export function relayBearer(gatewayId: string, secret: string, now = Date.now()): string {
  const exp = Math.floor(now / 1000) + 5 * 60;
  const payload = `${gatewayId}:${exp}`;
  const signature = crypto.createHmac("sha256", secret).update(payload).digest("hex");
  return Buffer.from(`${payload}:${signature}`, "utf8").toString("base64url");
}

export class MammothMailRelay {
  private socket?: RelaySocket;
  private stopped = false;
  private reconnectDelayMs = INITIAL_RECONNECT_DELAY_MS;
  private readonly initialReconnectDelayMs: number;
  private pending = new Map<string, PendingResult>();
  private abort?: AbortSignal;
  private readonly heartbeatIntervalMs: number;
  private readonly completedTtlMs: number;
  private readonly maxCompleted: number;
  private readonly createSocket: NonNullable<MammothMailRelayOptions["createSocket"]>;
  /** Events whose handler is still running, keyed by buffer id. */
  private inFlight = new Map<string, Promise<void>>();
  /** Recently completed buffer ids, so a post-disconnect replay is acked only. */
  private completed = new Map<string, number>();

  constructor(private readonly options: MammothMailRelayOptions) {
    this.initialReconnectDelayMs = options.reconnectDelayMs ?? INITIAL_RECONNECT_DELAY_MS;
    this.reconnectDelayMs = this.initialReconnectDelayMs;
    this.heartbeatIntervalMs = options.heartbeatIntervalMs ?? HEARTBEAT_INTERVAL_MS;
    this.completedTtlMs = options.completedTtlMs ?? COMPLETED_TTL_MS;
    this.maxCompleted = options.maxCompleted ?? MAX_COMPLETED;
    this.createSocket =
      options.createSocket ??
      ((url, init) => new WebSocket(url, init) as unknown as RelaySocket);
  }

  async start(signal: AbortSignal): Promise<void> {
    this.stopped = false;
    this.abort = signal;
    if (signal.aborted) {
      return;
    }
    const abort = () => {
      void this.stop();
    };
    signal.addEventListener("abort", abort, { once: true });
    try {
      await this.connectLoop(signal);
    } finally {
      signal.removeEventListener("abort", abort);
    }
  }

  async stop(): Promise<void> {
    this.stopped = true;
    for (const [id, pending] of this.pending) {
      clearTimeout(pending.timer);
      pending.reject(new Error(`mammothmail: relay stopped before result ${id}`));
    }
    this.pending.clear();
    const socket = this.socket;
    this.socket = undefined;
    if (socket && socket.readyState < WebSocket.CLOSING) {
      socket.close(1000, "connector stopping");
    }
  }

  /**
   * Sends a reply on a Mammoth Mail thread and resolves with the sent message id.
   * replyTo, when supplied, is the exact inbound message/arrival that started
   * the turn; Mammoth Mail then replies to that email rather than guessing from
   * the conversation id.
   */
  async sendText(threadId: string, text: string, replyTo?: string | null): Promise<string> {
    const socket = this.socket;
    if (!socket || socket.readyState !== WS_OPEN) {
      throw new Error("mammothmail: relay is not connected");
    }
    if (!threadId.trim() || !text.trim()) {
      throw new Error("mammothmail: thread id and text are required");
    }
    const requestId = crypto.randomUUID();
    const result = new Promise<string>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(requestId);
        reject(new Error("mammothmail: outbound result timeout"));
      }, OUTBOUND_RESULT_TIMEOUT_MS);
      timer.unref?.();
      this.pending.set(requestId, { resolve, reject, timer });
    });
    const action: Record<string, unknown> = { op: "send", chat_id: threadId, content: text };
    if (replyTo?.trim()) {
      action.reply_to_message_id = replyTo.trim();
    }
    socket.send(JSON.stringify({ type: "outbound", requestId, action }));
    return result;
  }

  private async connectLoop(signal: AbortSignal): Promise<void> {
    while (!this.stopped && !signal.aborted) {
      try {
        await this.connectOnce(signal);
        this.reconnectDelayMs = this.initialReconnectDelayMs;
      } catch (error) {
        if (this.stopped || signal.aborted) {
          return;
        }
        this.options.log?.warn?.(
          `mammothmail: relay connection failed: ${error instanceof Error ? error.message : String(error)}`,
        );
        this.options.onStatus?.("recovering");
        await sleep(this.reconnectDelayMs, signal);
        this.reconnectDelayMs = Math.min(this.reconnectDelayMs * 2, MAX_RECONNECT_DELAY_MS);
      }
    }
  }

  private connectOnce(signal: AbortSignal): Promise<void> {
    return new Promise((resolve, reject) => {
      const socket = this.createSocket(websocketUrl(this.options.baseUrl), {
        headers: {
          Authorization: `Bearer ${relayBearer(this.options.gatewayId, this.options.secret)}`,
        },
      });
      this.socket = socket;
      let settled = false;
      const finish = (error?: Error) => {
        if (settled) {
          return;
        }
        settled = true;
        if (this.socket === socket) {
          this.socket = undefined;
        }
        this.options.onStatus?.("disconnected");
        if (error) {
          reject(error);
        } else {
          resolve();
        }
      };
      const abort = () => socket.close(1000, "connector stopping");
      signal.addEventListener("abort", abort, { once: true });

      socket.on("open", () => {
        this.options.onStatus?.("connected");
        socket.send(JSON.stringify({ type: "hello", platform: "email", botId: "mammothmail-openclaw" }));
      });

      socket.on("message", (raw) => {
        void this.handleFrame(socket, raw.toString()).catch((error) => {
          socket.close(1011, "frame handling failed");
          finish(error instanceof Error ? error : new Error(String(error)));
        });
      });

      socket.on("close", () => {
        signal.removeEventListener("abort", abort);
        finish();
      });

      socket.on("error", (error) => {
        signal.removeEventListener("abort", abort);
        finish(error instanceof Error ? error : new Error(String(error)));
      });
    });
  }

  private async handleFrame(socket: RelaySocket, raw: string): Promise<void> {
    const frame = JSON.parse(raw) as RelayFrame;
    if (frame.type === "ping") {
      socket.send(JSON.stringify({ type: "pong" }));
      return;
    }
    if (frame.type === "descriptor") {
      return;
    }
    if (frame.type === "outbound_result") {
      const pending = this.pending.get(String(frame.requestId ?? ""));
      if (!pending) {
        return;
      }
      this.pending.delete(String(frame.requestId));
      clearTimeout(pending.timer);
      if (frame.result?.success) {
        pending.resolve(String(frame.result.message_id ?? frame.requestId));
      } else {
        pending.reject(new Error(String(frame.result?.error ?? "Mammoth Mail send failed")));
      }
      return;
    }
    if (frame.type !== "inbound" || !frame.event || !frame.bufferId) {
      return;
    }
    const bufferId = String(frame.bufferId);
    // A completion that happened while disconnected must not be dispatched
    // again: acknowledge the replay on the replacement socket and stop.
    if (this.isCompleted(bufferId)) {
      this.sendFrame({ type: "inbound_ack", bufferId });
      return;
    }
    // A replay while the original handler is still running joins that work
    // instead of starting a second agent turn. The waiter that finishes on a
    // replacement socket acks through the current socket.
    let processing = this.inFlight.get(bufferId);
    if (!processing) {
      processing = this.processInbound(bufferId, frame.event);
      this.inFlight.set(bufferId, processing);
    }
    await processing;
    // The ack is the durable-receipt signal: only send it after the inbound
    // handler has accepted the event for dispatch, so a crash replays it.
    this.sendFrame({ type: "inbound_ack", bufferId });
  }

  /** Runs one agent turn with heartbeats while it is in flight. */
  private async processInbound(bufferId: string, event: MammothMailInboundEvent): Promise<void> {
    const heartbeat = setInterval(() => {
      if (!this.isCompleted(bufferId)) {
        this.sendFrame({ type: "inbound_processing", bufferId });
      }
    }, this.heartbeatIntervalMs);
    heartbeat.unref?.();
    try {
      await this.options.onInbound(event);
      this.markCompleted(bufferId);
    } finally {
      clearInterval(heartbeat);
      this.inFlight.delete(bufferId);
    }
  }

  private isCompleted(bufferId: string): boolean {
    const expires = this.completed.get(bufferId);
    if (expires === undefined) {
      return false;
    }
    if (expires <= Date.now()) {
      this.completed.delete(bufferId);
      return false;
    }
    return true;
  }

  private markCompleted(bufferId: string): void {
    const now = Date.now();
    this.completed.set(bufferId, now + this.completedTtlMs);
    for (const [id, expires] of this.completed) {
      if (expires <= now) {
        this.completed.delete(id);
      }
    }
    while (this.completed.size > this.maxCompleted) {
      const oldest = this.completed.keys().next();
      if (oldest.done) {
        break;
      }
      this.completed.delete(oldest.value);
    }
  }

  private sendFrame(frame: Record<string, unknown>): void {
    const socket = this.socket;
    if (!socket || socket.readyState !== WS_OPEN) {
      return;
    }
    try {
      socket.send(JSON.stringify(frame));
    } catch {
      // A socket that failed between the state check and the write is handled
      // by its close/error event; the durable cursor keeps the event replayable.
    }
  }
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(resolve, ms);
    timer.unref?.();
    signal.addEventListener(
      "abort",
      () => {
        clearTimeout(timer);
        resolve();
      },
      { once: true },
    );
  });
}
