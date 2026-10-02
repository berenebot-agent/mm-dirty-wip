/**
 * MailMoose OpenClaw channel entry.
 *
 * Installed externally (npm/ClawHub); the OpenClaw host loads the built entry.
 * Setup is claimed through a MailMoose one-time code; see setup-entry.ts.
 */
import { defineChannelPluginEntry } from "openclaw/plugin-sdk/channel-core";
import { MAILMOOSE_CHANNEL_ID, mailMoosePlugin } from "./src/channel.js";
import { setMailMooseRuntime } from "./src/runtime.js";

export default defineChannelPluginEntry({
  id: MAILMOOSE_CHANNEL_ID,
  name: "MailMoose",
  description: "Email inbox connector for OpenClaw through a MailMoose relay.",
  plugin: mailMoosePlugin,
  setRuntime: (runtime) => setMailMooseRuntime(runtime),
});