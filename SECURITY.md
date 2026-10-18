# Security Policy

MailMoose handles untrusted email, credentials and tenant data, so we take
security reports seriously. Thank you for helping keep the project and its
operators safe.

## Reporting a vulnerability

Please report suspected vulnerabilities **privately** through GitHub's Private
Vulnerability Reporting:

<https://github.com/dellarb/mailmoose/security/advisories/new>

**Do not open a public issue or pull request for a suspected vulnerability.**
A public report exposes every deployment before a fix is available.

Include as much of the following as you can:

- affected version, tag, or commit;
- which component is affected (the application, or the MX edge);
- steps to reproduce, with a minimal request or input where possible;
- the impact you believe is possible;
- any proof-of-concept, logs, or a suggested fix.

## What is in scope

We are particularly interested in reports that affect:

- authentication, session handling, and API-key scoping;
- CSRF protection and cookie handling in the web UI;
- HTML sanitization and cross-site scripting in rendered email;
- outbound request handling and SSRF protections (`internal/netutil`);
- MIME parsing and attachment handling;
- encryption and handling of recoverable provider credentials
  (`APP_ENCRYPTION_KEY`);
- inbound deduplication, mailbox roles, and storage quota enforcement;
- the optional MX edge (SPF, DKIM, DMARC evaluation).

## What is out of scope

The following are generally not treated as vulnerabilities in this project:

- misconfiguration of a self-hosted deployment, including an exposed `.env`,
  a weak `APP_ENCRYPTION_KEY`, or an admin account left unprotected;
- issues in third-party transport providers or their APIs;
- volumetric denial of service, spam, or email deliverability complaints;
- social engineering.

If you are unsure whether something is in scope, report it privately and we
will help triage it.

## Known accepted risks

Accepted risks are design decisions that are deliberately retained. They are
**not** vulnerabilities to report — a finding matching one of these will be
closed as accepted, so please check here before spending effort on one.

### Webhook sender identity is not authenticated by the approval address checks

**Status:** accepted Low risk — decisions `D058`, `D059` and `D133` in `docs/DECISIONS.md`.

The approval workflow compares the reported envelope sender and MIME `From:`
with the nominated approver. These comparisons are not proof of sender identity:

- **Mailgun** — the webhook HMAC covers only `timestamp + token`
  (`internal/transport/mailgun/inbound.go`), so the `sender` form field is not
  provider-attested.
- **Cloudflare** — the envelope sender is read verbatim from the
  `X-MailMoose-Envelope-From` request header
  (`internal/transport/cloudflare/inbound.go`); the shared per-domain bearer
  authenticates the **caller**, not the **value** the caller asserts.
- **Postmark** — the envelope sender is read from the JSON `From` field
  (`internal/transport/postmark/inbound.go`); HTTP Basic authentication
  authenticates the caller holding the generated credentials, not the value it
  asserts. Postmark does not sign inbound webhooks.
- **Resend** — the adapter assigns Svix-signed webhook `data.from` to
  `EnvelopeFrom`. Signing a reported `From:` value authenticates Resend's report,
  not the original sender or an independent SMTP envelope. The two address checks
  may therefore compare the same attacker-asserted identity twice. An attacker
  does not need Resend's signing secret if Resend delivers their spoofed message.

**Exposure:** an attacker who can deliver a message satisfying the sender checks
still needs the **correct, live, 128-bit single-use approval token**. The token
permits a decision only on its associated pending request, whose reviewed content
is bound to the request. This is not demonstrated account takeover, general
mailbox access, or arbitrary editing of an approved message. Unauthorized approval
can nevertheless send the draft's existing content and attachments to its existing
recipients; unauthorized rejection can stop that request. It is not a guarantee
against data disclosure through the approved email.

**Why it is retained:** failing closed on an unattested envelope sender breaks
email-based draft approvals for the providers that pass one through, which are
supported and documented inbound providers. The maintainer judged the residual
risk lower than losing that capability.

**Eventual fix (out of scope until then):** carry a provenance flag on inbound
messages so an unattested envelope sender can never be consumed as attested, or
verify the approval reply's DKIM signature locally against the staged MIME
before acting on the sender's identity. Revisit only alongside an explicit
decision to change approval behaviour.

### Forwarded webhook envelope metadata is relay-supplied, not attested

**Status:** accepted risk — decision `D066` in `docs/DECISIONS.md`.

An outbound **forward** webhook carries the original transport envelope sender
and recipient in the `X-MailMoose-Envelope-From` / `X-MailMoose-Envelope-To`
headers alongside the unchanged raw MIME. The values are exactly what the
inbound transport reported, so their trustworthiness is the inbound transport's:
Cloudflare and Mailgun envelope senders are caller-asserted, and Resend's signed
reported From is not an authenticated SMTP-envelope identity. A receiver must treat these headers as **relay
metadata to record, not authority to act on**, unless it independently
establishes provenance. The headers are bounded and bounded-decoded, and a
missing sender is sent as an empty value so a receiver that requires one fails
closed rather than falling back to the spoofable MIME `From:` header.

**The general rule for reporters:** attestation is a property of *what a
signature covers*, never of a provider's name. A shared secret or bearer token
authenticates the caller; it says nothing about whether a value the caller
asserts is true. A new provider is attested only if its verified signature or
secret covers the sender value the adapter reports. Even a signed observation of
an address is not proof that the original sender controls that address.

### A managed approver inbox delegates token access to its readers

**Status:** intentional approval delivery — decision `D133`.

An approval notification addressed to a separate MailMoose-managed approver inbox
is ordinary incoming mail there. Its authorized readers can see the token, just
as readers could if that inbox were hosted by another provider. The requesting
inbox still consumes mail carrying an issued token rather than exposing it to its
readers. Random marker-shaped text is not a reason to discard ordinary mail.

Use a separate, restricted approver inbox. If the requesting agent can also read
the approver inbox, this is not an independent human-approval boundary. A shared
approver inbox delegates access to its approval requests to every authorized
reader, including its connected agents and forwarding connectors.

### Private outbound destinations are allowed by default for self-hosting

**Status:** intentional operator policy — decisions `D086` and `D133`.

`ALLOW_PRIVATE_OUTBOUND=true` is the default. Webhooks may therefore reach local,
private-network and link-local HTTP destinations, just as other outbound clients
may use private gateways. The operator accepts that reachability when allowing
users to configure destinations. Set `ALLOW_PRIVATE_OUTBOUND=false` to enable the
existing public-routable destination guard. This is not a webhook-specific
restriction, and it does not change the default. Webhooks do not follow redirects
and discard response bodies, but sending a request can still affect an internal
service even without reading its response.

### Standalone-inbox approval relies on the token plus the message From address

**Status:** accepted risk (decision `D097`; consistent with `D058`/`D059`).

A standalone inbox has no provider-signed inbound envelope. When an assistant
submits a draft for approval on a standalone inbox set to **MailMoose
approvals**, the approval email is delivered back through the inbox's own
connected mailbox and returned to MailMoose over a path whose envelope sender is
not attested. The approval decision therefore matches the **message `From`
address** directly against the nominated approver
(`Service.HandleRemoteApprovalControl` in `internal/app/control.go`), together
with the 128-bit single-use approval token.

The consequence is the same class of exposure as `D058`/`D059`: a caller who can
place mail in the connected mailbox can assert the approver's `From` address.
Exploitation still requires the 128-bit single-use token, so the practical bar
is high, and the approver identity is server-fixed (the attacker can only force
a decision on the specific draft whose token they hold).

**Why it is retained:** a standalone inbox cannot fail closed on envelope
attestation without losing email approvals entirely on that path, and the
mailbox credentials already gate who can place mail in the connected inbox.

**Eventual fix (out of scope until then):** the same provenance flag or local
DKIM verification proposed for `D058`/`D059`. Revisit only alongside an explicit
decision to change approval behaviour.

### Legacy ciphertext is rejected on read and the passphrase KDF uses a fixed salt

**Status:** accepted risk — decision `D117` in `docs/DECISIONS.md`, updated by
the status note at the end of that entry.

Provider credentials are encrypted with AES-GCM under `APP_ENCRYPTION_KEY`.
Every stored secret is bound to the identity of the row that owns it (an
additional-authenticated-data value), and a value without the `v2.` prefix is
**refused** by every read path. Only the current bound format is supported; the
temporary legacy-secret upgrade tool has been retired after both existing
instances were upgraded. No reader ever opens an unbound blob.

The fixed passphrase KDF salt remains an accepted risk:

- The passphrase fallback still stretches the key with PBKDF2 under a fixed
  application salt, so a database-write attacker who also knows the
  `APP_ENCRYPTION_KEY` passphrase can re-derive that key (the fixed salt is
  required because there is nowhere in the envelope to store a per-installation
  one).

A 32-byte `APP_ENCRYPTION_KEY` has no legacy derivation and is used directly.

## Supported versions

MailMoose is in public beta. Only the latest release and the current `main`
branch are supported; there is no backport line for older versions.

## What to expect

This is a small, best-effort project, so the below are targets rather than
guarantees:

- acknowledgement of your report within about 3 business days;
- an initial assessment and, where confirmed, a plan and rough timeline;
- coordinated disclosure, with credit if you would like it.

Please give us a reasonable opportunity to release a fix before publishing
details.

## Operators

If you run MailMoose, keep `APP_ENCRYPTION_KEY` and a filesystem-consistent
snapshot of `/data` backed up separately, as described in the README's backup
section. See `docs/DECISIONS.md` for the security model and the
decisions behind it.

The root encryption key is optional to supply. When `APP_ENCRYPTION_KEY` is
unset, MailMoose generates one into `<DATA_DIR>/encryption_key` (mode 0600) on
first start and logs the path. Setting your own key is what keeps the key out of
a `/data` backup, so a backup that leaks cannot decrypt stored provider
credentials; a generated key lives in `/data` and a `/data` backup is therefore
self-decrypting. Either way, changing the key on a deployment that already has
stored credentials leaves those credentials unreadable and they must be entered
again.

System-administrator credentials (`ADMIN_EMAIL`, `ADMIN_PASSWORD`, and their
`*_FILE` variants) are the source of truth for the installation's system
administrator login. When present they create that user on first start and
rotate its stored credentials (revoking its sessions) on later starts; when
absent the stored login is preserved. They are never written to logs, error
responses, diagnostic bundles, admin pages, or API responses. Prefer the
`*_FILE` forms with Docker secrets over a plain environment variable. Because
the configured secret owns this login, the account settings page blocks the
system administrator from changing its email or password and
`mailmoose admin reset-password` refuses it, directing the operator to the
configuration instead. Resets for ordinary users revoke all their browser
sessions and record an audit event without recording the password; API keys are
revoked only on an explicit `mailmoose admin revoke-api-keys`.

The system administrator may additionally register passkeys (WebAuthn). These
are **additive**: they are an independent sign-in method the configuration never
creates, rotates, or deletes, and the configured password always remains
available as a break-glass recovery path. Rotating `ADMIN_PASSWORD` revokes
existing sessions but never removes registered passkeys. The system
administrator cannot disable password sign-in — the "make this my only sign-in
method" option is hidden and refused for that account, and a config-driven
credential rotation always restores `password_auth_enabled`.

### Password hashing and passkeys

Human passwords are hashed with Argon2id (`m=65536`, `t=3`, `p=4`). Because
Argon2id commits 64 MiB per derivation, a process-wide admission gate admits at
most four concurrent hashes/verifications, bounding peak memory at 256 MiB. Only
an Argon2id encoding verifies: a pre-Argon2id `pbkdf2-sha256` row cannot log in
and needs a password reset (the break-glass `ADMIN_PASSWORD` path still works).
Machine tokens (API keys, session cookies) are 256-bit random values stored only
as SHA-256 hashes.

Passkeys use the `go-webauthn/webauthn` library with attestation explicitly not
requested and no extensions, so the heavy attestation/TPM code paths are never
exercised. A user cannot remove their last remaining sign-in method (the store
refuses to delete the only passkey of a password-disabled account).
