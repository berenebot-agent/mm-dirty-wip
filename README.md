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

## Direct SMTP (MX) inbound — optional

Instead of a webhook provider you can receive mail straight on port 25 with the
optional `gatehouse-mx` edge, built into the same image. It speaks SMTP at the
edge and calls the core over signed HMAC endpoints, keeping routing, policy,
quota and storage in the core. The edge holds no `/data` mount and no
`APP_ENCRYPTION_KEY`.

1. Enable MX in the app environment and register an operator edge key:
   `MX_RECEIVE_ENABLED=true` and `MX_EDGE_KEYS=edge-1:<secret>`.
2. Run the edge (profile-gated sidecar):

   ```bash
   docker compose --profile mx up -d
   ```

   Give it `GATEHOUSE_INGEST_URL`, `MX_EDGE_KEY_ID`, `MX_EDGE_SECRET`,
   `MX_HOSTNAME` and (for STARTTLS) `MX_TLS_CERT`/`MX_TLS_KEY`.
3. Point the domain's MX record at the edge hostname and publish SPF.
4. In the Admin UI, set the domain's receiving provider to **Gatehouse MX (direct
   SMTP)** and choose an enforcement mode (moderate default, or hard).

Full details, the wire contract, authentication policy, Spam handling and retry
semantics are in [docs/MX.md](docs/MX.md).

## Dedicated inbound listener

The server always listens on two ports:

- `LISTEN_ADDR` (default `:8081`) serves the API, web UI, Relay, and inbound webhooks.
- `:8082` is a dedicated listener that serves **only** the inbound webhook and MX
  routes (`/internal/ingest/mailgun/raw-mime`, `/internal/ingest/{provider}`,
  and, when `MX_RECEIVE_ENABLED=true`, `/internal/mx/resolve` and
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
