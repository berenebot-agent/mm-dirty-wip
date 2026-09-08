# Open Agent Inbox

Open email infrastructure for AI agents: lightweight inbox identities, searchable message history, replayable realtime events, scoped mailbox roles, BYO outbound delivery, and Hermes Relay.

## Run

```bash
cp .env.example .env
# Set APP_ENCRYPTION_KEY and BASE_URL in .env
docker compose up -d
```

Put the service behind your HTTPS reverse proxy and open `BASE_URL` in a browser. The first visit creates the initial Admin account.

Persistent state is stored in `./data`.

## Mailgun inbound

1. Add and verify the receiving domain in Mailgun, including the MX records Mailgun provides.
2. Set `MAILGUN_SIGNING_KEY` to the account webhook signing key.
3. Create a Mailgun catch-all route for the domain that forwards incoming mail to:

```text
https://your-host.example/internal/ingest/mailgun
```

Open Agent Inbox resolves the recipient to its logical inbox. A configured domain catch-all handles unmatched local parts.

## Cloudflare Email Routing inbound

Inbound can also be received via Cloudflare Email Routing through a Worker that forwards messages to the generic webhook. Full dashboard navigation and the Worker example are in [docs/CLOUDFLARE_INBOUND.md](docs/CLOUDFLARE_INBOUND.md); the Worker source is [docs/cloudflare-worker.js](docs/cloudflare-worker.js).

High-level steps:

1. Set `CLOUDFLARE_WEBHOOK_SECRET` in `.env` to a long random secret.
2. Create a Worker from `docs/cloudflare-worker.js`, setting `WEBHOOK_URL` to your instance's `/internal/ingest/cloudflare` endpoint and `SECRET` to the same value.
3. In Cloudflare, enable **Email Routing** for your domain and follow the MX verification.
4. Under Email Routing -> **Routing rules**, add a **Send to a Worker** rule for each receiving address, choosing your Worker as the action.

Cloudflare hands each message to the Worker, which POSTs the raw MIME (base64) to:

```text
https://your-host.example/internal/ingest/cloudflare
```

The server resolves the recipient to its logical inbox with the same behavior as Mailgun.

## Outbound

Create a BYO provider in the Admin UI or API, then assign its credential ID to an inbox with `PATCH /v1/inboxes/{id}`.

Mailgun configuration:

```json
{"api_key":"key-...","domain":"mg.example.com"}
```

Brevo configuration:

```json
{"api_key":"xkeysib-..."}
```

Brevo requires the inbox sender address to be a verified sender in Brevo.

SMTP configuration:

```json
{"host":"smtp.example.com","port":587,"username":"user","password":"secret","security":"starttls"}
```

Send and reply requests may include base64-encoded attachments:

```json
{"filename":"quote.pdf","content_type":"application/pdf","content":"<base64>"}
```

Use the object above in an `attachments` array. The application translates attachments to each provider's native format and stores sent attachment metadata with the raw MIME message.

## API

Give an agent the base URL and an API key. Discovery starts at:

```text
/.well-known/agent-inbox
/agent
/v1/bootstrap
/openapi.json
```

Mailbox permissions are assigned per inbox:

- **Read** — messages, threads, search and attachments
- **Assistant** — Read plus delete and drafts
- **Owner** — Assistant plus send/reply and mailbox settings
- **Admin** — account-wide administration

## Hermes Relay

From the Admin UI choose an inbox and create a Hermes enrollment command, then run the displayed `hermes gateway enroll` command on the Hermes host. Email delivered to that inbox is replayed over Hermes Relay and replies egress through the inbox's configured outbound provider.

Hermes Relay is isolated under `internal/hermesrelay` because the upstream contract is experimental.

## Backup

Back up a filesystem-consistent snapshot of `./data` and separately retain `APP_ENCRYPTION_KEY`. Both are required to restore encrypted provider credentials.

## Development

```bash
go test ./...
go run ./cmd/server
```

Implementation decisions and acceptance criteria are in `docs/`.
