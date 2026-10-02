import test from "node:test";
import assert from "node:assert/strict";
import crypto from "node:crypto";
import { relayBearer, websocketUrl } from "../src/relay.js";

test("websocketUrl maps http(s) to ws(s) and keeps only the relay path", () => {
  assert.equal(websocketUrl("https://mail.example.com"), "wss://mail.example.com/relay");
  assert.equal(websocketUrl("http://mail.example.com:8081/base/"), "ws://mail.example.com:8081/relay");
});

// The relay bearer must stay byte-compatible with internal/hermesrelay:
// base64url("<gatewayId>:<exp>:<hex hmac-sha256 of "gatewayId:exp">").
test("relayBearer matches the MailMoose upgrade token format", () => {
  const gatewayId = "gw-oc-test";
  const secret = "s3cret";
  const now = 1_800_000_000_000; // fixed clock
  const token = relayBearer(gatewayId, secret, now);
  const raw = Buffer.from(token, "base64url").toString("utf8");
  const [id, exp, sig] = raw.split(":");
  assert.equal(id, gatewayId);
  assert.equal(Number(exp), Math.floor(now / 1000) + 300);
  const expected = crypto.createHmac("sha256", secret).update(`${id}:${exp}`).digest("hex");
  assert.equal(sig, expected);
});

test("relayBearer produces different signatures for different secrets", () => {
  assert.notEqual(relayBearer("gw", "a", 0), relayBearer("gw", "b", 0));
});
