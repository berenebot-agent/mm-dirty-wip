# Security Policy

MailMoose handles untrusted email, credentials and tenant data, so we take
security reports seriously. Thank you for helping keep the project and its
operators safe.

## Reporting a vulnerability

Please report suspected vulnerabilities **privately** through GitHub's Private
Vulnerability Reporting:

<https://github.com/dellarb/mailmoose/security/advisories/new>

**Do not open a public issue or pull request for a suspected vulnerability.**
A public report exposes every self-hosted and hosted deployment before a fix is
available.

Include as much of the following as you can:

- affected version, tag, or commit;
- deployment mode (hosted service, self-hosted Docker, or the MX edge);
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

### The Mailgun and Cloudflare inbound envelope senders are not provider-attested

**Status:** accepted risk — decision `D058` in `docs/DECISIONS.md`.

Two inbound providers pass an envelope sender that the request's authentication
does not cover, and the approval workflow uses that value as the message's
envelope sender. The approval decision requires the envelope sender to equal the
nominated approver, so on both providers a caller who holds the transport
credential can assert the approver address:

- **Mailgun** — the webhook HMAC covers only `timestamp + token`
  (`internal/transport/mailgun/inbound.go`), so the `sender` form field is not
  provider-attested.
- **Cloudflare** — the envelope sender is read verbatim from the
  `X-MailMoose-Envelope-From` request header
  (`internal/transport/cloudflare/inbound.go`); the shared per-domain bearer
  authenticates the **caller**, not the **value** the caller asserts.

The consequence on either provider is that a caller holding valid transport
credentials — Mailgun signing material, or the domain's Cloudflare receiving
secret — can set the approver address and obtain an approval. Exploitation also
requires the 128-bit single-use approval token, so the practical bar is high.

**Not affected:** the **Resend** adapter takes its envelope sender from the
Svix-signed webhook payload, so the signature covers the value the adapter
reports. It is genuinely attested.

**Why it is retained:** failing closed on an unattested envelope sender breaks
email-based draft approvals for the providers that pass one through, which are
supported and documented inbound providers. The maintainer judged the residual
risk lower than losing that capability.

**Eventual fix (out of scope until then):** carry a provenance flag on inbound
messages so an unattested envelope sender can never be consumed as attested, or
verify the approval reply's DKIM signature locally against the staged MIME
before acting on the sender's identity. Revisit only alongside an explicit
decision to change approval behaviour.

**The general rule for reporters:** attestation is a property of *what a
signature covers*, never of a provider's name. A shared secret or bearer token
authenticates the caller; it says nothing about whether a value the caller
asserts is true. A new provider is attested only if its verified signature or
secret covers the sender value the adapter reports.

## Supported versions

MailMoose is at V1. Only the latest release and the current `main`
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
