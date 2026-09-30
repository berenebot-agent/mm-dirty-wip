# MailMoose → Billbot inbound webhook contract

This document is the shared source of truth for the wire contract between
MailMoose (producer) and Billbot (consumer). Both repositories maintain an
identical copy of the committed fixture and this format description; keep them
in sync by hand when the contract changes.

Canonical direction:

```text
MailMoose webhook client (forward mode) → POST /api/v1/ingest/<webhook-id>  (Billbot)
```

## Request

```http
POST /api/v1/ingest/<webhook-id>
Authorization: Bearer <MailMoose-generated-token>
Content-Type: message/rfc822
X-MailMoose-Envelope-From: <RFC 3986 percent-encoded UTF-8 envelope sender>
X-MailMoose-Envelope-To:   <RFC 3986 percent-encoded UTF-8 envelope recipient>
X-MailMoose-Event:         message.received
X-MailMoose-Delivery:      <client id>:<cursor>
X-MailMoose-Message-Id:    <MailMoose message id>
X-MailMoose-Cursor:        <event cursor>
```

- **Body** is the original MIME message, byte-for-byte, streamed from the stored
  raw file. MailMoose never rewrites it.
- **Auth** is the static bearer token generated once per webhook client
  (creation or rotation) and shown once. Billbot stores only the token's
  SHA-256 hash. The timestamped signature mode is not part of this contract;
  the consumer uses bearer mode only.
- **Envelope metadata** is carried in the two `X-MailMoose-Envelope-*` headers,
  not in the MIME `From:`/`Return-Path:` headers. Values use RFC 3986
  percent-encoding over UTF-8: space is `%20`, a literal plus is `%2B`, `@` is
  `%40` (this is **not** `application/x-www-form-urlencoded`, where `+` means
  space). The values come from the persisted transport metadata and are
  identical on retries.
- A **missing envelope sender** is emitted as an empty header value. The
  receiver fails closed in restricted mode and must never fall back to the MIME
  `From:` header.
- The event/delivery/cursor/message-id headers are informative only. Tenant
  routing always comes from the authenticated `<webhook-id>`, never from a
  header or the payload.
- The envelope sender is **relay-supplied**, not provider-attested: it is
  metadata a receiver may record, not authority it should trust without its own
  provenance check (see `SECURITY.md`).

## Responses

| Status | Meaning |
|---|---|
| `202` | accepted (idempotent: a retry returns the same `job_id`) |
| `400` | invalid message, including a JSON/non-`message/rfc822` payload |
| `401` | missing or bad bearer token |
| `403` | sender policy rejection (`sender_not_allowed`); permanent, the worker does not retry |
| `413` | body exceeds the configured size cap |
| `503` | ingestion service unavailable |

## Committed fixture

The contract is proven end to end with a fixed synthetic request produced by
MailMoose's actual webhook worker (Go) and consumed by Billbot's intake tests
(Python):

- MailMoose: `tests/fixtures/mailmoose-webhook-contract.json`
- Billbot: `tests/fixtures/mailmoose-webhook-contract.json`

Both copies must be byte-identical. The generating test is
`TestWebhookForwardContractFixture` in `tests/unit/app/webhook_forward_test.go`;
it is skipped unless `BILLBOT_CONTRACT_FIXTURE` is set, so ordinary test runs
never write files. Billbot's `tests/test_mailmoose.py::test_mailmoose_fixture_end_to_end`
runs against the committed fixture on a clean checkout; setting
`BILLBOT_CONTRACT_FIXTURE` overrides the path, and an override pointing at a
missing file skips the test.

## Regenerating the fixture

From the MailMoose checkout:

```sh
BILLBOT_CONTRACT_FIXTURE="$PWD/tests/fixtures/mailmoose-webhook-contract.json" \
  ./mailmoose-go.sh test -count=1 -run TestWebhookForwardContractFixture ./tests/unit/app/
```

Then copy the file to Billbot's `tests/fixtures/` and commit both copies
together. Do not hand-edit the fixture: its bearer token
(`fixture-bearer-token`), body and headers are deterministic and the tests
assert against them.
