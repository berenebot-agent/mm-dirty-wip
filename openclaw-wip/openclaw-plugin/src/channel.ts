import {
  createChatChannelPlugin,
  createChannelPluginBase,
  type OpenClawConfig,
} from "openclaw/plugin-sdk/channel-core";
import { requireRelay } from "./runtime.js";

type MailMooseAccount = {
  accountId: string | null;
  baseUrl: string;
  gatewayId: string;
  secret: string;
  deliveryKey?: string;
};

function section(cfg: OpenClawConfig): any {
  return (cfg.channels as Record<string, any> | undefined)?.mailmoose ?? {};
}

function resolveAccount(cfg: OpenClawConfig, accountId?: string | null): MailMooseAccount {
  const value = section(cfg);
  if (!value.baseUrl || !value.gatewayId || !value.secret) {
    throw new Error("mailmoose: baseUrl, gatewayId and secret are required");
  }

  return {
    accountId: accountId ?? "default",
    baseUrl: value.baseUrl,
    gatewayId: value.gatewayId,
    secret: value.secret,
    deliveryKey: value.deliveryKey,
  };
}

export const mailMoosePlugin = createChatChannelPlugin<MailMooseAccount>({
  base: createChannelPluginBase({
    id: "mailmoose",
    config: {
      listAccountIds: () => ["default"],
      resolveAccount,
      inspectAccount(cfg) {
        const value = section(cfg);
        const configured = Boolean(value.baseUrl && value.gatewayId && value.secret);
        return {
          enabled: value.enabled !== false,
          configured,
          tokenStatus: value.secret ? "available" : "missing",
        };
      },
    },
    setup: {
      applyAccountConfig: ({ cfg, input }) => ({
        ...cfg,
        channels: {
          ...cfg.channels,
          mailmoose: {
            ...(cfg.channels as Record<string, any> | undefined)?.mailmoose,
            ...input,
          },
        },
      }),
    },
  }),

  threading: {
    topLevelReplyToMode: "reply",
  },

  outbound: {
    attachedResults: {
      channel: "mailmoose",
      sendText: async (params: any) => {
        const relay = requireRelay();
        const messageId = await relay.sendText(
          String(params.to ?? params.threadId ?? ""),
          String(params.text ?? ""),
        );
        return { messageId };
      },
    },
  },
});
