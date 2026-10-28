/**
 * Mammoth Mail OpenClaw channel entry.
 *
 * Installed externally (npm/ClawHub); the OpenClaw host loads the built entry.
 * Setup is claimed through a Mammoth Mail one-time code; see setup-entry.ts.
 */
import { defineChannelPluginEntry } from "openclaw/plugin-sdk/channel-core";
import { MAIL_CHANNEL_ID, mammothMailPlugin } from "./src/channel.js";
import { setMammothMailRuntime } from "./src/runtime.js";

export default defineChannelPluginEntry({
  id: MAIL_CHANNEL_ID,
  name: "Mammoth Mail",
  description: "Email inbox connector for OpenClaw through a Mammoth Mail relay.",
  plugin: mammothMailPlugin,
  setRuntime: (runtime) => setMammothMailRuntime(runtime),
});