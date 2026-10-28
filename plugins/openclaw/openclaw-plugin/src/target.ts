/**
 * Mammoth Mail target grammar.
 *
 * A target identifies one email thread, which is the unit of conversation in
 * Mammoth Mail. The canonical form is `thread:<thread_id>`; a bare id is accepted
 * for convenience (and the plugin also accepts a `mammothmail:`/`mm:` channel
 * prefix, which core strips before this parser runs).
 */

export type MammothMailTarget = {
  kind: "thread";
  id: string;
};

const PREFIXES = ["mammothmail", "mm", "thread"];

function stripPrefix(value: string): string {
  let trimmed = value.trim();
  // Strip any chain of known prefixes (`mammothmail:thread:<id>`, `mm:<id>`, ...)
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

/** Parses a Mammoth Mail target into a thread reference. */
export function parseMammothMailTarget(raw: string): MammothMailTarget {
  const value = raw.trim();
  if (!value) {
    throw new Error("Mammoth Mail target is required");
  }
  const id = stripPrefix(value);
  if (!id) {
    throw new Error(`Unsupported Mammoth Mail target: ${raw}`);
  }
  return { kind: "thread", id };
}

/** Formats a parsed target back into canonical `<kind>:<id>` syntax. */
export function buildMammothMailTarget(target: MammothMailTarget): string {
  return `${target.kind}:${target.id}`;
}

/** Normalizes user-entered target text for channel routing. */
export function normalizeMammothMailTarget(raw: string): string {
  return buildMammothMailTarget(parseMammothMailTarget(raw));
}

/** Reports whether a target string can be offered to the parser. */
export function looksLikeMammothMailTarget(raw: string): boolean {
  return raw.trim().length > 0;
}

/** Extracts the bare thread id used on the relay wire. */
export function threadIdFromTarget(raw: string): string {
  return parseMammothMailTarget(raw).id;
}
