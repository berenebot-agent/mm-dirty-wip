import test from "node:test";
import assert from "node:assert/strict";
import {
  buildMailMooseTarget,
  looksLikeMailMooseTarget,
  normalizeMailMooseTarget,
  parseMailMooseTarget,
  threadIdFromTarget,
} from "../src/target.js";

test("parses canonical thread targets", () => {
  assert.deepEqual(parseMailMooseTarget("thread:thr_123"), { kind: "thread", id: "thr_123" });
});

test("accepts channel prefixes and bare ids", () => {
  assert.equal(threadIdFromTarget("mailmoose:thread:thr_1"), "thr_1");
  assert.equal(threadIdFromTarget("mm:thr_2"), "thr_2");
  assert.equal(threadIdFromTarget("thr_3"), "thr_3");
});

test("round-trips and normalizes", () => {
  const target = parseMailMooseTarget("thr_9");
  assert.equal(buildMailMooseTarget(target), "thread:thr_9");
  assert.equal(normalizeMailMooseTarget(" mm:thr_9 "), "thread:thr_9");
});

test("rejects empty targets", () => {
  assert.throws(() => parseMailMooseTarget("   "));
  assert.throws(() => parseMailMooseTarget("thread:"));
});

test("looksLikeMailMooseTarget accepts any non-empty text", () => {
  assert.equal(looksLikeMailMooseTarget("thread:thr_1"), true);
  assert.equal(looksLikeMailMooseTarget("  "), false);
});
