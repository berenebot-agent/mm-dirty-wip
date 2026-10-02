/**
 * MailMoose relay client.
 *
 * Dial-out WebSocket transport shared with the Hermes connector:
 *   hello -> descriptor -> inbound/outbound frames -> inbound_ack.
 *
 * The client never exposes an inbound port; MailMoose pushes mail down the
 * socket this process opened. The durable cursor lives on MailMoose: an event
 * is acked only after the inbound handler resolves.
 */
import crypto from "node:crypto";
import WebSocket from "ws";
import type { MailMooseInboundEvent, RelayFrame } from "./types.js";

export type MailMooseRelayOptions = {
  baseUrl: string;
  gatewayId: string;
  secret: string;
  onInbound: (event: MailMooseInboundEvent) => Promise<void>;
  onStatus?: (status: "connected" | "disconnected" | "recovering") => void;
  log?: {
    info?: (message: string) => void;
    warn?: (message: string) => void;
  };
};

type PendingResult = {
  resolve: (messageId: string) => void;
  reject: (error: Error) => void;
  timer: ReturnType<typeof setTimeout>;
};

const OUTBOUND_RESULT_TIMEOUT_MS = 60_000;
const MAX_RECONNECT_DELAY_MS = 30_000;
const INITIAL_RECONNECT_DELAY_MS = 1_000;

/** Builds the relay WebSocket URL for a MailMoose base URL. */
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

export class MailMooseRelay {
  private socket?: WebSocket;
  private stopped = false;
  private reconnectDelayMs = INITIAL_RECONNECT_DELAY_MS;
  private pending = new Map<string, PendingResult>();
  private abort?: AbortSignal;

  constructor(private readonly options: MailMooseRelayOptions) {}

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
      pending.reject(new Error(`mailmoose: relay stopped before result ${id}`));
    }
    this.pending.clear();
    const socket = this.socket;
    this.socket = undefined;
    if (socket && socket.readyState < WebSocket.CLOSING) {
      socket.close(1000, "connector stopping");
    }
  }

  /** Sends a reply on a MailMoose thread and resolves with the sent message id. */
  async sendText(threadId: string, text: string): Promise<string> {
    const socket = this.socket;
    if (!socket || socket.readyState !== WebSocket.OPEN) {
      throw new Error("mailmoose: relay is not connected");
    }
    if (!threadId.trim() || !text.trim()) {
      throw new Error("mailmoose: thread id and text are required");
    }
    const requestId = crypto.randomUUID();
    const result = new Promise<string>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(requestId);
        reject(new Error("mailmoose: outbound result timeout"));
      }, OUTBOUND_RESULT_TIMEOUT_MS);
      timer.unref?.();
      this.pending.set(requestId, { resolve, reject, timer });
    });
    socket.send(
      JSON.stringify({
        type: "outbound",
        requestId,
        action: { op: "send", chat_id: threadId, content: text },
      }),
    );
    return result;
  }

  private async connectLoop(signal: AbortSignal): Promise<void> {
    while (!this.stopped && !signal.aborted) {
      try {
        await this.connectOnce(signal);
        this.reconnectDelayMs = INITIAL_RECONNECT_DELAY_MS;
      } catch (error) {
        if (this.stopped || signal.aborted) {
          return;
        }
        this.options.log?.warn?.(
          `mailmoose: relay connection failed: ${error instanceof Error ? error.message : String(error)}`,
        );
        this.options.onStatus?.("recovering");
        await sleep(this.reconnectDelayMs, signal);
        this.reconnectDelayMs = Math.min(this.reconnectDelayMs * 2, MAX_RECONNECT_DELAY_MS);
      }
    }
  }

  private connectOnce(signal: AbortSignal): Promise<void> {
    return new Promise((resolve, reject) => {
      const socket = new WebSocket(websocketUrl(this.options.baseUrl), {
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
        socket.send(JSON.stringify({ type: "hello", platform: "email", botId: "mailmoose-openclaw" }));
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

  private async handleFrame(socket: WebSocket, raw: string): Promise<void> {
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
        pending.reject(new Error(String(frame.result?.error ?? "MailMoose send failed")));
      }
      return;
    }
    if (frame.type !== "inbound" || !frame.event || !frame.bufferId) {
      return;
    }
    // The ack is the durable-receipt signal: only send it after the inbound
    // handler has accepted the event for dispatch, so a crash replays it.
    await this.options.onInbound(frame.event);
    socket.send(JSON.stringify({ type: "inbound_ack", bufferId: String(frame.bufferId) }));
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
