import crypto from "node:crypto";
import WebSocket from "ws";

export type MailMooseInboundEvent = {
  text: string;
  message_type?: string;
  user_id: string;
  user_name?: string;
  message_id: string;
  source: {
    platform: "email" | string;
    chat_id: string;
    chat_type?: string;
    chat_name?: string;
    user_id?: string;
    user_name?: string;
    thread_id?: string;
    message_id?: string;
  };
  metadata?: Record<string, unknown>;
  provenance?: Record<string, unknown>;
  timestamp?: string;
};

type RelayOptions = {
  baseUrl: string;
  gatewayId: string;
  secret: string;
  onInbound: (event: MailMooseInboundEvent) => Promise<void>;
};

type PendingResult = {
  resolve: (messageId: string) => void;
  reject: (error: Error) => void;
  timer: ReturnType<typeof setTimeout>;
};

function websocketUrl(baseUrl: string): string {
  const url = new URL(baseUrl);
  url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
  url.pathname = "/relay";
  url.search = "";
  url.hash = "";
  return url.toString();
}

export function relayBearer(gatewayId: string, secret: string, now = Date.now()): string {
  const exp = Math.floor(now / 1000) + 5 * 60;
  const payload = `${gatewayId}:${exp}`;
  const signature = crypto.createHmac("sha256", secret).update(payload).digest("hex");
  return Buffer.from(`${payload}:${signature}`, "utf8").toString("base64url");
}

export class MailMooseRelay {
  private socket?: WebSocket;
  private stopped = false;
  private reconnectDelayMs = 1000;
  private pending = new Map<string, PendingResult>();

  constructor(private readonly options: RelayOptions) {}

  async start(signal?: AbortSignal): Promise<void> {
    this.stopped = false;
    if (signal?.aborted) {
      return;
    }

    const abort = () => {
      void this.stop();
    };
    signal?.addEventListener("abort", abort, { once: true });

    try {
      await this.connectLoop(signal);
    } finally {
      signal?.removeEventListener("abort", abort);
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
      socket.close(1000, "plugin stopping");
    }
  }

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
      }, 60_000);
      this.pending.set(requestId, { resolve, reject, timer });
    });

    socket.send(JSON.stringify({
      type: "outbound",
      requestId,
      action: {
        op: "send",
        chat_id: threadId,
        content: text,
      },
    }));

    return result;
  }

  private async connectLoop(signal?: AbortSignal): Promise<void> {
    while (!this.stopped && !signal?.aborted) {
      try {
        await this.connectOnce(signal);
        this.reconnectDelayMs = 1000;
      } catch (error) {
        if (this.stopped || signal?.aborted) {
          return;
        }
        await new Promise((resolve) => setTimeout(resolve, this.reconnectDelayMs));
        this.reconnectDelayMs = Math.min(this.reconnectDelayMs * 2, 30_000);
      }
    }
  }

  private connectOnce(signal?: AbortSignal): Promise<void> {
    return new Promise((resolve, reject) => {
      const socket = new WebSocket(websocketUrl(this.options.baseUrl), {
        headers: {
          Authorization: `Bearer ${relayBearer(this.options.gatewayId, this.options.secret)}`,
        },
      });
      this.socket = socket;

      let settled = false;
      const finish = (error?: Error) => {
        if (settled) return;
        settled = true;
        if (this.socket === socket) this.socket = undefined;
        error ? reject(error) : resolve();
      };

      const abort = () => socket.close(1000, "gateway stopping");
      signal?.addEventListener("abort", abort, { once: true });

      socket.on("open", () => {
        socket.send(JSON.stringify({
          type: "hello",
          platform: "email",
          botId: "openclaw-mailmoose",
        }));
      });

      socket.on("message", (raw) => {
        void this.handleFrame(socket, raw.toString()).catch((error) => {
          socket.close(1011, "frame handling failed");
          finish(error instanceof Error ? error : new Error(String(error)));
        });
      });

      socket.on("close", () => {
        signal?.removeEventListener("abort", abort);
        finish();
      });

      socket.on("error", (error) => {
        signal?.removeEventListener("abort", abort);
        finish(error instanceof Error ? error : new Error(String(error)));
      });
    });
  }

  private async handleFrame(socket: WebSocket, raw: string): Promise<void> {
    const frame = JSON.parse(raw) as any;

    if (frame.type === "ping") {
      socket.send(JSON.stringify({ type: "pong" }));
      return;
    }

    if (frame.type === "descriptor") {
      return;
    }

    if (frame.type === "outbound_result") {
      const pending = this.pending.get(String(frame.requestId ?? ""));
      if (!pending) return;

      this.pending.delete(String(frame.requestId));
      clearTimeout(pending.timer);
      if (frame.result?.success) {
        pending.resolve(String(frame.result?.message_id ?? frame.requestId));
      } else {
        pending.reject(new Error(String(frame.result?.error ?? "MailMoose send failed")));
      }
      return;
    }

    if (frame.type !== "inbound" || !frame.event || !frame.bufferId) {
      return;
    }

    await this.options.onInbound(frame.event as MailMooseInboundEvent);
    socket.send(JSON.stringify({
      type: "inbound_ack",
      bufferId: String(frame.bufferId),
    }));
  }
}
