import type { MailMooseInboundEvent } from "./relay.js";

/**
 * Single OpenClaw-SDK-specific seam.
 *
 * The relay transport deliberately does not know how OpenClaw chooses an agent,
 * builds a session key, records the inbound message, or dispatches a reply.
 * Wire that here using the supported api.runtime.channel.inbound API for the
 * pinned OpenClaw release.
 *
 * Keep the MailMoose thread id as the conversation/thread target and preserve
 * the provenance supplied by MailMoose. In particular, email From must remain
 * external/untrusted rather than becoming an authenticated OpenClaw identity.
 */
export async function dispatchInbound(runtime: any, event: MailMooseInboundEvent): Promise<void> {
  const inbound = runtime?.channel?.inbound;
  if (!inbound?.run) {
    throw new Error("mailmoose: OpenClaw inbound runtime is unavailable");
  }

  // TODO before publishing:
  // Replace this intentionally narrow WIP guard with the pinned-version
  // runtime.channel.inbound.run(...) adapter. The transport will ACK only after
  // this function resolves, so a dispatch failure remains replayable.
  //
  // Required normalized facts:
  //   channel: "mailmoose"
  //   accountId: "default"
  //   text: event.text
  //   sender id/name: event.user_id / event.user_name
  //   conversation target: event.source.chat_id
  //   thread id: event.source.thread_id
  //   provider message id: event.message_id
  //   timestamp: event.timestamp
  //   provenance: external_untrusted / unauthenticated sender
  void event;
  throw new Error("mailmoose: inbound adapter not wired yet; see openclaw-wip/README.md");
}
