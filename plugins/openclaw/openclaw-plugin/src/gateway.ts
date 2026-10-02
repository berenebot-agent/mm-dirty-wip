/**
 * Per-account gateway for the MailMoose channel.
 *
 * OpenClaw calls startAccount when the account is enabled and configured, and
 * aborts the signal on reload/stop. The relay dials out and stays up for the
 * lifetime of the account, mirroring the bundled ClickClack gateway shape.
 */
import type { ChannelGatewayContext } from "openclaw/plugin-sdk/channel-contract";
import type { PluginRuntime } from "openclaw/plugin-sdk/channel-core";
import { channelReadyPatch, channelStoppedPatch } from "openclaw/plugin-sdk/gateway-runtime";
import { formatErrorMessage } from "openclaw/plugin-sdk/error-runtime";
import { resolveMailMooseAccount } from "./accounts.js";
import { dispatchMailMooseInbound } from "./inbound.js";
import { MailMooseRelay } from "./relay.js";
import { setRelay } from "./runtime.js";
import type { CoreConfig, ResolvedMailMooseAccount } from "./types.js";

export async function startMailMooseAccount(
  ctx: ChannelGatewayContext<ResolvedMailMooseAccount>,
): Promise<void> {
  const account = resolveMailMooseAccount({
    cfg: ctx.cfg as CoreConfig,
    accountId: ctx.account.accountId,
  });
  if (!account.configured) {
    throw new Error(`MailMoose is not configured for account "${account.accountId}"`);
  }
  const log = {
    info: (message: string) => ctx.log?.info?.(message),
    warn: (message: string) => ctx.log?.warn?.(message),
  };
  const relay = new MailMooseRelay({
    baseUrl: account.baseUrl,
    gatewayId: account.gatewayId,
    secret: account.secret,
    log,
    onStatus: (status) => {
      if (status === "connected") {
        ctx.setStatus(channelReadyPatch({ accountId: account.accountId }));
      } else if (status === "recovering") {
        ctx.setStatus({
          accountId: account.accountId,
          connected: false,
          lifecycle: "recovering",
        });
      }
    },
    onInbound: async (event) => {
      await dispatchMailMooseInbound({
        account,
        cfg: ctx.cfg as CoreConfig,
        event,
        abortSignal: ctx.abortSignal,
        runtime: ctx.channelRuntime as PluginRuntime["channel"] | undefined,
        log,
      });
    },
  });
  setRelay(account.accountId, relay);
  ctx.setStatus({
    accountId: account.accountId,
    running: true,
    lifecycle: "starting",
    configured: true,
    enabled: account.enabled,
    baseUrl: account.baseUrl,
  });
  try {
    await relay.start(ctx.abortSignal);
  } catch (error) {
    log.warn(`mailmoose: relay stopped: ${formatErrorMessage(error)}`);
  } finally {
    setRelay(account.accountId, undefined);
    if (!ctx.abortSignal.aborted) {
      ctx.setStatus(channelStoppedPatch({ accountId: account.accountId }));
    }
  }
}

export async function stopMailMooseAccount(
  ctx: ChannelGatewayContext<ResolvedMailMooseAccount>,
): Promise<void> {
  const account = resolveMailMooseAccount({
    cfg: ctx.cfg as CoreConfig,
    accountId: ctx.account.accountId,
  });
  setRelay(account.accountId, undefined);
}
