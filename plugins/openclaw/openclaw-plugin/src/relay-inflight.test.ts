import test from "node:test";
import assert from "node:assert/strict";
import { MailMooseRelay, type RelaySocket } from "../src/relay.js";
import type { MailMooseInboundEvent } from "../src/types.js";

class FakeSocket implements RelaySocket {
  readyState = 0;
  sent: string[] = [];
  private handlers = new Map<string, Array<(...args: any[]) => void>>();

  on(event: "open" | "message" | "close" | "error", listener: (...args: any[]) => void): unknown {
    const list = this.handlers.get(event) ?? [];
    list.push(listener);
    this.handlers.set(event, list);
    return this;
  }

  send(data: string): void {
    if (this.readyState !== 1) {
      throw new Error("socket is not open");
    }
    this.sent.push(data);
  }

  close(code?: number, reason?: string): void {
    void code;
    void reason;
    if (this.readyState === 3) {
      return;
    }
    this.readyState = 3;
    this.emit("close");
  }

  open(): void {
    this.readyState = 1;
    this.emit("open");
  }

  serverClose(): void {
    this.readyState = 3;
    this.emit("close");
  }

  receive(frame: unknown): void {
    this.emit("message", Buffer.from(JSON.stringify(frame)));
  }

  frames(): Array<Record<string, any>> {
    return this.sent.map((value) => JSON.parse(value) as Record<string, any>);
  }

  private emit(event: string, ...args: any[]): void {
    for (const listener of this.handlers.get(event) ?? []) {
      listener(...args);
    }
  }
}

type Fixture = {
  sockets: FakeSocket[];
  abort: AbortController;
  relay: MailMooseRelay;
};

function fixture(onInbound: (event: MailMooseInboundEvent) => Promise<void>): Fixture {
  const sockets: FakeSocket[] = [];
  const relay = new MailMooseRelay({
    baseUrl: "http://mail.example.test",
    gatewayId: "gw-inflight",
    secret: "secret",
    onInbound,
    reconnectDelayMs: 5,
    heartbeatIntervalMs: 5,
    completedTtlMs: 60_000,
    createSocket: () => {
      const socket = new FakeSocket();
      sockets.push(socket);
      return socket;
    },
  });
  const abort = new AbortController();
  void relay.start(abort.signal);
  return { sockets, abort, relay };
}

function event(text: string): MailMooseInboundEvent {
  return {
    text,
    message_type: "text",
    user_id: "alice@outside.test",
    message_id: "rar_1",
    source: { platform: "email", chat_id: "<one@test>", thread_id: "<one@test>" },
  };
}

function inbound(bufferId: string, text: string): Record<string, unknown> {
  return { type: "inbound", bufferId, event: event(text) };
}

async function waitFor(check: () => boolean, timeoutMs = 1000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (check()) {
      return;
    }
    await new Promise((resolve) => setTimeout(resolve, 2));
  }
  throw new Error("condition not met before timeout");
}

async function stop(fx: Fixture): Promise<void> {
  fx.abort.abort();
  await fx.relay.stop();
}

test("relay joins a replayed buffer and heartbeats while processing", async () => {
  let resolveTurn: (() => void) | undefined;
  let dispatches = 0;
  const fx = fixture(
    () =>
      new Promise((resolve) => {
        dispatches += 1;
        resolveTurn = resolve;
      }),
  );
  try {
    await waitFor(() => fx.sockets.length === 1);
    const socket = fx.sockets[0];
    socket.open();
    socket.receive(inbound("evt_1", "first"));
    socket.receive(inbound("evt_1", "replayed"));
    await waitFor(() => socket.frames().some((frame) => frame.type === "inbound_processing"));
    assert.equal(dispatches, 1, "a replayed buffer must join the in-flight turn");
    assert.equal(
      socket.frames().filter((frame) => frame.type === "inbound_ack").length,
      0,
      "an event must not be acknowledged before processing completes",
    );
    resolveTurn?.();
    await waitFor(() => socket.frames().some((frame) => frame.type === "inbound_ack"));
    assert.equal(dispatches, 1);
  } finally {
    await stop(fx);
  }
});

test("relay records completion while disconnected and acks the replay", async () => {
  let resolveTurn: (() => void) | undefined;
  let dispatches = 0;
  const fx = fixture(
    () =>
      new Promise((resolve) => {
        dispatches += 1;
        resolveTurn = resolve;
      }),
  );
  try {
    await waitFor(() => fx.sockets.length === 1);
    fx.sockets[0].open();
    fx.sockets[0].receive(inbound("evt_2", "first"));
    await waitFor(() => dispatches === 1);
    fx.sockets[0].serverClose();
    await waitFor(() => fx.sockets.length === 2);
    // Processing finishes while the replacement socket is not yet open.
    resolveTurn?.();
    await new Promise((resolve) => setTimeout(resolve, 10));
    const replacement = fx.sockets[1];
    replacement.open();
    replacement.receive(inbound("evt_2", "replayed after reconnect"));
    await waitFor(() => replacement.frames().some((frame) => frame.type === "inbound_ack"));
    assert.equal(dispatches, 1, "a completion before reconnect must not dispatch again");
  } finally {
    await stop(fx);
  }
});

test("relay replays a buffer after a handler failure", async () => {
  let dispatches = 0;
  const fx = fixture(async () => {
    dispatches += 1;
    if (dispatches === 1) {
      throw new Error("turn failed");
    }
  });
  try {
    await waitFor(() => fx.sockets.length === 1);
    fx.sockets[0].open();
    fx.sockets[0].receive(inbound("evt_3", "first"));
    await waitFor(() => fx.sockets[0].readyState === 3);
    await waitFor(() => fx.sockets.length === 2);
    const replacement = fx.sockets[1];
    replacement.open();
    replacement.receive(inbound("evt_3", "replayed"));
    await waitFor(() => replacement.frames().some((frame) => frame.type === "inbound_ack"));
    assert.equal(dispatches, 2, "a failed turn must be replayed");
  } finally {
    await stop(fx);
  }
});
