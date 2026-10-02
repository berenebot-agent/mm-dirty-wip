/**
 * MailMoose channel plugin object.
 *
 * MailMoose is a dial-out email channel: the gateway runs one relay socket per
 * account, dispatches inbound mail through the shared turn kernel, and sends
 * replies back over the same socket. The channel declares a thread-first
 * messaging target (`thread:<id>`), matching one MailMoose email thread to one
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
  resolveMailMooseAccount,
} from "./accounts.js";
import { startMailMooseAccount, stopMailMooseAccount } from "./gateway.js";
import { sendMailMooseText } from "./outbound.js";
import { mailMooseSetupContract } from "./setup.js";
import {
  buildMailMooseTarget,
  looksLikeMailMooseTarget,
  normalizeMailMooseTarget,
  parseMailMooseTarget,
} from "./target.js";
import type { CoreConfig, ResolvedMailMooseAccount } from "./types.js";

export const MAILMOOSE_CHANNEL_ID = "mailmoose" as const;

// External channels define their own metadata; getChatChannelMeta only knows
// about bundled channel ids.
export const mailMooseMeta: ChannelMeta = {
  id: MAILMOOSE_CHANNEL_ID,
  label: "MailMoose",
  selectionLabel: "MailMoose (email)",
  docsPath: "/channels/mailmoose",
  blurb: "Give your agent its own email inbox through MailMoose.",
  detailLabel: "MailMoose inbox",
  systemImage: "envelope",
  markdownCapable: false,
  order: 60,
};

export const mailMooseConfigAdapter = {
  listAccountIds: (cfg: CoreConfig) => listAccountIds(cfg as never),
  resolveAccount: (cfg: CoreConfig, accountId?: string | null) =>
    resolveMailMooseAccount({ cfg, accountId }),
  defaultAccountId: (cfg: CoreConfig) => resolveDefaultAccountId(cfg as never),
  isConfigured: (account: ResolvedMailMooseAccount) => account.configured,
  resolveAllowFrom: ({ cfg, accountId }: { cfg: CoreConfig; accountId?: string | null }) =>
    resolveMailMooseAccount({ cfg, accountId }).allowFrom,
} satisfies NonNullable<ChannelPlugin<ResolvedMailMooseAccount>["config"]>;

export const mailMoosePlugin: ChannelPlugin<ResolvedMailMooseAccount> = createChatChannelPlugin({
  base: {
    id: MAILMOOSE_CHANNEL_ID,
    meta: mailMooseMeta,
    capabilities: {
      chatTypes: ["direct", "channel"],
      threads: false,
      media: false,
      blockStreaming: false,
    },
    reload: { configPrefixes: ["channels.mailmoose"] },
    config: mailMooseConfigAdapter,
    setupContract: mailMooseSetupContract,
    gateway: {
      startAccount: startMailMooseAccount,
      stopAccount: stopMailMooseAccount,
    },
    status: createComputedAccountStatusAdapter<ResolvedMailMooseAccount>({
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
      targetPrefixes: ["mailmoose", "mm"],
      normalizeTarget: normalizeMailMooseTarget,
      inferTargetChatType: () => "channel",
      targetResolver: {
        looksLikeId: looksLikeMailMooseTarget,
        hint: "<thread:<thread_id>>",
      },
      resolveOutboundSessionRoute: ({ cfg, agentId, accountId, target, replyToId, threadId }) => {
        const parsed = parseMailMooseTarget(target);
        return buildChannelOutboundSessionRoute({
          cfg,
          agentId,
          channel: MAILMOOSE_CHANNEL_ID,
          accountId,
          recipientSessionExact: false,
          peer: { kind: "channel", id: buildMailMooseTarget(parsed) },
          chatType: "channel",
          from: `mailmoose:${accountId ?? DEFAULT_ACCOUNT_ID}`,
          to: buildMailMooseTarget(parsed),
          threadId: threadId ?? parsed.id,
        });
      },
      resolveSessionConversation: ({ rawId }) => {
        const parsed = parseMailMooseTarget(rawId);
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
      channel: MAILMOOSE_CHANNEL_ID,
      sendText: async (ctx) => {
        const messageId = await sendMailMooseText({
          cfg: ctx.cfg,
          accountId: ctx.accountId,
          to: String(ctx.to ?? ""),
          text: String(ctx.text ?? ""),
        });
        return { messageId: messageId ?? "" };
      },
    },
  },
});