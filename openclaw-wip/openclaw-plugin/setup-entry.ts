/**
 * MailMoose setup entry.
 *
 * Loaded by OpenClaw during onboarding/`channels add`, without starting the
 * relay. It exposes the same channel plugin so the setup contract is available
 * before the account is configured.
 */
import { defineSetupPluginEntry } from "openclaw/plugin-sdk/channel-core";
import { mailMoosePlugin } from "./src/channel.js";

export default defineSetupPluginEntry(mailMoosePlugin);