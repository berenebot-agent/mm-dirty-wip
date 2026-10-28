/**
 * Mammoth Mail channel plugin object.
 *
 * Mammoth Mail is a dial-out email channel: the gateway runs one relay socket per
 * account, dispatches inbound mail through the shared turn kernel, and sends
 * replies back over the same socket. The channel declares a thread-first
 * messaging target (`thread:<id>`), matching one Mammoth Mail email thread to one
 * OpenClaw conversation.
 */
import {
  buildChannelOutboundSessionRoute,
  createChatChannelPlugin,
  type ChannelPlugin,
} from "openclaw/plugin-sdk/channel-core";
import type { ChannelMeta } from "openclaw/plugin-sdk/channel-contract";
import {
  createComputedAccountStatusAdapter,
  createDefaultChannelRuntimeState,
} from "openclaw/plugin-sdk/status-helpers";
import {
  DEFAULT_ACCOUNT_ID,
  listAccountIds,
  resolveDefaultAccountId,
  resolveMammothMailAccount,
} from "./accounts.js";
import { startMammothMailAccount, stopMammothMailAccount } from "./gateway.js";
import { sendMammothMailText } from "./outbound.js";
import { mammothMailSetupContract } from "./setup.js";
import {
  buildMammothMailTarget,
  looksLikeMammothMailTarget,
  normalizeMammothMailTarget,
  parseMammothMailTarget,
} from "./target.js";
import type { CoreConfig, ResolvedMammothMailAccount } from "./types.js";

export const MAIL_CHANNEL_ID = "mammothmail" as const;

// External channels define their own metadata; getChatChannelMeta only knows
// about bundled channel ids.
export const mammothMailMeta: ChannelMeta = {
  id: MAIL_CHANNEL_ID,
  label: "Mammoth Mail",
  selectionLabel: "Mammoth Mail (email)",
  docsPath: "/channels/mammothmail",
  blurb: "Give your agent its own email inbox through Mammoth Mail.",
  detailLabel: "Mammoth Mail inbox",
  systemImage: "envelope",
  markdownCapable: false,
  order: 60,
};

export const mammothMailConfigAdapter = {
  listAccountIds: (cfg: CoreConfig) => listAccountIds(cfg as never),
  resolveAccount: (cfg: CoreConfig, accountId?: string | null) =>
    resolveMammothMailAccount({ cfg, accountId }),
  defaultAccountId: (cfg: CoreConfig) => resolveDefaultAccountId(cfg as never),
  isConfigured: (account: ResolvedMammothMailAccount) => account.configured,
  resolveAllowFrom: ({ cfg, accountId }: { cfg: CoreConfig; accountId?: string | null }) =>
    resolveMammothMailAccount({ cfg, accountId }).allowFrom,
} satisfies NonNullable<ChannelPlugin<ResolvedMammothMailAccount>["config"]>;

export const mammothMailPlugin: ChannelPlugin<ResolvedMammothMailAccount> = createChatChannelPlugin({
  base: {
    id: MAIL_CHANNEL_ID,
    meta: mammothMailMeta,
    capabilities: {
      chatTypes: ["direct", "channel"],
      threads: false,
      media: false,
      blockStreaming: false,
    },
    reload: { configPrefixes: ["channels.mammothmail"] },
    config: mammothMailConfigAdapter,
    setupContract: mammothMailSetupContract,
    gateway: {
      startAccount: startMammothMailAccount,
      stopAccount: stopMammothMailAccount,
    },
    status: createComputedAccountStatusAdapter<ResolvedMammothMailAccount>({
      defaultRuntime: createDefaultChannelRuntimeState(DEFAULT_ACCOUNT_ID),
      buildChannelSummary: ({ snapshot }) => ({
        ok: snapshot.configured,
        label: snapshot.configured ? "configured" : "missing config",
        detail: snapshot.baseUrl ?? "",
      }),
      resolveAccountSnapshot: ({ account }) => ({
        accountId: account.accountId,
        name: account.name,
        enabled: account.enabled,
        configured: account.configured,
        extra: {
          baseUrl: account.baseUrl,
          gatewayId: account.gatewayId,
          tokenStatus: account.tokenStatus,
        },
      }),
    }),
    messaging: {
      targetPrefixes: ["mammothmail", "mm"],
      normalizeTarget: normalizeMammothMailTarget,
      inferTargetChatType: () => "channel",
      targetResolver: {
        looksLikeId: looksLikeMammothMailTarget,
        hint: "<thread:<thread_id>>",
      },
      resolveOutboundSessionRoute: ({ cfg, agentId, accountId, target, replyToId, threadId }) => {
        const parsed = parseMammothMailTarget(target);
        return buildChannelOutboundSessionRoute({
          cfg,
          agentId,
          channel: MAIL_CHANNEL_ID,
          accountId,
          recipientSessionExact: false,
          peer: { kind: "channel", id: buildMammothMailTarget(parsed) },
          chatType: "channel",
          from: `mammothmail:${accountId ?? DEFAULT_ACCOUNT_ID}`,
          to: buildMammothMailTarget(parsed),
          threadId: threadId ?? parsed.id,
        });
      },
      resolveSessionConversation: ({ rawId }) => {
        const parsed = parseMammothMailTarget(rawId);
        return {
          id: parsed.id,
          baseConversationId: parsed.id,
          parentConversationCandidates: [parsed.id],
        };
      },
    },
  },
  threading: { topLevelReplyToMode: "reply" },
  outbound: {
    base: { deliveryMode: "direct" },
    attachedResults: {
      channel: MAIL_CHANNEL_ID,
      sendText: async (ctx) => {
        const messageId = await sendMammothMailText({
          cfg: ctx.cfg,
          accountId: ctx.accountId,
          to: String(ctx.to ?? ""),
          text: String(ctx.text ?? ""),
          replyTo: ctx.replyToId ?? undefined,
        });
        return { messageId: messageId ?? "" };
      },
    },
  },
});