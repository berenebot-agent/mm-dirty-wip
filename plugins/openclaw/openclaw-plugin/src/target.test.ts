import test from "node:test";
import assert from "node:assert/strict";
import {
  buildMammothMailTarget,
  looksLikeMammothMailTarget,
  normalizeMammothMailTarget,
  parseMammothMailTarget,
  threadIdFromTarget,
} from "../src/target.js";

test("parses canonical thread targets", () => {
  assert.deepEqual(parseMammothMailTarget("thread:thr_123"), { kind: "thread", id: "thr_123" });
});

test("accepts channel prefixes and bare ids", () => {
  assert.equal(threadIdFromTarget("mammothmail:thread:thr_1"), "thr_1");
  assert.equal(threadIdFromTarget("mm:thr_2"), "thr_2");
  assert.equal(threadIdFromTarget("thr_3"), "thr_3");
});

test("round-trips and normalizes", () => {
  const target = parseMammothMailTarget("thr_9");
  assert.equal(buildMammothMailTarget(target), "thread:thr_9");
  assert.equal(normalizeMammothMailTarget(" mm:thr_9 "), "thread:thr_9");
});

test("rejects empty targets", () => {
  assert.throws(() => parseMammothMailTarget("   "));
  assert.throws(() => parseMammothMailTarget("thread:"));
});

test("looksLikeMammothMailTarget accepts any non-empty text", () => {
  assert.equal(looksLikeMammothMailTarget("thread:thr_1"), true);
  assert.equal(looksLikeMammothMailTarget("  "), false);
});
