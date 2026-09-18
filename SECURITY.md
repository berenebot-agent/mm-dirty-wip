# Security Policy

Gatehouse Mail handles untrusted email, credentials and tenant data, so we take
security reports seriously. Thank you for helping keep the project and its
operators safe.

## Reporting a vulnerability

Please report suspected vulnerabilities **privately** through GitHub's Private
Vulnerability Reporting:

<https://github.com/dellarb/gatehouse-mail/security/advisories/new>

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

## Supported versions

Gatehouse Mail is at V1. Only the latest release and the current `master`
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

If you run Gatehouse Mail, keep `APP_ENCRYPTION_KEY` and a filesystem-consistent
snapshot of `/data` backed up separately, as described in the README's backup
section. See `docs/DECISIONS.md` for the security model and the
decisions behind it.

Bootstrap credentials (`INITIAL_ADMIN_EMAIL`, `INITIAL_ADMIN_PASSWORD`, and
their `*_FILE` variants) are read only to create the first administrator on an
empty database. They are never written to logs, error responses, diagnostic
bundles, admin pages, or API responses, and they are ignored once any user
exists, so they can be removed after first start. Prefer the `*_FILE` forms
with Docker secrets over a plain environment variable. Operator password resets
(`gatehouse admin reset-password`) revoke all browser sessions and record an
audit event without recording the password; API keys are revoked only on an
explicit `gatehouse admin revoke-api-keys`.
