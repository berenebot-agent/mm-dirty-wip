/**
 * MailMoose outbound send path.
 *
 * Replies travel back over the same authenticated relay socket the gateway
 * opened, addressed by the MailMoose thread id (`thread:<id>` target).
 */
import type { OpenClawConfig } from "openclaw/plugin-sdk/channel-core";
import { resolveMailMooseAccount } from "./accounts.js";
import { getRelay } from "./runtime.js";
import { threadIdFromTarget } from "./target.js";
import type { CoreConfig } from "./types.js";

export async function sendMailMooseText(params: {
  cfg: OpenClawConfig;
  accountId?: string | null;
  to: string;
  text: string;
}): Promise<string> {
  const account = resolveMailMooseAccount({
    cfg: params.cfg as CoreConfig,
    accountId: params.accountId,
  });
  const relay = getRelay(account.accountId);
  if (!relay) {
    throw new Error("mailmoose: relay is not connected for this account");
  }
  const threadId = threadIdFromTarget(params.to);
  return relay.sendText(threadId, params.text);
}
