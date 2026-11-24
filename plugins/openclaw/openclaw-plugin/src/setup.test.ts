import test from "node:test";
import assert from "node:assert/strict";
import { moveSingleAccountChannelSectionToDefaultAccount } from "openclaw/plugin-sdk/setup";
import { resolveMammothMailAccount } from "../src/accounts.js";
import { claimMammothMailSetupCode, mammothMailSetupContract, parseSetupCode } from "../src/setup.js";

test("promotes the paired default before adding a named connector", () => {
  const cfg = { channels: { mammothmail: {
    enabled: true, baseUrl: "https://first.example.com", gatewayId: "first", secret: "first-secret",
    agentId: "agent", allowFrom: ["alice@example.com"],
  } } };
  const promoted = moveSingleAccountChannelSectionToDefaultAccount({
    cfg: cfg as never, channelKey: "mammothmail", setupSurface: mammothMailSetupContract,
  });
  const updated = mammothMailSetupContract.applyAccountConfig({
    cfg: promoted, accountId: "second", input: {
      baseUrl: "https://second.example.com", gatewayId: "second", secret: "second-secret",
    },
  });
  const first = resolveMammothMailAccount({ cfg: updated, accountId: "default" });
  const second = resolveMammothMailAccount({ cfg: updated, accountId: "second" });
  assert.equal(first.baseUrl, "https://first.example.com");
  assert.equal(first.gatewayId, "first");
  assert.equal(first.secret, "first-secret");
  assert.equal(first.agentId, "agent");
  assert.deepEqual(first.allowFrom, ["alice@example.com"]);
  assert.equal(second.baseUrl, "https://second.example.com");
  assert.equal(second.gatewayId, "second");
  assert.equal(second.secret, "second-secret");
});

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

test("claimMammothMailSetupCode posts the code and returns credentials", async () => {
  const calls: Array<{ url: string; body: string }> = [];
  const fetchImpl = (async (url: string, init: { body: string }) => {
    calls.push({ url: String(url), body: init.body });
    return new Response(JSON.stringify({ secret: "sec", gatewayId: "gw-x", kind: "openclaw" }), {
      status: 200,
    });
  }) as unknown as typeof fetch;
  const result = await claimMammothMailSetupCode({
    baseUrl: "http://mail.example.com/",
    code: "abc",
    gatewayId: "gw-x",
    fetchImpl,
  });
  assert.equal(calls[0].url, "http://mail.example.com/relay/enroll");
  assert.deepEqual(JSON.parse(calls[0].body), { enrollmentToken: "abc", gatewayId: "gw-x" });
  assert.equal(result.secret, "sec");
});

test("claimMammothMailSetupCode surfaces an expired code", async () => {
  const fetchImpl = (async () => new Response("no", { status: 403 })) as unknown as typeof fetch;
  await assert.rejects(
    () => claimMammothMailSetupCode({ baseUrl: "http://mail.example.com", code: "x", fetchImpl }),
    /invalid, expired or already used/,
  );
});
