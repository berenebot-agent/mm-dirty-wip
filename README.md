<div align="center">

<img src="assets/mailmoose-logo-horizontal-1200x400.png" alt="MailMoose" width="600">

# MailMoose

**Open email infrastructure for AI agents.**

Unlimited logical inbox identities, realtime replayable events, BYO outbound delivery — self-hosted in one container.

[![ci](https://github.com/dellarb/mailmoose/actions/workflows/ci.yml/badge.svg)](https://github.com/dellarb/mailmoose/actions/workflows/ci.yml)
[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL--3.0-blue.svg)](LICENSE)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](CONTRIBUTING.md)

</div>

> [!WARNING]
> **MailMoose is in active development and is not yet production-ready.**
> APIs, configuration, and on-disk formats may change without notice before
> the first stable release. Kick the tyres, break things, and tell us what's
> missing — but don't point it at anything you depend on yet.

---

## The 5-Second Hook

Your agents need email. Not an API wrapper around someone else's inbox — real
addresses that receive mail, hold history, and send through providers you
control.

Today the options are bad: SaaS mail APIs give you one shared inbox per
provider and lock your agent's identity to their platform. DIY Postfix gives
you an ops project, not a feature.

MailMoose gives every agent its own address — `research@yourdomain.com`,
`task-8291@yourdomain.com`, as many as you want — with full-text search,
durable event streams, scoped API keys, and sending through Mailgun, Brevo,
Resend, Direct MX, or any SMTP relay you already pay for. Self-host the
whole thing in a single container.

## Why MailMoose vs Alternatives

| | **MailMoose** | SaaS mail APIs | DIY Postfix + IMAP |
|---|---|---|---|
| **Speed** | Events delivered in-process the moment the webhook lands; agents catch up after downtime by replaying a cursor | Webhook per provider, each with its own retry/dedup semantics to babysit | Poll-only unless you build IMAP IDLE handling yourself |
| **Developer experience** | One bearer key, agent self-discovery (`/.well-known`, `/agent`, OpenAPI), scoped roles per inbox | One SDK per provider; permission models built for humans, not agents | You own TLS, spam, auth, parsing, and a stateful daemon forever |
| **Footprint** | 1 binary · 1 process · 1 SQLite DB · 1 `./data` directory | Zero (your data lives on their servers) | Multiple daemons, mail queues, and config files to back up and secure |

## Quickstart

Running in under two minutes — no clone, no build:

**1. Create a folder** with `docker-compose.yml`:

```yaml
services:
  mailmoose:
    image: ghcr.io/dellarb/mailmoose:latest
    restart: unless-stopped
    env_file: .env
    volumes:
      - ./data:/data        # all persistent state lives here
    ports:
      - "8081:8081"         # API + web UI (webhook ingest also works here)
                            # expose publicly only behind an HTTPS reverse proxy
```

**2. Create** `.env`:

```bash
# Generate the key separately — Compose does NOT run shell commands inside .env,
# so the value must already be generated before you paste it in:
openssl rand -hex 32
```

Paste that output as the value of `APP_ENCRYPTION_KEY` (back it up — it decrypts
provider secrets), alongside the rest:

```bash
APP_ENCRYPTION_KEY=<paste the 64-character hex value from above>
BASE_URL=https://mail.example.com            # where you'll reach this instance
ADMIN_EMAIL=admin@example.com
ADMIN_PASSWORD=a-long-random-password
```

**3. Run it:**

```bash
docker compose up -d
```

Open `BASE_URL`, sign in with the system administrator credentials, and add your domain. From the **Admin** page you can invite other people, each as their own separate account. Then:

- Choose how to **receive email** → options below
- Configure **outbound providers** → [docs/PROVIDERS.md](docs/PROVIDERS.md)
- Connect an **agent or service to an inbox** (Hermes Relay, OpenClaw, webhook, API key) → [docs/CONNECTORS.md](docs/CONNECTORS.md)
- Add a **standalone inbox** that reads and sends through an existing mailbox over IMAP/SMTP, alongside your managed-domain inboxes → inbox settings (**Add standalone inbox**), or [standalone provider setup](docs/PROVIDERS.md#standalone-mailbox-providers)
- Direct MX uses the bundled receiver; publish host port 25 (`25:2525`) and select it under **Domain → Receiving → Direct MX**. For remote receivers → [docs/REMOTE_MX.md](docs/REMOTE_MX.md); for deployment hardening → [docs/SELFHOSTING.md](docs/SELFHOSTING.md)
- Build from source → the repo's [`docker-compose.yml`](docker-compose.yml) + [CONTRIBUTING.md](CONTRIBUTING.md)

## Ways to receive email

Choose a receiving option for each domain. Antler MX is the easiest way to get
direct SMTP receiving without opening an inbound port; the other options let
you bring an existing provider or run your own receiver.

- **[Antler MX — free hosted relay](docs/REMOTE_MX.md#antler-mx).** MailMoose connects outward to the shared relay, so there’s no SMTP port to open and no receiver to install or maintain. Publish the MX and domain-specific TXT records MailMoose provides. The TXT proof authorizes the relay for your domain, and the relay also verifies its hostname is listed in your MX records before accepting mail. Antler MX delivers to your MailMoose instance; it doesn’t host a mailbox or retain delivered messages.
- **Provider integrations.** Receive through Mailgun, Cloudflare Email Routing, Resend, or Postmark, using each provider’s inbound webhook integration with MailMoose. See [inbound provider setup](docs/PROVIDERS.md).
- **Your own SMTP receiver.** Use **Direct MX** with the included receiver or a separate receiver, **Remote MX** with an account-owned receiver, or **Dial MX** with a receiver you operate. These options give you control over the SMTP edge; a receiver still needs to be reachable by sending mail servers on port 25. See [Remote SMTP receiving](docs/REMOTE_MX.md).
- **An existing mailbox.** Connect an inbox you already use over IMAP/SMTP with a [standalone inbox](docs/PROVIDERS.md#standalone-mailbox-providers). This connects to that mailbox rather than receiving mail for a managed domain.

## Key Features

- **Give every agent its own identity.** Inboxes are lightweight database rows — spin up per-role, per-project, or per-task addresses instantly, with aliases, catch-all routing, and API keys scoped to exactly the mailboxes each agent should touch (`read` / `assistant` / `owner`). A **standalone** inbox can instead connect to an existing mailbox you already own over IMAP/SMTP, so MailMoose fronts a mailbox it does not host.
- **Never miss a message, never process one twice.** Every event is a durable SQLite row with a cursor; agents reconnect after a crash and replay the backlog, and inbound webhooks are deduplicated on the provider's authenticated delivery token.
- **Ship mail through providers you already trust.** Bring your own Mailgun, Brevo, Resend, or SMTP credentials — configured per domain, encrypted at rest, with the full delivery log visible for every attempt.
- **Own the whole stack without operating it.** One Go binary, one SQLite database, raw MIME on disk under `./data` — backup is copying one directory plus one key.

## API

Give an agent the origin (`BASE_URL`) and an API key — that's the whole onboarding. Keys are sent as `Authorization: Bearer mmm_...`, and a capable agent can discover everything else itself, starting from:

```text
$BASE/.well-known/mailmoose   # discovery document
$BASE/agent                   # markdown agent guide
$BASE/v1/bootstrap            # key-specific capabilities
$BASE/openapi.json            # full OpenAPI spec
```

Permissions are assigned **per inbox**, so one key can hold different roles on different mailboxes:

| Role | Access |
|---|---|
| `read` | messages, threads, search, attachments |
| `assistant` | read + delete + drafts |
| `owner` | assistant + send/reply + mailbox settings |
| `admin` | account-wide administration |

## Hermes Relay

For [Hermes](https://github.com/NousResearch/hermes) agents, no polling code is needed: in the Admin UI, **Create key → Hermes relay connection**, choose an inbox, and paste the generated `.env` block into the Hermes host. Email delivered to that inbox is replayed to the agent over an authenticated WebSocket, and replies egress through the sending provider configured on the inbox's domain.

Full setup is in [docs/CONNECTORS.md](docs/CONNECTORS.md).

For setup instructions for Hermes, OpenClaw, webhooks, and API keys, see
[docs/CONNECTORS.md](docs/CONNECTORS.md).

## Contributing

MailMoose is V1 with a deliberately small scope, and that's an invitation: the
fastest way to shape it is to use it and tell us what's missing.

- ⭐ **Star the repo** if agent-native email is a problem you care about.
- 🐛 **Open issues** for bugs and API gaps.
- 🔧 **Send PRs** — see [CONTRIBUTING.md](CONTRIBUTING.md) for the Docker-based
  toolchain, test expectations, and the formatting gate.
- 🔒 **Security issues**: never open a public issue — follow
  [SECURITY.md](SECURITY.md).

## Documentation

| Doc | Contents |
|---|---|
| [docs/PROVIDERS.md](docs/PROVIDERS.md) | Inbound, outbound, and standalone mailbox providers |
| [docs/CONNECTORS.md](docs/CONNECTORS.md) | Inbox-level connectors: Hermes Relay, OpenClaw, webhooks and API keys |
| [docs/SELFHOSTING.md](docs/SELFHOSTING.md) | First-run admin, deployment, backup, and upgrades |
| [docs/REMOTE_MX.md](docs/REMOTE_MX.md) | Antler MX, shared Dial MX and private single-mode receivers |
| [docs/API.md](docs/API.md) | API contract and generated endpoint reference |

## License

MailMoose is licensed under the GNU Affero General Public License v3.0
([AGPL-3.0](LICENSE)). Third-party components and their licences are listed in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
