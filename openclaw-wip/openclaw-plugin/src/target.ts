/**
 * MailMoose target grammar.
 *
 * A target identifies one email thread, which is the unit of conversation in
 * MailMoose. The canonical form is `thread:<thread_id>`; a bare id is accepted
 * for convenience (and the plugin also accepts a `mailmoose:`/`mm:` channel
 * prefix, which core strips before this parser runs).
 */

export type MailMooseTarget = {
  kind: "thread";
  id: string;
};

const PREFIXES = ["mailmoose", "mm", "thread"];

function stripPrefix(value: string): string {
  let trimmed = value.trim();
  // Strip any chain of known prefixes (`mailmoose:thread:<id>`, `mm:<id>`, ...)
  // so both the channel prefix core adds and the kind prefix users type work.
  for (;;) {
    const lower = trimmed.toLowerCase();
    const match = PREFIXES.find((prefix) => lower.startsWith(`${prefix}:`));
    if (!match) {
      return trimmed;
    }
    trimmed = trimmed.slice(match.length + 1).trim();
  }
}

/** Parses a MailMoose target into a thread reference. */
export function parseMailMooseTarget(raw: string): MailMooseTarget {
  const value = raw.trim();
  if (!value) {
    throw new Error("MailMoose target is required");
  }
  const id = stripPrefix(value);
  if (!id) {
    throw new Error(`Unsupported MailMoose target: ${raw}`);
  }
  return { kind: "thread", id };
}

/** Formats a parsed target back into canonical `<kind>:<id>` syntax. */
export function buildMailMooseTarget(target: MailMooseTarget): string {
  return `${target.kind}:${target.id}`;
}

/** Normalizes user-entered target text for channel routing. */
export function normalizeMailMooseTarget(raw: string): string {
  return buildMailMooseTarget(parseMailMooseTarget(raw));
}

/** Reports whether a target string can be offered to the parser. */
export function looksLikeMailMooseTarget(raw: string): boolean {
  return raw.trim().length > 0;
}

/** Extracts the bare thread id used on the relay wire. */
export function threadIdFromTarget(raw: string): string {
  return parseMailMooseTarget(raw).id;
}
