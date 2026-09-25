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

### The Mailgun inbound envelope sender is not provider-attested

**Status:** accepted risk — decision `D058` in `docs/DECISIONS.md`.

The Mailgun webhook HMAC covers only `timestamp + token`
(`internal/transport/mailgun/inbound.go`), so the `sender` form field is not
provider-attested. The approval workflow nevertheless uses that value as the
message's envelope sender, and the approval decision requires the envelope
sender to equal the nominated approver.

The consequence is that a caller holding valid Mailgun signing material — the
signing key, or a captured `timestamp`/`token`/`signature` triple replayable
within its 24-hour window — can set the approver address and obtain an approval.
Exploitation requires **both** of those signing values **and** the 128-bit
single-use approval token, so the practical bar is high.

**Why it is retained:** failing closed on an unattested envelope sender breaks
email-based draft approvals for every Mailgun deployment, which is a supported
and documented inbound provider. The maintainer judged the residual risk lower
than losing that capability.

**Eventual fix (out of scope until then):** carry a provenance flag on inbound
messages so an unattested envelope sender can never be consumed as attested, or
verify the approval reply's DKIM signature locally against the staged MIME
before acting on the sender's identity. Revisit only alongside an explicit
decision to change approval behaviour.

The **Resend** inbound adapter carries a genuinely attested envelope sender and is
**not** affected: its envelope sender is taken from the Svix-signed webhook payload,
so the signature covers the value the adapter reports.

The **Cloudflare** inbound adapter is **also affected**. It reads the envelope sender
verbatim from the `X-MailMoose-Envelope-From` request header; the shared bearer
authenticates the caller, not the value the caller asserts. An earlier revision of
this document described Cloudflare as "genuinely attested" — that was incorrect for
the approval-sender purpose. See retest finding A in
`the removed review report` and decision D058.

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

Bootstrap credentials (`INITIAL_ADMIN_EMAIL`, `INITIAL_ADMIN_PASSWORD`, and
their `*_FILE` variants) are read only to create the first administrator on an
empty database. They are never written to logs, error responses, diagnostic
bundles, admin pages, or API responses, and they are ignored once any user
exists, so they can be removed after first start. Prefer the `*_FILE` forms
with Docker secrets over a plain environment variable. Operator password resets
(`mailmoose admin reset-password`) revoke all browser sessions and record an
audit event without recording the password; API keys are revoked only on an
explicit `mailmoose admin revoke-api-keys`.
