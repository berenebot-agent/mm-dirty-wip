import type { MailMooseRelay } from "./relay.js";

let activeRelay: MailMooseRelay | undefined;

export function setRelay(relay: MailMooseRelay | undefined): void {
  activeRelay = relay;
}

export function requireRelay(): MailMooseRelay {
  if (!activeRelay) {
    throw new Error("mailmoose: relay is not connected");
  }
  return activeRelay;
}
