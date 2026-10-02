import { defineChannelPluginEntry } from "openclaw/plugin-sdk/channel-core";
import { mailMoosePlugin } from "./src/channel.js";
import { MailMooseRelay } from "./src/relay.js";
import { setRelay } from "./src/runtime.js";
import { dispatchInbound } from "./src/inbound.js";

function channelConfig(cfg: any) {
  return cfg?.channels?.mailmoose ?? {};
}

export default defineChannelPluginEntry({
  id: "mailmoose",
  name: "MailMoose",
  description: "MailMoose outbound-only email relay",
  plugin: mailMoosePlugin,

  registerFull(api) {
    let relay: MailMooseRelay | undefined;

    api.registerService({
      id: "mailmoose-relay",
      reload: { configPrefixes: ["channels.mailmoose"] },

      async start(ctx) {
        const cfg = channelConfig(ctx.config);
        if (cfg.enabled === false) {
          return;
        }
        if (!cfg.baseUrl || !cfg.gatewayId || !cfg.secret) {
          throw new Error("mailmoose: baseUrl, gatewayId and secret are required");
        }

        relay = new MailMooseRelay({
          baseUrl: cfg.baseUrl,
          gatewayId: cfg.gatewayId,
          secret: cfg.secret,
          onInbound: async (event) => {
            await dispatchInbound(api.runtime, event);
          },
        });

        setRelay(relay);
        await relay.start(ctx.abortSignal);
      },

      async stop() {
        setRelay(undefined);
        await relay?.stop();
        relay = undefined;
      },
    });
  },
});
