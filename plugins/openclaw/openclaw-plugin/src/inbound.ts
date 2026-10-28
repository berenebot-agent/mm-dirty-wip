/**
 * Inbound dispatch for Mammoth Mail email.
 *
 * Mammoth Mail already authenticated the socket and normalized the message. This
 * module turns one event into an OpenClaw inbound turn using the same runtime
 * kernel as bundled channels: scoped ingress policy, session recording and
 * reply delivery stay in core.
 *
 * Provenance rule: email From is caller-asserted, never an authenticated
 * OpenClaw identity. The event is dispatched as untrusted external input.
 * Mammoth Mail's inbox allow list is the primary sender gate; the optional
 * OpenClaw-side allowFrom is a second, narrower gate.
 */
import type { PluginRuntime } from "openclaw/plugin-sdk/channel-core";
import type { MammothMailInboundEvent, CoreConfig, ResolvedMammothMailAccount } from "./types.js";
import { sendMammothMailText } from "./outbound.js";

const CHANNEL_ID = "mammothmail";

type ChannelRuntime = PluginRuntime["channel"];

export type DispatchMammothMailInboundParams = {
  account: ResolvedMammothMailAccount;
  cfg: CoreConfig;
  event: MammothMailInboundEvent;
  abortSignal: AbortSignal;
  runtime?: ChannelRuntime;
  log?: { info?: (message: string) => void; warn?: (message: string) => void };
};

function normalizeEmail(value: string): string | null {
  const trimmed = value.trim().toLowerCase();
  return trimmed || null;
}

const mammothMailIngressIdentity = {
  key: "email",
  normalizeEntry: normalizeEmail,
  normalizeSubject: normalizeEmail,
  isWildcardEntry: (entry: string) => entry.trim() === "*",
  entryIdPrefix: "mammothmail-email",
};

/** Dispatches one Mammoth Mail email event to the agent and delivers its reply. */
export async function dispatchMammothMailInbound(params: DispatchMammothMailInboundParams): Promise<void> {
  const { account, cfg, event } = params;
  if (params.abortSignal.aborted) {
    return;
  }
  const runtime = params.runtime;
  if (!runtime?.inbound?.dispatch || !runtime.inbound.buildContext) {
    throw new Error("mammothmail: OpenClaw inbound runtime is unavailable");
  }
  const threadId = String(event.source?.thread_id || event.source?.chat_id || "");
  const messageId = String(event.message_id || "");
  if (!threadId || !messageId) {
    throw new Error("mammothmail: inbound event is missing thread or message id");
  }
  const sender = event.user_id || event.source?.user_id || "unknown@email";
  const senderName = event.user_name || event.source?.user_name || sender;
  const target = `thread:${threadId}`;
  const timestamp = event.timestamp ? Date.parse(event.timestamp) : Date.now();

  const route = runtime.routing.resolveAgentRoute({
    cfg,
    channel: CHANNEL_ID,
    accountId: account.accountId,
    peer: { kind: "channel", id: target },
  });

  const allowFrom = account.allowFrom.length > 0 ? account.allowFrom : ["*"];
  const ingress = await runtime.inbound.ingress.resolveStable({
    channelId: CHANNEL_ID,
    accountId: account.accountId,
    identity: mammothMailIngressIdentity,
    cfg: cfg as never,
    subject: { stableId: sender },
    conversation: { kind: "direct", id: threadId },
    contextBinding: {
      agentId: route.agentId,
      sessionKey: route.sessionKey,
      nativeChannelId: threadId,
      messageId,
      inboundEventKind: "user_request",
    },
    allowFrom,
    dmPolicy: account.dmPolicy as "allowlist" | "open" | "pairing",
    groupPolicy: "allowlist",
    mentionFacts: { canDetectMention: false, wasMentioned: false },
    policy: { activation: { requireMention: false, allowTextCommands: true } },
    command: false,
  });
  if (ingress.ingress.decision !== "allow" || ingress.activationAccess.shouldSkip) {
    params.log?.info?.(
      `mammothmail: dropped inbound reason=${ingress.ingress.reasonCode} from=${sender}`,
    );
    return;
  }

  const senderLabel = senderName === sender ? sender : `${senderName} <${sender}>`;
  const body = String(event.text ?? "");
  const ctxPayload = runtime.inbound.buildContext({
    channelIngress: ingress,
    channel: CHANNEL_ID,
    accountId: account.accountId,
    messageId,
    messageIdFull: messageId,
    timestamp,
    from: senderLabel,
    sender: { id: sender, name: senderName },
    conversation: { kind: "direct", id: threadId, nativeChannelId: threadId },
    route: {
      agentId: route.agentId,
      dmScope: route.dmScope,
      accountId: route.accountId,
      routeSessionKey: route.sessionKey,
    },
    reply: { to: target, originatingTo: target, replyToId: messageId },
    message: { body, bodyForAgent: body, rawBody: body, commandBody: body },
    access: { commands: { authorized: false } },
  });

  await runtime.inbound.dispatch({
    cfg,
    channel: CHANNEL_ID,
    accountId: account.accountId,
    route: { agentId: route.agentId, dmScope: route.dmScope, sessionKey: route.sessionKey },
    ctxPayload,
    delivery: {
      deliver: async (payload: { text?: string | null }) => {
        const text = payload.text ?? "";
        if (!text.trim()) {
          return;
        }
        await sendMammothMailText({
          cfg,
          accountId: account.accountId,
          to: target,
          text,
          replyTo: messageId,
        });
      },
      durable: () => false,
    },
  });
  params.log?.info?.(`mammothmail: inbound dispatched thread=${threadId} from=${sender}`);
}
