/**
 * Mammoth Mail outbound send path.
 *
 * Replies travel back over the same authenticated relay socket the gateway
 * opened, addressed by the Mammoth Mail thread id (`thread:<id>` target).
 */
import type { OpenClawConfig } from "openclaw/plugin-sdk/channel-core";
import { resolveMammothMailAccount } from "./accounts.js";
import { getRelay } from "./runtime.js";
import { threadIdFromTarget } from "./target.js";
import type { CoreConfig } from "./types.js";

export async function sendMammothMailText(params: {
  cfg: OpenClawConfig;
  accountId?: string | null;
  to: string;
  text: string;
  /** Exact inbound mail this reply answers (local message or remote arrival id). */
  replyTo?: string | null;
}): Promise<string> {
  const account = resolveMammothMailAccount({
    cfg: params.cfg as CoreConfig,
    accountId: params.accountId,
  });
  const relay = getRelay(account.accountId);
  if (!relay) {
    throw new Error("mammothmail: relay is not connected for this account");
  }
  const threadId = threadIdFromTarget(params.to);
  return relay.sendText(threadId, params.text, params.replyTo);
}
