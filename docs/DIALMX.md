# Dial MX — standalone receiver

All MX deployments now use the same Dial MX session transport. The core
**dials out**: a standalone receiver
terminates SMTP, verifies the core's right to receive for a domain using
a DNS-anchored Ed25519 challenge, and streams received mail to that core over the
outbound-established session. No public inbound port on the core is required — only outbound
HTTPS/2 from the core to the receiver.

The receiver defaults to `DIALMX_MODE=single`: bearer authentication, one core,
no domain registration, and optional session TLS. See [MX.md](MX.md) for built-in
and private standalone setup. The remainder of this guide describes
`DIALMX_MODE=shared`: the existing DNS-authenticated multi-tenant mode, with TLS
required. A domain's provider is `mx` for a private receiver or `dialmx` for a
shared receiver; both use core-established sessions.

The receiver is a single pure-Go process with no `/data`, no database and no
`APP_ENCRYPTION_KEY`. The core remains the durable source of truth.

## 1. Topology

```text
Internet TCP :25
       |
       v
mailmoose-dialmx (one process, non-root, in-memory staging)
  |  SMTP edge :2525 (SPF/DKIM/DMARC on the original bytes)
  |  HTTPS/2 session listener :8443
  |
  |  core dials receiver outbound over verified TLS/HTTP2
  v
MailMoose core (dialmx provider configured per domain)
  |
  v
SQLite + raw MIME under /data -> durable events -> API / UI / Relay
```

The receiver holds the SMTP connection; the core establishes the bidirectional
session. On its own schedule the core opens a session per receiver URL, proves
key control, registers the domains it is responsible for, and then serves
`Resolve` and `Ingest` requests as SMTP mail arrives.

## 2. Deployment

Build and run from the repository root:

```bash
mkdir -p dialmx/certs
cp /path/to/fullchain.pem /path/to/privkey.pem dialmx/certs/
docker compose -f dialmx/compose.yml up -d --build
```

The image is built from `dialmx/Dockerfile` with the repository root as build
context. Host `:25` maps to the SMTP edge (`:2525` inside) and host `:443` maps
to the session listener (`:8443` inside). Both are unprivileged inside the
container, so the process drops all capabilities and needs no writable
filesystem.

A minimal image is also available for a remote host:

```bash
docker build -f dialmx/Dockerfile -t mailmoose-dialmx .
```

The receiver must be reachable by the core at an HTTPS base URL and by sending
MTAs on port 25. It needs DNS egress for domain proofs and SPF/DKIM/DMARC, plus
operator-managed TLS certificates.

### Core side

The core only starts its dialer for domains that have a `dialmx` receiving
configuration. `MX_ENABLE` is about the **inbound** MX edge only: Dial MX works
with `MX_ENABLE=false`, `true`, or `remote`. A Dial-MX-only deployment can use
`MX_ENABLE=false` and needs no shared bearer key. The core always runs its dedicated
inbound webhook listener on `:8082`; that listener continues to accept the
webhook connectors, so do not expose it to the internet unless you also use
those — Dial MX itself opens no inbound port on the core.

### Configuration

The receiver reuses the documented `MX_*` SMTP surface and adds only the
session listener and certificate settings:

| Variable | Default | Meaning |
|---|---|---|
| `DIALMX_MODE` | `single` | `single` for one bearer-authenticated core; `shared` for DNS-authenticated domains. |
| `DIALMX_CORE_KEY` | — | Required bearer key in single mode; unused in shared mode. |
| `MX_HOSTNAME` | `localhost` | SMTP greeting hostname; independent of the session listener address. |
| `DIALMX_LISTEN_ADDR` | `:8443` | HTTPS/2 session listener. |
| `DIALMX_TLS_CERT` | — | Session certificate; optional in single mode, required in shared mode. |
| `DIALMX_TLS_KEY` | — | Session private key; set together with certificate. |
| `MX_TLS_CERT` / `MX_TLS_KEY` | unset | Optional SMTP STARTTLS pair; set together. |
| `MX_REQUIRE_TLS` | `false` | Refuse plaintext SMTP. |
| `MX_VERIFY_SPF` / `MX_VERIFY_DKIM` / `MX_VERIFY_DMARC` | `true` | Which evidence classes the edge computes. |
| `MX_DNS_RESOLVER` | system | Resolver for verification and the TXT proof. |
| `MX_MAX_MESSAGE_BYTES` | `31457280` | Per-message cap. |
| `MX_STAGING_BYTES` | `268435456` | In-memory staging budget. |
| `MX_MAX_RECIPIENTS` | `100` | Recipients per transaction. |
| `MX_MAX_CONNECTIONS` | `256` | Concurrent SMTP connections. |
| `MX_MAX_TRANSACTIONS` | `128` | Global receiver-to-core transaction groups. |
| `MX_MAX_DOMAINS_PER_CONNECTION` | `128` | Distinct domains per session. |
| `MX_MAX_TRANSACTIONS_PER_CONNECTION` | `16` | Concurrent groups per session. |
| `MX_MAX_TRANSACTIONS_PER_DOMAIN` | `8` | Concurrent transactions per domain. |
| `MX_AUTH_TIMEOUT_SECONDS` | `10` | Bounds one DNS proof. |
| `MX_RESOLVE_TIMEOUT_SECONDS` | `10` | Bounds one recipient resolve. |
| `MX_INGEST_TIMEOUT_SECONDS` | `180` | Bounds one staged message handoff. |
| `MX_REVALIDATE_SECONDS` | `240` | Nominal binding renewal period. |

Select `DIALMX_MODE=shared` and set the session certificate paths explicitly
when following this shared-mode guide. SMTP STARTTLS is configured separately.
The non-root receiver uid must be able to read both certificate files. Mount
renewed certificates and restart the receiver to reload them.

See `dialmx/.env.example` for a commented template.

## 3. Domain onboarding

1. In the core's Admin UI, open the domain and set **Receiving** to **Dial MX**.
2. Enter the receiver's HTTPS base URL (`https://mx.example.com`). Up to eight
   comma-separated base URLs are accepted; each must be an HTTPS origin with no
   path, userinfo, query or fragment. One domain may be served by several
   receivers.
3. The core generates a per-domain Ed25519 key pair and shows the matching
   public record. Publish it as a DNS TXT record:

   ```text
   _mailmoose-mx.<domain>  TXT  "v=MM1; k=ed25519; id=<key-id>; p=<base64 public key>"
   ```

   Every exact domain, including an inherited subdomain, owns a **separate key**
   and needs its own `_mailmoose-mx.<domain>` TXT proof. Receiver settings and
   enforcement may inherit; credentials never do. Regenerating the key requires
   replacing that domain's TXT record. Existing authorization lasts no longer
   than its current five-minute grant and fails at the next unsuccessful renewal.
4. Publish the domain's MX record pointing at the advertised SMTP hostname shown
   by the receiver status; it may differ from the HTTPS endpoint hostname.
5. Choose `moderate` or `hard` authentication enforcement. The evidence
   semantics are identical to the MX edge (see [MX.md](MX.md)): auth failure is
   a durable **Spam** delivery, never an SMTP rejection.

The credential private seed is stored encrypted with `APP_ENCRYPTION_KEY`. The
public record is safe to publish and is shown in the UI.

## 4. DNS authorisation flow

The core proves control of the **core signing key** published at
`_mailmoose-mx.<domain>`, not of the receiver. Every receiver URL is authorised
independently; a domain configured with three receivers is verified against
each.

1. The core opens an HTTPS/2 session to the receiver and sends `Hello`.
2. The receiver replies `Ready` with its SMTP hostname and message cap.
3. For each domain, the core sends `DomainAuth`. The receiver resolves the
   domain's `_mailmoose-mx` TXT record and returns a `Challenge` bound to this
   receiver id, connection id, domain, key id and a fresh 32-byte nonce.
4. The core signs the challenge transcript with the domain's Ed25519 key and
   returns `ChallengeResponse`. The receiver re-resolves DNS fresh and verifies
   the signature. On success it installs a binding for `AuthLifetime` (5
   minutes) and answers `AuthResult`.

Fresh DNS is required at every proof: a proof is only as good as the TXT record
observed at verification time, so revocation is effective as soon as the
receiver's resolver sees the new record. An upstream DNS cache with a long TTL
can delay that observation beyond the 5-minute local binding lifetime; the
binding itself is never extended past `AuthLifetime` from the last proof.

Bindings are renewed automatically before expiry (nominal `MX_REVALIDATE_SECONDS`,
default 240 s), so a healthy session keeps its domains registered. A failed
renewal invalidates the domain fail-closed.

## 5. Message flow

Once a binding is active, mail arriving at the SMTP edge is delivered without
further policy at the receiver:

1. **RCPT** — the receiver resolves the recipient live against the core: it
   sends `Resolve` for the recipient's domain and waits for `ResolveResult`. The
   core authorises the domain and decides per recipient. A temporary answer
   keeps the recipient retryable.
2. **DATA** — the receiver stages the original MIME once. `To`, `Cc` and `Bcc`
   headers are not used for routing; the envelope recipient is authoritative.
3. **Ingest** — the receiver sends `IngestStart` (the accepted domains and
   metadata: envelope from, client IP, HELO, computed SPF/DKIM/DMARC, size and
   the SHA-256 of the original bytes), streams the body in `IngestChunk` frames,
   then `IngestEnd`. The core verifies the exact size and digest before
   committing.

A single SMTP message can cover recipients across several domains. The receiver
groups recipients by connection, so one message fans out to one transaction per
connection, each carrying the domains that connection authenticated; the body is
streamed chunk-by-chunk and never buffered whole per recipient.

`Resolve` and `Ingest` are **connection-scoped**: the transaction id is scoped
to the session, and the digest is computed over the original bytes before any
local trace header is added.

### Durable acknowledgement

The SMTP `250` is returned **only** after the core has durably handled every
accepted recipient. A core that cannot commit answers `451`, so the sending MTA
retries. A partial fan-out (some connections accepted, some not) or a lost
acknowledgement also returns `451`: the receiver never claims success on behalf
of the core.

Deduplication and retry identity are the core's: the delivery fingerprint
(`mxfp-v1`) is a versioned digest over the canonical envelope sender, canonical
recipient and the SHA-256 of the original MIME, and the core records a durable
receipt per recipient for **7 days**. A sender retry inside that window returns
the recorded disposition without a second message, quota charge or event. As in
[MX.md](MX.md), this is **not** exactly-once: identical intentional resends
inside the window collapse, and changed upstream trace headers can defeat
cross-node deduplication.

## 6. Session wire contract (`mx-v2`)

The session is a single `POST /mx/v2/session` request over TLS with
`NextProtos: h2`. HTTP/1.1 fallback is refused, TLS 1.2 is the minimum, and the
core verifies the receiver's certificate and hostname. Unknown
frame types or flags are fatal: neither peer guesses transaction semantics.

### Frame header

Every frame is a fixed 24-byte network-byte-order header followed by an opaque
payload:

```text
offset  size  field
0       1     version (= 2)
1       1     type
2       2     flags (= 0; reserved, any other value is fatal)
4       4     payload length (uint32, <= 64 KiB)
8       8     transaction id (uint64; 0 for non-transactional frames)
16      8     domain channel id (uint64; 0 for connection frames and grouped ingest/cancel)
```

A metadata payload is JSON, decoded with unknown fields rejected, and is
capped at 16 KiB. A chunk payload is a 4-byte big-endian sequence number
followed by up to 32 KiB of body.

### Frame types

| Value | Type | Direction | Purpose |
|---|---|---|---|
| 1 | `Hello` | core → receiver | `{version, instance}`. |
| 2 | `Ready` | receiver → core | `{version, receiver_id, connection_id, smtp_hostname, max_message_bytes}`. |
| 3 | `DomainAuth` | core → receiver | `{domain, key_id}`. |
| 4 | `Challenge` | receiver → core | `{domain, key_id, receiver_id, connection_id, nonce}`. |
| 5 | `ChallengeResponse` | core → receiver | `{domain, key_id, nonce, signature}`. |
| 6 | `AuthResult` | receiver → core | `{domain, key_id, accepted, reason, expires_at}`. |
| 7 | `DomainRevoked` | receiver → core | `{domain, reason}` — a binding was superseded/expired. |
| 8 | `DomainUnregister` | core → receiver | `{domain, reason}`. |
| 9 | `Ping` / 10 `Pong` | both | keepalive. |
| 11 | `Resolve` | receiver → core | `{domain, recipient}`. |
| 12 | `ResolveResult` | core → receiver | `ResolveResponse`. |
| 13 | `IngestStart` | receiver → core | `{domains, metadata}`. |
| 14 | `IngestChunk` | receiver → core | sequence + body bytes. |
| 15 | `IngestEnd` | receiver → core | `{size, content_digest}`. |
| 16 | `IngestResult` | core → receiver | `IngestResponse`, one result per accepted recipient. |
| 17 | `Cancel` | both | abort the transaction. |

### Authentication transcript

`AuthTranscript` is the SHA-256 of six length-prefixed byte strings, each
prefixed with a `uint32` big-endian length:

```text
"mailmoose-mx-v2" || receiver_id || connection_id || domain || key_id || nonce
```

The nonce is the base64url-decoded 32 bytes from the `Challenge`. Ed25519 signs
the SHA-256 digest as a **message** (not the Ed25519ph variant). The signature
is standard base64 in `ChallengeResponse.signature`.

Test fixtures pin this transcript so the two implementations cannot drift.

### Result contract

Each `ResolveResult` reports accepted/denied per recipient, with a temporary
flag for retryable answers. Each `IngestResult` carries exactly one entry per
accepted recipient; only the `OK` and `duplicate` machine codes are durable
successes, and every such result must carry a valid disposition
(`stored`/`spam`/`blocked`/`control`).

## 7. Known limitations

- **Not exactly-once.** Deduplication is sender-independent within the 7-day
  receipt window only; see above.
- **One receiver per session.** The core opens one physical session per receiver
  URL and registers the union of the domains configured for it. Domains on
  inherited configurations are served through the parent's receivers.
- **A replaced binding still serves pinned DATA** until it expires: new RCPT
  lookups go to the new binding; an already accepted transaction may finish on
  the old connection while its grant remains valid. Revocation/expiry aborts it.
- **No automatic core HA.** The last valid authentication wins for an exact
  domain on each receiver. This permits a deliberate takeover, not coordinated
  active-active cores or automatic failover.
- **No mTLS.** The receiver authenticates the core by the DNS-anchored challenge
  in §4, not by client certificates. Session traffic is TLS-encrypted but the
  peer is identified by the signed challenge, and DNS freshness is the
  revocation mechanism.
- **Local DNS cache TTL** delays observed revocation; see §4.
- **Operator-managed CA trust.** The core uses system roots. `DIALMX_CA_FILE` can
  add a PEM private-CA bundle mounted in the core; hostname verification remains
  mandatory. There is no insecure TLS mode.

## Metadata logging

The standalone receiver writes JSON records to stderr. All records have `time`
(UTC), `level`, `msg`, `event`, `schema_version: 1`, `service: dialmx` and
`boot_id`. Normal lifecycle events are INFO; failures also retain warning/error
diagnostics. Message bodies, subjects, raw headers, TXT records, challenge
payloads, signatures and credentials are not logged. Envelope addresses, IPs,
domains and normalized authentication evidence are logged.

### Correlation and events

- **Transport:** `dialmx transport accepted`, `tls failure`, `closed` record
  HTTPS peer/port, `transport_id`, duration and TLS handshake failure. The
  transport ID links to admitted sessions.
- **Core sessions:** `dialmx session rejected`, `opened`, `hello`, `closed`
  record protocol/TLS details, `core_connection_id`, advertised `core_label`,
  duration, close reason and counters. The label is caller-advertised, not a
  unique or authenticated core identity (the current core sends `gatehouse`).
  Domain/key bindings identify the authority proved on that connection.
- **Domain authority:** `dialmx domain proof` records initial and renewal
  DNS lookup, key parsing, signature validation, grants, expiry, revocation and
  replacement. Fields include domain, key ID, core session, phase, outcome,
  reason, query name, resolver setting, duration and grant expiry as applicable.
  DNS lookup success and signature validity are distinct outcomes. `system`
  denotes resolver configuration, not a known upstream resolver address.
- **SMTP:** `mx connection opened/closed`, `mx session started` and
  `mx starttls established` record `smtp_connection_id`, endpoints, HELO,
  TLS details, byte counts and duration. Identity persists across STARTTLS.
  Open/close coverage includes peers that never reach HELO. The backend
  observes successful STARTTLS; failed SMTP handshakes and syntax errors handled
  internally by go-smtp may only appear in its error diagnostics/connection
  closure, rather than a classified transaction event.
- **Messages:** `mx mail transaction started/rejected`, `mx recipient routing`,
  `mx data staging`, `mx transaction abandoned`, and
  `mx smtp transaction decision` record `message_transaction_id`, envelope
  addresses, staging size/digest, duration, routing and final SMTP decision.
  Reset, replacement MAIL and disconnect terminate unfinished transactions.
- **Recipient destinations:** `dialmx resolve` records the full recipient,
  destination domain, selected core/key/channel, wire transaction, duration and
  accepted/rejected/temporary result. No binding is recorded explicitly rather
  than attributed to a core.
- **Authentication:** `mx auth evidence` records normalized `auth_results`
  (SPF, DKIM signatures, DMARC policy/alignment and diagnostic reasons), enabled
  flags and duration. Disabled verification differs from absent evidence. DKIM
  selector/algorithm are omitted because the current evaluator does not populate
  them.
- **Handoff:** `dialmx handoff` start and terminal records contain recipients,
  domains, size/digest, completed body-chunk bytes, duration, phase and outcome.
  `handoff_id` combines `core_connection_id` and `wire_transaction_id`; wire
  IDs alone are not globally unique. `dialmx handoff result` records each
  recipient's machine code, disposition, duplicate flag, core message ID and
  bounded reason. Partial fan-out retains earlier acknowledgements even when
  a later core fails.
- **Reply observation:** `mx core ingest result` and
  `mx smtp transaction decision` separate core response from chosen SMTP reply.
  `mx smtp reply transport write` observes the next underlying transport write,
  not remote receipt of a full reply; with TLS it can be the first encrypted
  fragment. Connection read outcomes classify timeout/EOF/error where observed.
- **Process:** receiver starting/settings/listener bound/ready/stopping/stopped
  events report build/protocol, effective limits and verification settings.
  Abrupt process termination cannot emit terminal events; unmatched starts
  from an earlier `boot_id` remain interrupted/unknown.

A valid core response is `acknowledged`, including quota/transient responses;
only individual `ok`/`duplicate` results with a valid durable disposition prove
durable handling. Failure while sending `IngestEnd` or awaiting/validating a
response is `unknown`: the core may have committed. The sender still receives
a temporary failure. Logs never substitute for durable receipts in the core.

### Retention and viewing

The bundled Compose explicitly selects Docker's `local` log driver with
`max-size: 20m` and `max-file: 10` (approximately 200 MB per container before
compression). Rotation is size-based, not a guaranteed number of days. Docker
retains logs across stop/start and host restarts, but removes them when the
container is removed/recreated. They are outside `/data` and its backups.

```bash
docker compose -f dialmx/compose.yml logs --timestamps --tail=200 dialmx
docker compose -f dialmx/compose.yml logs --follow dialmx
```

Trace a message using its `message_transaction_id`, then follow its resolve and
handoff records to `core_connection_id`; use that ID for DNS proof and session
history. Use `smtp_connection_id` for multiple messages on one SMTP connection.
Export needed history before replacing the container.

## 8. Testing

Go is not on the host. Run focused tests through the wrapper with a bounded
timeout:

```bash
timeout 60s ./mailmoose-go.sh test -race -count=1 -timeout=40s ./tests/unit/dialmx/... ./tests/unit/mxdial
```

`./tests/run.sh` runs the CI-parity tiers (`unit + vet + fmt`); the formatting
gate includes `dialmx`:

```bash
./tests/run.sh --fmt
```

The wire protocol has byte-level fixtures so the core and the receiver cannot
silently diverge.
