/**
 * Module-scoped runtime store for the MailMoose channel.
 *
 * The plugin runtime is installed by the channel entry at registration time
 * and used by the per-account gateway and outbound paths. The active relay is
 * tracked per account id so a config reload restarts only the affected socket.
 */
import type { PluginRuntime } from "openclaw/plugin-sdk/channel-core";
import type { MailMooseRelay } from "./relay.js";

let runtime: PluginRuntime | undefined;
const relays = new Map<string, MailMooseRelay>();

export function setMailMooseRuntime(next: PluginRuntime | undefined): void {
  runtime = next;
}

export function getMailMooseRuntime(): PluginRuntime {
  if (!runtime) {
    throw new Error("mailmoose: plugin runtime is not available");
  }
  return runtime;
}

export function tryGetMailMooseRuntime(): PluginRuntime | undefined {
  return runtime;
}

export function setRelay(accountId: string, relay: MailMooseRelay | undefined): void {
  if (relay) {
    relays.set(accountId, relay);
  } else {
    relays.delete(accountId);
  }
}

export function getRelay(accountId: string): MailMooseRelay | undefined {
  return relays.get(accountId);
}
