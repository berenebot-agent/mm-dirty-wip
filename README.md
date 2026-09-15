# Gatehouse Mail

Open email infrastructure for AI agents: lightweight inbox identities, searchable message history, replayable realtime events, scoped mailbox roles, BYO outbound delivery, and Hermes Relay.

## Run

```bash
cp .env.example .env
# Set APP_ENCRYPTION_KEY and BASE_URL in .env
docker compose up -d
```

Put the service behind your HTTPS reverse proxy and open `BASE_URL` in a browser. The first visit creates the initial Admin account.

Persistent state is stored in `./data`.

The default Compose starts the embedded MX edge and publishes direct SMTP on
host port 25. If you receive mail only through a webhook provider (Mailgun,
Cloudflare Email Routing, Resend), set `MX_ENABLE=false` in `.env`; port 25 then
has no listener.

This is the minimal deployment. For a hardened stack (read-only root
filesystem, a /tmp tmpfs, dropped capabilities), use
`docker-compose.advanced.yml`:

```bash
docker compose -f docker-compose.advanced.yml up -d
```

## Mailgun inbound

1. Add and verify the receiving domain in Mailgun, including the MX records Mailgun provides.
2. In the Admin UI, open the domain's page (**Dashboard → Settings → the domain**) and configure a **Receiving** provider of type **Mailgun**, entering the account's webhook signing key. The secret is stored encrypted on that domain; it is no longer read from the environment.
3. Create a Mailgun catch-all route for the domain that forwards incoming mail to:

```text
https://your-host.example/internal/ingest/mailgun/raw-mime
```

Gatehouse Mail resolves the recipient to its logical inbox. A configured domain catch-all handles unmatched local parts. The `raw-mime` suffix is protocol-significant: it selects raw MIME delivery. The legacy `/internal/ingest/mailgun` alias has been removed.

## Cloudflare Email Routing inbound

Inbound can also be received via Cloudflare Email Routing through a Worker that streams the raw MIME to the generic webhook. Full dashboard navigation is in [docs/CLOUDFLARE_INBOUND.md](docs/CLOUDFLARE_INBOUND.md).

High-level steps:

1. On the dashboard, click **Receiving** for the domain and configure a **Receiving** provider of type **Cloudflare Worker**. Gatehouse generates the shared secret, stores it encrypted on that domain, and shows the complete Worker code and Cloudflare steps in a one-time dialog. The generated secret is shown once; use **Regenerate secret** in the same dialog if you lose it (this replaces it, and the old Worker stops working until you paste the new code).
2. In Cloudflare, create a Worker and paste the generated code (it already contains your ingest URL and secret), then deploy it.
3. In Cloudflare, enable **Email Routing** for your domain and follow the MX verification.
4. Under Email Routing -> **Routing rules**, add a **Send to a Worker** rule for each receiving address, choosing your Worker as the action.

Cloudflare hands each message to the Worker, which streams the raw MIME to:

```text
https://your-host.example/internal/ingest/cloudflare
```

The server resolves the recipient to its logical inbox with the same behavior as Mailgun.

## Resend inbound

Inbound can also be received via Resend. Resend posts a signed metadata webhook; Gatehouse verifies it and then fetches the raw MIME from the Resend API. Full setup is in [docs/RESEND.md](docs/RESEND.md).

The webhook URL to register in Resend is:

```text
https://your-host.example/internal/ingest/resend
```

1. In Resend, verify the domain (including the inbound MX record) and create a **full access** API key. A send-only key cannot read received mail.
2. On the dashboard, click **Receiving** for that domain and choose **Resend**. The form shows the exact webhook URL before you save. Create the Resend webhook for that URL subscribed to **`email.received`**, copy its signing secret (`whsec_...`), then enter it with the API key and save.
3. Each domain stores its own receiving configuration; if several domains share one Resend webhook, enter the same signing secret on each domain.

Resend also works as a sending provider (see below).

## Direct SMTP (MX) inbound

Instead of a webhook provider you can receive mail straight on port 25 with the
optional MX edge. One setting, `MX_ENABLE`, selects the mode. The server binary
defaults to `false` when the variable is unset; the shipped `docker-compose.yml`
sets it to `true`.

- **`true`** (default `docker-compose.yml`) — receive on port 25 with the edge
  embedded in the app container as a separate, unprivileged uid. The edge
  credential is generated automatically.
- **`false`** (server default) — no MX; receive via a webhook provider only. Set
  this in `.env` for a webhook-only deployment.
- **`remote`** — receive on port 25 with the edge in its own container/image
  (`gatehouse-mx`, via `docker-compose.mx-sidecar.yml`) or on another host. Needs
  a shared `MX_EDGE_SECRET`.

The edge holds no `/data` access and no `APP_ENCRYPTION_KEY`, and stages
messages in memory only.

### true (one container, Compose default)

```bash
# .env — MX_ENABLE=true is already the docker-compose.yml default
#MX_HOSTNAME=mail.example.com   # optional; defaults to gatehouse-mx
```

```bash
docker compose up -d --build
```

Then, in the Admin UI, open the domain and set **Receiving → MX**. Point the
domain's MX record at `MX_HOSTNAME` and publish SPF. The domain is only an MX
receiver once you set this in the UI; a domain left on a webhook provider is
unaffected.

`true` (embedded) requires the container to start as root (it must spawn the
edge under a different uid before dropping). A strict compose `user:` or
`cap_drop: [ALL]` disables that; startup then refuses with a clear error — use
`MX_ENABLE=remote` instead.
This is DAC + separate-uid isolation, not namespaces.

### remote (separate edge container/image, strongest isolation)

The edge runs from its own minimal image (`Dockerfile.mx`) in its own filesystem
and network namespace. Recommended when you can run two containers:

```bash
# .env
MX_EDGE_SECRET=<long random secret>   # generate: openssl rand -hex 32
docker compose -f docker-compose.mx-sidecar.yml up -d
```

The sidecar file sets `MX_ENABLE=remote` on the core and runs the `gatehouse-mx`
image for the edge. The edge does not boot as root and needs no writable
filesystem.

That is the whole required setup. Everything below is optional tuning.

### Advanced options (all optional — defaults shown)

You do not need to set any of these to run MX. They exist to tune limits or
enable TLS/resolver overrides.

Core service (`gatehouse-mail`):

| Variable | Default | Meaning |
|---|---|---|
| `MX_UID` / `MX_GID` | `65533` | Uid/gid the embedded edge runs as (must differ from the app's). |
| `MX_SIGNATURE_SKEW_SECONDS` | `600` | How old a signed edge request may be (replay window bound). |
| `MX_RECEIPT_RETENTION_HOURS` | `168` (7 days) | How long a delivery receipt deduplicates a sender retry, surviving message deletion. |
| `MX_EDGE_KEYS` | auto-generated (embedded) | Override the edge credential (`key_id:secret`, comma-separated for rotation). Required for `remote`. |
| `INBOUND_TLS_CERT_FILE` / `INBOUND_TLS_KEY_FILE` | empty | Optional TLS directly on the core's `:8082`; set both or neither. Usually unnecessary when a reverse proxy terminates TLS. |

Edge service (`gatehouse-mx`):

| Variable | Default | Meaning |
|---|---|---|
| `GATEHOUSE_INGEST_URL` | `http://127.0.0.1:8082` | Core URL (a public HTTPS proxy for a remote edge). |
| `MX_EDGE_NAME` | `mx-1` | Edge name shown in logs and signed metadata. |
| `MX_LISTEN_ADDR` | `:2525` | SMTP listener (unprivileged internally; publish host `25`). |
| `MX_TLS_CERT` / `MX_TLS_KEY` | empty | Optional STARTTLS; set both or neither. |
| `MX_VERIFY_SPF` / `MX_VERIFY_DKIM` / `MX_VERIFY_DMARC` | `true` | Which evidence classes the edge computes. |
| `MX_MAX_MESSAGE_BYTES` | `31457280` | Largest message the edge accepts. Advertised to senders as the ESMTP `SIZE` value at `EHLO` and echoed in the oversize `552` rejection. |
| `MX_STAGING_BYTES` | `268435456` (256 MiB) | Total in-memory staging across concurrent transactions; a burst above it returns a temporary failure. |
| `MX_MAX_RECIPIENTS` / `MX_MAX_CONNECTIONS` | `100` / `256` | Recipients per transaction and concurrent connections. |
| `MX_READ_TIMEOUT_SECONDS` / `MX_WRITE_TIMEOUT_SECONDS` / `MX_DATA_TIMEOUT_SECONDS` | `60` / `60` / `300` | Command, write and DATA read timeouts. |
| `MX_DNS_RESOLVER` / `MX_DNS_TIMEOUT_SECONDS` | system / `10` | Optional resolver `host:port` for SPF/DKIM/DMARC. |
| `MX_HEALTH_ADDR` | empty | Optional listener for `/healthz` and `/readyz`. |

Per-domain settings — enforcement mode (moderate/hard), catch-all — live in the
**Admin UI**, not in either environment. The edge is policy-free: at `RCPT` it
asks the core, and the core decides from the domain's receiving configuration.

Full details, the wire contract, authentication policy, Spam handling and retry
semantics are in [docs/MX.md](docs/MX.md).

### Remote edge

A remote edge is the same binary pointed at a public HTTPS endpoint. You do
**not** need cert files if a reverse proxy terminates TLS: the signature covers
the method and path only, not the host or scheme. Point `GATEHOUSE_INGEST_URL`
at the proxy (for example `https://inbound.example.com`) and forward to the
core's `:8082` **without rewriting the path**. Use `MX_TLS_CERT`/`MX_TLS_KEY`
only when the edge speaks directly to the core with no proxy. See
[docs/MX.md](docs/MX.md).

## Dedicated inbound listener

The server always listens on two ports:

- `LISTEN_ADDR` (default `:8081`) serves the API, web UI, Relay, and inbound webhooks.
- `:8082` is a dedicated listener that serves **only** the inbound webhook and MX
  routes (`/internal/ingest/mailgun/raw-mime`, `/internal/ingest/{provider}`,
  and, when `MX_ENABLE=true|remote`, `/internal/mx/resolve` and
  `/internal/mx/ingest`) plus `/healthz`.

To keep the API and UI off the public internet, expose only `:8082` to your
reverse proxy and keep `LISTEN_ADDR` bound to a private interface or blocked by
the firewall. Point provider webhook URLs at the dedicated host/port:

```text
https://inbound.example.com/internal/ingest/mailgun/raw-mime
https://inbound.example.com/internal/ingest/cloudflare
https://inbound.example.com/internal/ingest/resend
```

The ingest routes remain available on the main listener for backward
compatibility. The dedicated listener is plain HTTP like the main listener:
terminate TLS at the reverse proxy and do not expose the port directly to the
internet.

## Outbound

Sending is configured per domain. Open a domain's page, choose a **Sending** provider, and fill in the fields it asks for (for example Brevo only needs an API key). Each domain owns its own configuration; there is no account-level connector pool, no reusable named credential, and no assignment step, so mail can only leave through the provider configured on its domain. A domain with no sending provider queues mail until one is configured, and removing a provider pauses sending for that domain only.

The same operations are available through the Admin API, scoped to a domain:

```http
GET    /v1/admin/domains/{id}/sending
PUT    /v1/admin/domains/{id}/sending
DELETE /v1/admin/domains/{id}/sending
```

`PUT` accepts `{"provider":"...","config":{...}}`, and `GET /v1/admin/domains/{id}/sending/deliveries` lists the domain's send attempts newest first. The legacy `POST /v1/admin/outbound` endpoint, the account-level connector model, and the `/ui/outbound*` admin pages have been removed.

Mailgun configuration:

```json
{"api_key":"key-...","domain":"mg.example.com"}
```

Brevo configuration:

```json
{"api_key":"xkeysib-..."}
```

Brevo requires the inbox sender address to be a verified sender in Brevo.

Resend configuration:

```json
{"api_key":"re_..."}
```

The `from` domain must be a verified sending domain in Resend.

SMTP configuration:

```json
{"host":"smtp.example.com","port":587,"username":"user","password":"secret","security":"starttls"}
```

Send, reply and draft write requests may include base64-encoded attachments:

```json
{"filename":"quote.pdf","content_type":"application/pdf","content":"<base64>"}
```

Use the object above in an `attachments` array. The application translates attachments to each provider's native format and stores sent attachment metadata with the raw MIME message. Draft writes also accept an `action` of `draft`, `request-send` or `send`, so a draft can be created, attached and submitted for approval in one request.

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

From the Admin UI use **Create key → Hermes relay connection**, choose an inbox, and paste the generated `.env` block into the Hermes host's environment. The block includes `GATEWAY_RELAY_PLATFORMS=email`, which the gateway must advertise to match this connector's email descriptor, and `GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS=true`, which keeps any existing direct platform connections (such as Telegram) alive alongside the relay. Email delivered to that inbox is replayed over Hermes Relay and replies egress through the sending provider configured on the inbox's domain.

The one-time enrollment-token flow (`hermes gateway enroll` against `POST /relay/enroll`) remains available for hosted provisioning. Hermes Relay is isolated under `internal/hermesrelay` because the upstream contract is experimental.

## Upgrading

This release replaces account-level, named provider connectors with one optional
sending and one optional receiving configuration owned by each domain. It is a
single-install breaking change: the old connector APIs (`/v1/admin/outbound*`,
`/v1/admin/inbound*`), the domain credential-assignment fields, and the
standalone connector UI are removed.

Migration 013 runs automatically the first time the new binary opens the
database. It:

- copies each domain's assigned connector into a private per-domain config,
  preserving the encrypted bytes (a connector shared by several domains becomes
  independent copies, and no re-key is needed because `APP_ENCRYPTION_KEY` is
  unchanged);
- drops connectors not assigned to any domain;
- preserves mailbox, auth, message, storage, and account data;
- preserves delivery-log history, attributing each attempt to a domain where it
  can be derived from the attempt's message and inbox (attempts that cannot be
  attributed keep an unattributed entry instead of being dropped).

There is no full database wipe. Back up `/data` and `APP_ENCRYPTION_KEY` before
running the new binary. If startup reports a partially applied schema, restore
from backup and retry rather than deleting the database.

## Backup

Back up a filesystem-consistent snapshot of `./data` and separately retain `APP_ENCRYPTION_KEY`. Both are required to restore encrypted provider configurations.

## Development

Go is not installed on the host; all Go commands run via Docker through `./gatehouse-go.sh`:

```bash
./gatehouse-go.sh test -race -count=1 ./...
./gatehouse-go.sh vet ./...
./gatehouse-go.sh build -buildvcs=false ./cmd/server
```

Or use the tiered runner (CI parity): `./tests/run.sh`.

## Licence

Gatehouse Mail is licensed under the GNU Affero General Public License v3.0
(AGPL-3.0). See [LICENSE](LICENSE).

Third-party components and their licences are listed in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

Implementation decisions and acceptance criteria are in `docs/`.
