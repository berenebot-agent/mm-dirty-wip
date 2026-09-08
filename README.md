# Gatehouse Email

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

Gatehouse Email resolves the recipient to its logical inbox. A configured domain catch-all handles unmatched local parts.

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

## Dedicated inbound listener

The server always listens on two ports:

- `LISTEN_ADDR` (default `:8081`) serves the API, web UI, Relay, and inbound webhooks.
- `:8082` is a dedicated listener that serves **only** the inbound webhook routes
  (`/internal/ingest/mailgun` and `/internal/ingest/{provider}`) plus `/healthz`.

To keep the API and UI off the public internet, expose only `:8082` to your
reverse proxy and keep `LISTEN_ADDR` bound to a private interface or blocked by
the firewall. Point provider webhook URLs at the dedicated host/port:

```text
https://inbound.example.com/internal/ingest/mailgun
https://inbound.example.com/internal/ingest/cloudflare
```

The ingest routes remain available on the main listener for backward
compatibility. The dedicated listener is plain HTTP like the main listener:
terminate TLS at the reverse proxy and do not expose the port directly to the
internet.

## Outbound

In the Admin UI, click **Add outbound provider**, pick a provider, and fill in the fields it asks for (for example Brevo only needs an API key). One configured provider is the **active** provider and is used for all sending; you can add more and switch the active one at any time.

The API still accepts a JSON `config` object via `POST /v1/admin/outbound`.

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
/.well-known/gatehouse
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

From the Admin UI use **Create key → Hermes relay connection**, choose an inbox, and paste the generated `.env` block into the Hermes host's environment. The block includes `GATEWAY_RELAY_PLATFORMS=email`, which the gateway must advertise to match this connector's email descriptor, and `GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS=true`, which keeps any existing direct platform connections (such as Telegram) alive alongside the relay. Email delivered to that inbox is replayed over Hermes Relay and replies egress through the inbox's configured outbound provider.

The one-time enrollment-token flow (`hermes gateway enroll` against `POST /relay/enroll`) remains available for hosted provisioning. Hermes Relay is isolated under `internal/hermesrelay` because the upstream contract is experimental.

## Backup

Back up a filesystem-consistent snapshot of `./data` and separately retain `APP_ENCRYPTION_KEY`. Both are required to restore encrypted provider credentials.

## Development

```bash
go test ./...
go run ./cmd/server
```

Implementation decisions and acceptance criteria are in `docs/`.
