import { defineSetupPluginEntry } from "openclaw/plugin-sdk/channel-core";
import { mailMoosePlugin } from "./src/channel.js";

export default defineSetupPluginEntry(mailMoosePlugin);
