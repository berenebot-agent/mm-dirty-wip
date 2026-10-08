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
session. On its own schedule the core proves key control, registers the domains
it is responsible for, and then serves `Resolve` and `Ingest` requests as SMTP
mail arrives.

In shared mode the core does **not** open a single session per receiver URL: it
packs the configured domains into a bounded number of **stable shards**, each
one a session to that receiver, so a large domain set is not forced through one
session and the receiver's advertised `max_domains` is respected. A domain keeps
its shard across reconciles (adding a domain never re-authenticates the ones
already placed), and a new shard is opened only when every existing shard is
full and the shard limit allows. Single mode (the installation **Included** /
**Remote** receiver) always uses one session of unbounded domain capacity, since
it serves exactly one core.

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
configuration. Per-domain Dial MX is **legacy compatibility**, configured in the
domain's receiving wizard with its own per-domain key and `_mailmoose-mx` TXT
record. It is independent of the installation-wide **Direct MX receiver**
setting (Included or Remote), which is the shared receiver the direct-SMTP **MX**
provider uses; Dial MX is not a fourth global choice. The installation setting
also replaces the old core MX environment variables, which are now imported once
and then ignored.

A domain selecting the account-level **Remote MX** provider (`remotemx`) uses the
same single-mode session transport, but the core dials the account's own receiver
instead of a DNS-authenticated shared one. The core runs one such dialer per
account receiver that is in use; see [MX.md](MX.md).

The core defaults to a dedicated inbound webhook listener on `:8082` (configurable
with `DEDICATED_RECEIVER_ENABLE` and `DEDICATED_RECEIVER_PORT`); that
listener continues to accept the webhook connectors, so do not expose it to the
internet unless you also use those — Dial MX itself opens no inbound port on the
core. (The `auto` receiver choice is deferred: it is shown disabled in the UI and
rejected by the API.)

### Configuration

The receiver reuses the documented `MX_*` SMTP surface and adds only the
session listener and certificate settings:

| Variable | Default | Meaning |
|---|---|---|
| `DIALMX_MODE` | `single` | `single` for one bearer-authenticated core; `shared` for DNS-authenticated domains. |
| `DIALMX_CORE_KEY` | — | Required bearer key in single mode; unused in shared mode. |
| `DIALMX_BROWSER_REDIRECT_URL` | unset | Optional https landing page; a browser `GET /` is 302-redirected there. |
| `MX_HOSTNAME` | `localhost` | SMTP greeting hostname; independent of the session listener address. |
| `DIALMX_LISTEN_ADDR` | `:8443` | HTTPS/2 session listener. |
| `DIALMX_TLS_CERT` | — | Session certificate; optional in single mode, required in shared mode unless `DIALMX_TRUSTED_PROXIES` is set. |
| `DIALMX_TLS_KEY` | — | Session private key; set together with certificate. |
| `DIALMX_TRUSTED_PROXIES` | unset | Comma-separated IPs/CIDRs allowed to open a cleartext shared-mode session (the TLS-terminating proxy). Empty keeps shared mode TLS-only. |
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
| `MX_PER_IP_CONN_LIMIT` | `16` | Concurrent sessions per source IP; shared per proxy behind one. |
| `MX_PER_IP_CONN_WINDOW_MAX` | `128` | Sessions one source IP may open per minute. |
| `MX_PER_IP_AUTH_CONCURRENT` | `16` | Concurrent authentication jobs per source IP. |
| `MX_PER_IP_AUTH_WINDOW_MAX` | `256` | Authentication jobs one source IP may start per minute. |
| `MX_AUTH_TIMEOUT_SECONDS` | `10` | Bounds one DNS proof. |
| `MX_RESOLVE_TIMEOUT_SECONDS` | `10` | Bounds one recipient resolve. |
| `MX_INGEST_TIMEOUT_SECONDS` | `180` | Bounds one staged message handoff. |
| `MX_REVALIDATE_SECONDS` | `240` | Nominal binding renewal period. |

Select `DIALMX_MODE=shared` and set the session certificate paths explicitly
when following this shared-mode guide. SMTP STARTTLS is configured separately.
The non-root receiver uid must be able to read both certificate files. Mount
renewed certificates and restart the receiver to reload them.

To run shared mode behind a TLS-terminating reverse proxy instead, set
`DIALMX_TRUSTED_PROXIES` to the proxy's address (or range) and omit the session
certificate pair: the listener then serves cleartext HTTP/2 (prior knowledge)
and admits a session only from loopback or an allowlisted peer, rejecting any
other cleartext session with `426`. The core still dials the proxy over verified
`https`, so its TLS and the DNS-anchored domain proof are unchanged. The proxy
must forward **cleartext HTTP/2 to the upstream** (nginx `proxy_pass` cannot —
use Nginx Proxy Manager **Streams** with SSL, Caddy `transport http { versions
h2c }`, HAProxy `proto h2c`, or a similar L4 front). Keep the SMTP edge direct:
if the proxy also fronted `:25` the receiver would see the proxy's IP, breaking
SPF alignment and per-source limits. Behind a proxy every core arrives from the
proxy's IP, so the per-source caps (`MX_PER_IP_*`) are shared; raise them if one
proxy fronts many cores.

These are the **receiver container's** settings. When the core runs the receiver
itself (**Included** under Domain → Receiving → Direct MX), the same SMTP surface is instead
configured in the core's database through that panel — the hostname, limits,
verification, DNS resolver, timeouts and the STARTTLS certificate/private key —
and the core never reads the receiver env for them.

**Legacy `MX_TLS_CERT`/`MX_TLS_KEY` files are imported once and fail closed.**
On first start the core reads any legacy STARTTLS file paths into the persisted
Included configuration. If only one path is set, or a file cannot be read, the
one-time import is abandoned: no Included settings are written, the core keeps
running unconfigured, and the error is logged, so a deployment that intended to
require STARTTLS can never come up offering plaintext. Fix the paths (or set the
pair in the Direct MX receiver editor) and restart. Check `smtp_tls_key_configured` after a
successful import; `RequireTLS` is rejected at save without a certificate.

See `dialmx/.env.example` for a commented template.

## 3. Domain onboarding

1. In the core's Admin UI, open the domain and set **Receiving** to **Antler MX
   (Free SMTP Relay - no port forwards required)**, or to the Dial MX provider
   with a **custom** service.
2. For **Antler MX**, enter a **contact email** and save. The core resolves the
   hosted receiver set, generates the domain's exact key, and shows the MX
   records and the `_mailmoose-mx.<domain>` TXT record to publish. The email is
   operational metadata only: it is logged with the domain's usage so the
   service can see who is using it, and it can be shared or reused across
   domains. It is not authority and is not tied to an account.
3. For a **custom** service, enter the receiver's HTTPS base URL(s). Up to eight
   comma-separated base URLs are accepted; each must be an HTTPS origin with no
   path, userinfo, query or fragment. One domain may be served by several
   receivers.
4. The core generates a per-domain Ed25519 key pair and shows the matching
   public record. Publish it as a DNS TXT record:

   ```text
   _mailmoose-mx.<domain>  TXT  "v=MM1; k=ed25519; id=<key-id>; p=<base64 public key>"
   ```

   Every exact domain, including an inherited subdomain, owns a **separate key**
   and needs its own `_mailmoose-mx.<domain>` TXT proof. Receiver settings and
   enforcement may inherit; credentials never do. Regenerating the key requires
   replacing that domain's TXT record. Existing authorization lasts no longer
   than its current five-minute grant and fails at the next unsuccessful renewal.
5. Publish the domain's MX record(s) pointing at the advertised SMTP hostname
   shown by the receiver status; they may differ from the HTTPS endpoint
   hostname. An Antler MX setup shows the exact MX records to publish.
6. Choose `moderate` or `hard` authentication enforcement. The evidence
   semantics are identical to the MX edge (see [MX.md](MX.md)): auth failure is
   a durable **Spam** delivery, never an SMTP rejection.

The credential private seed is stored encrypted with `APP_ENCRYPTION_KEY`. The
public record is safe to publish and is shown in the UI.

### Antler MX endpoints

Antler MX is a predefined shared service. Its receiver set lives in a versioned
JSON manifest, compiled into the core and also fetched live from the project
repository at setup-save time:

```json
{
  "schema_version": 1,
  "receivers": [
    { "id": "antler-1", "session_url": "https://antler1.hgolabs.com", "smtp_hostname": "antler1.hgolabs.com", "mx_priority": 10 },
    { "id": "antler-2", "session_url": "https://antler2.hgolabs.com", "smtp_hostname": "antler2.hgolabs.com", "mx_priority": 20 }
  ]
}
```

The live copy is cached for ten minutes; a fetch failure or outage falls back to
the last known good copy and then to the embedded copy, so setup never depends
on the network. The decoder tolerates unknown fields so the hosted document can
grow, and validates known fields strictly (canonical HTTPS origins, public-only
destinations, DNS hostnames, bounded priority). The resolved set is
**snapshotted** into each domain's configuration, so adding or re-pointing
capacity only affects new setups; existing domains keep the receivers their MX
records already point at. Adding a receiver is preferred over re-pointing one,
and old receivers stay up.

### Setup traffic lights

The domain receiving API returns the live setup picture for a Dial MX domain:

- `status[]` — per-receiver authentication state learned over the core's
  outbound session (ready, rejected, connecting, …) with a bounded reason and
  expiry.
- `dns[]` — cached, best-effort published-record checks: the domain's MX
  hostnames against the expected receivers, and the `_mailmoose-mx` TXT record
  against the domain's exact key. `state` is `ok`, `pending` (not published yet)
  or `mismatch`. The MX check is `ok` as soon as any one expected receiver
  hostname is published and lists those in `matched` (the per-connector status
  reads it); an operator who points MX at a single receiver is not failed by the
  others in the advertised set. It is `mismatch` only when MX records exist but
  none belongs to a receiver. The TXT check stays exact.
- `instructions` — the copy-ready MX and TXT records for an Antler MX setup.

The checks are asynchronous and never block a save or a render. A green light
means the record matches, or the receiver holds an active authenticated domain
binding. Published MX records and receiver authentication are separate checks: a
domain can authenticate while its MX still points elsewhere, and a green
receiver session is not a public SMTP-port delivery test.

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

### Source-IP fairness and authentication pacing

The receiver's shared session listener is reachable by many cores, so it bounds
per source IP as well as in aggregate:

- a per-IP concurrent connection cap and a per-IP connection window (openings
  per minute) stop one source consuming every connection slot or cycling
  short-lived connections to evade the concurrent cap;
- a per-IP authentication window bounds how many domain challenges a source may
  start per minute, and a per-IP concurrent-auth cap bounds DNS work in flight,
  so a burst of `DomainAuth` frames cannot become unbounded resolver load;
- a short failure cooldown is applied during `AUTH`, not on connection open, so
  a legitimate sender opening many short connections is not punished for a
  single failed lookup.

The core paces its own `DomainAuth` frames: `MaxAuthInflight` bounds concurrent
authentications across every session (and is further clamped to a receiver's
advertised `max_auth_inflight`), so the core never floods a receiver even when
many domains are reconciled at once. Both sides bound the DNS work a burst can
start; neither relies on the other to be polite.

**Resource fairness is a deliberate V1 boundary.** A richer scheduling pass —
per-domain or per-recipient bandwidth/queue fairness across shards — was
described but is **not implemented**; the current guarantee is the bounded
per-IP and per-session caps above, not a global fair-share scheduler. Treat
queueing between domains on one receiver as best-effort.


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
core verifies the receiver's certificate and hostname. In a proxied shared
deployment the TLS terminates at the proxy and the receiver serves the same
request over cleartext HTTP/2 from the allowlisted proxy; the core still verifies
the proxy's certificate. Unknown
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

### Compatibility and versioning

`Ready` now carries three advisory fields the core uses to size its shards and
pace authentication: `max_domains` (distinct domains the receiver admits per
session), `max_auth_inflight` (concurrent domain authentications the receiver
will service per session) and `revalidate_seconds` (its nominal binding renewal
cadence). All three are **optional** in the JSON (`omitempty`) so an older
receiver that does not send them still decodes.

However, metadata is decoded **with unknown fields rejected**, so the change is
a **strict break in the other direction**: a receiver or core that starts sending
these fields to a peer built before they existed will have its `Ready`/metadata
frame rejected as malformed. The new `Ready` fields and the shard/pacing
behaviour therefore require a **coordinated core and receiver release** — do not
roll a new receiver into a deployment still running an older core (or vice
versa) and expect the session to come up. A mismatch fails closed as a session
error, not as silent misrouting, so it is safe but not transparent; upgrade both
sides together.

### Frame types

| Value | Type | Direction | Purpose |
|---|---|---|---|
| 1 | `Hello` | core → receiver | `{version, instance}`. |
| 2 | `Ready` | receiver → core | `{version, receiver_id, connection_id, smtp_hostname, max_message_bytes, max_domains?, max_auth_inflight?, revalidate_seconds?}`. |
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
- **Domains are sharded, not one-per-session.** In shared mode the core packs the
  configured domains into a bounded number of stable shards, each a session to
  the receiver, respecting the receiver's advertised `max_domains`. Domains on
  inherited configurations are served through the parent's receivers. Single mode
  uses one session of unbounded capacity. The shard set is bounded by a hard cap
  (`hardMaxShards`), so a very large domain set is divided across at most that
  many sessions rather than opening one per domain.
- **A replaced binding still serves pinned DATA** until it expires: new RCPT
  lookups go to the new binding; an already accepted transaction may finish on
  the old connection while its grant remains valid. Revocation/expiry aborts it.
- **No automatic core HA.** The last valid authentication wins for an exact
  domain on each receiver. This permits a deliberate takeover, not coordinated
  active-active cores or automatic failover.
- **No core credential rotation.** The core has no scheduled rotation of its
  session bearer credential on this release; a leaked key is rotated manually by
  reconfiguring the receiver. Binding renewal (§4) rotates domain *proofs*, not
  the core's bearer key.
- **No mTLS.** The receiver authenticates the core by the DNS-anchored challenge
  in §4 (or, in single mode, by the shared bearer key), not by client
  certificates. Session traffic is TLS-encrypted unless a trusted reverse proxy
  terminates TLS and the receiver serves cleartext from that allowlisted peer
  (`DIALMX_TRUSTED_PROXIES`, §2); the peer is
  identified by the signed challenge or bearer key, and DNS freshness is the
  shared-mode revocation mechanism. There is no per-core identity or certificate
  claim beyond that.
- **Local DNS cache TTL** delays observed revocation; see §4.
- **Operator-managed CA trust.** The core uses system roots for the legacy
  per-domain Dial MX dialer, optionally extended by a `DIALMX_CA_FILE` PEM bundle
  mounted in the core; hostname verification remains mandatory and there is no
  insecure TLS mode. For the installation **Remote** receiver the private CA is
  the non-secret `ca` setting in `/v1/admin/mx` (see [MX.md](MX.md)).
- **Resource fairness is bounded, not scheduled.** See the source-IP fairness
  section in §5: the guarantee is bounded per-IP and per-session caps, not a
  global fair-share scheduler between domains.

## Metadata logging

The receiver writes compact plain-text records to stderr in the same
`<time> [MX] <LEVEL> <message> key=value ...` format the core uses for its
`[Core]` lines, so the included edge and the core interleave readably on one
container stream. Normal lifecycle events are INFO; failures also retain
warning/error diagnostics. Message bodies, subjects, raw headers, TXT records,
challenge payloads, signatures and credentials are not logged. Envelope
addresses, IPs, domains and normalized authentication evidence are logged.

Verbosity is split by level. Process lifecycle, mail receipt, mail transfer,
the single per-attempt domain-auth summary and failures are INFO. The
per-connection and per-session transport chatter — `dialmx transport
accepted/closed`, `dialmx session opened/hello/closed`, `mx connection
opened/closed`, `mx session started`, `mx starttls established`, the reply-write
record and the per-step `dialmx domain proof` records — is DEBUG, hidden at the
default level. For a managed one-core receiver this chatter is pure noise, so it
stays off unless `DIALMX_LOG_LEVEL=debug` is set. In the embedded deployment the
core forwards that variable to the included child; the standalone receiver reads
it from its own environment.

A core enrolled through a named shared service (Antler MX) supplies
`contact_email` and `setup_id` on `DomainAuth`. Both are operational metadata,
never credentials: domain authority remains the DNS-anchored Ed25519 proof. A
malformed value is dropped rather than failing the proof. The receiver logs both
on the `dialmx domain auth` registration/renewal records and attaches them to
the per-recipient `dialmx resolve` and `dialmx handoff result` records, so usage
(contact, setup id, domains, messages) can be read or exported from the log
stream. They are omitted entirely for a custom receiver. Retention remains the
Docker log rotation described below; export before replacing a container.

### Correlation and events

- **Transport:** `dialmx transport accepted`, `tls failure`, `closed` record
  HTTPS peer/port, `transport_id`, duration and TLS handshake failure. The
  transport ID links to admitted sessions.
- **Core sessions:** `dialmx session rejected`, `opened`, `hello`, `closed`
  record protocol/TLS details, `core_connection_id`, advertised `core_label`,
  duration, close reason and counters. The label is caller-advertised, not a
  unique or authenticated core identity (the current core sends `gatehouse`).
  Domain/key bindings identify the authority proved on that connection.
- **Domain authority:** `dialmx domain auth` records one INFO summary per proof
  attempt (initial registration, renewal or revocation). Fields include domain,
  key ID, core session, terminal phase, outcome, reason, duration, grant expiry
  as applicable, and a compact `steps` string folding the per-step outcomes
  (`dns_lookup=ok,parse_key=ok,signature=ok,grant=ok`). DNS lookup success and
  signature validity remain distinct outcomes inside `steps`. The per-step
  `dialmx domain proof` records (lookup, key parse, signature, grant, replacement)
  carry the query name and resolver setting and are emitted at DEBUG only, so the
  default INFO stream stays one line per attempt. `system` denotes resolver
  configuration, not a known upstream resolver address.
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
  flags and duration, in a single record per message (there is no separate
  verification-completed line). Disabled verification differs from absent
  evidence. DKIM selector/algorithm are omitted because the current evaluator
  does not populate them.
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
  Abrupt process termination cannot emit terminal events; an unmatched start
  with no matching stop remains interrupted/unknown.

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
