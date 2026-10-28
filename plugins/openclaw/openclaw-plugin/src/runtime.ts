/**
 * Module-scoped runtime store for the Mammoth Mail channel.
 *
 * The plugin runtime is installed by the channel entry at registration time
 * and used by the per-account gateway and outbound paths. The active relay is
 * tracked per account id so a config reload restarts only the affected socket.
 */
import type { PluginRuntime } from "openclaw/plugin-sdk/channel-core";
import type { MammothMailRelay } from "./relay.js";

let runtime: PluginRuntime | undefined;
const relays = new Map<string, MammothMailRelay>();

export function setMammothMailRuntime(next: PluginRuntime | undefined): void {
  runtime = next;
}

export function getMammothMailRuntime(): PluginRuntime {
  if (!runtime) {
    throw new Error("mammothmail: plugin runtime is not available");
  }
  return runtime;
}

export function tryGetMammothMailRuntime(): PluginRuntime | undefined {
  return runtime;
}

export function setRelay(accountId: string, relay: MammothMailRelay | undefined): void {
  if (relay) {
    relays.set(accountId, relay);
  } else {
    relays.delete(accountId);
  }
}

export function getRelay(accountId: string): MammothMailRelay | undefined {
  return relays.get(accountId);
}
