import test from "node:test";
import assert from "node:assert/strict";
import { normalizeBaseUrl, resolveMailMooseAccount } from "../src/accounts.js";

test("normalizes valid http(s) base URLs", () => {
  assert.equal(normalizeBaseUrl("https://mail.example.com/"), "https://mail.example.com");
  assert.equal(normalizeBaseUrl("http://host:8081/base/"), "http://host:8081/base");
  assert.equal(normalizeBaseUrl("ftp://host"), "");
  assert.equal(normalizeBaseUrl("not a url"), "");
});

test("resolves the implicit default account from root channel config", () => {
  const account = resolveMailMooseAccount({
    cfg: {
      channels: {
        mailmoose: {
          baseUrl: "https://mail.example.com",
          gatewayId: "gw-1",
          secret: "s",
        },
      },
    },
  });
  assert.equal(account.accountId, "default");
  assert.equal(account.configured, true);
  assert.equal(account.tokenStatus, "available");
  assert.deepEqual(account.allowFrom, []);
});

test("an incomplete config is not configured", () => {
  const account = resolveMailMooseAccount({
    cfg: { channels: { mailmoose: { baseUrl: "https://mail.example.com" } } },
  });
  assert.equal(account.configured, false);
  assert.equal(account.tokenStatus, "missing");
});

test("allowFrom entries are normalized and trimmed", () => {
  const account = resolveMailMooseAccount({
    cfg: {
      channels: {
        mailmoose: {
          baseUrl: "https://mail.example.com",
          gatewayId: "gw-1",
          secret: "s",
          allowFrom: [" alice@example.com ", "", "bob@example.com"],
        },
      },
    },
  });
  assert.deepEqual(account.allowFrom, ["alice@example.com", "bob@example.com"]);
});
