import test from "node:test";
import assert from "node:assert/strict";
import { claimMailMooseSetupCode, parseSetupCode } from "../src/setup.js";

test("parseSetupCode reads the base URL from a setup URL fragment", () => {
  const parsed = parseSetupCode({ code: "http://mail.example.com/#abc123" });
  assert.equal(parsed.baseUrl, "http://mail.example.com");
  assert.equal(parsed.code, "abc123");
});

test("parseSetupCode requires a base URL for a bare code", () => {
  assert.throws(() => parseSetupCode({ code: "abc123" }));
  const parsed = parseSetupCode({ code: "abc123", baseUrl: "https://mail.example.com/" });
  assert.equal(parsed.baseUrl, "https://mail.example.com");
  assert.equal(parsed.code, "abc123");
});

test("claimMailMooseSetupCode posts the code and returns credentials", async () => {
  const calls: Array<{ url: string; body: string }> = [];
  const fetchImpl = (async (url: string, init: { body: string }) => {
    calls.push({ url: String(url), body: init.body });
    return new Response(JSON.stringify({ secret: "sec", gatewayId: "gw-x", kind: "openclaw" }), {
      status: 200,
    });
  }) as unknown as typeof fetch;
  const result = await claimMailMooseSetupCode({
    baseUrl: "http://mail.example.com/",
    code: "abc",
    gatewayId: "gw-x",
    fetchImpl,
  });
  assert.equal(calls[0].url, "http://mail.example.com/relay/enroll");
  assert.deepEqual(JSON.parse(calls[0].body), { enrollmentToken: "abc", gatewayId: "gw-x" });
  assert.equal(result.secret, "sec");
});

test("claimMailMooseSetupCode surfaces an expired code", async () => {
  const fetchImpl = (async () => new Response("no", { status: 403 })) as unknown as typeof fetch;
  await assert.rejects(
    () => claimMailMooseSetupCode({ baseUrl: "http://mail.example.com", code: "x", fetchImpl }),
    /invalid, expired or already used/,
  );
});
