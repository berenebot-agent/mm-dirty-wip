# Remote SMTP receiving

**Advanced users only. Not required for typical installations.**

This guide covers separate SMTP receivers that you operate yourself. For easy SMTP receiving configuration without running another receiver or opening inbound SMTP ports, use [Antler MX](EMAIL_PROVIDERS.md#inbound-antler-mx).

For the receiver included with Mammoth Mail, see [Direct MX](EMAIL_PROVIDERS.md#inbound-direct-mx).

| Option | When to use it |
| --- | --- |
| **Dial MX** | One public receiver you run for multiple independent domains or installations |
| **Remote MX** | A private receiver dedicated to one Mammoth Mail account |

Both options need an SMTP hostname reachable on **port 25**. Your Mammoth Mail core opens a connection **outbound** to the receiver; it does not need an incoming SMTP port. The receiver passes mail to the core, which stores it in `/data`.

## Dial MX: shared receiver (This is the mode the Antler MX service runs in)

Run a public receiver with an HTTPS session endpoint and a valid TLS certificate.

1. Point its SMTP and session hostnames at the receiver server.
2. Set the following environment for `dialmx/compose.yml` and put the certificate files in `dialmx/certs/`:

```dotenv
DIALMX_MODE=shared
MX_HOSTNAME=mx.example.com
DIALMX_TLS_CERT=/certs/fullchain.pem
DIALMX_TLS_KEY=/certs/privkey.pem
```

3. Start the receiver from the repository root:

```bash
docker compose -f dialmx/compose.yml up -d --build
```

4. For each domain, choose **Receiving → Dial MX** in Mammoth Mail, enter the receiver's HTTPS URL, and publish the MX and `_mammothmail-mx.<domain>` TXT records shown.

Each domain, including subdomains, needs its own TXT proof. You can configure multiple receiver URLs for redundancy. Keep public SMTP traffic directly reachable on port 25; a proxy that hides the sending server's IP can interfere with sender checks. A receiver URL that reaches the receiver through a reverse proxy has to meet the [proxy requirements](#reverse-proxies-in-front-of-the-receiver).

## Remote MX: one receiver for an account

Use `single` mode with a shared key. Generate one with `openssl rand -hex 32` and configure the remote receiver:

```dotenv
DIALMX_MODE=single
DIALMX_CORE_KEY=<generated-secret>
MX_HOSTNAME=mx.example.com
DIALMX_TLS_CERT=/certs/fullchain.pem
DIALMX_TLS_KEY=/certs/privkey.pem
```

Start the receiver with the same Compose command above. Publish its SMTP port 25 and make its session URL reachable from Mammoth Mail. Use a valid TLS certificate if the session crosses a public network; unencrypted HTTP/2 is only suitable on a network you control. If a reverse proxy sits in front of the receiver, it has to meet the [proxy requirements](#reverse-proxies-in-front-of-the-receiver).

In Mammoth Mail, configure the receiver's session URL and key under the account's **Remote MX** settings. Then choose **Remote MX** for each domain and point its MX records to your receiver's hostname. No per-domain TXT proof is needed in this mode. A receiver URL can belong to only one account.

**Direct MX** uses the SMTP receiver bundled with Mammoth Mail and delivers directly to the core. If you run a separate receiver yourself, configure it in **Remote MX** for one account (above), or use **Dial MX** for a shared public receiver.

## Reverse proxies in front of the receiver

Mammoth Mail opens the session to the receiver itself: a long-lived HTTP/2 `POST /mx/v1/session` that carries mail from the receiver to your core. The session endpoint speaks **HTTP/2 only** — h2 over TLS, or cleartext h2c on a network you control. Mammoth Mail never falls back to HTTP/1.1, and the receiver answers `426 HTTP/2 required` to an HTTP/1.1 request.

A reverse proxy in front of the receiver has to:

- **offer HTTP/2 to Mammoth Mail** (h2 through ALPN), or cleartext h2c on a network you control;
- **speak HTTP/2 to the receiver** as well. If the proxy terminates TLS it must re-encrypt to the receiver, or hand the decrypted stream to a receiver that accepts cleartext h2c;
- **stream the request body, never buffer it.** The session is one long-lived request, so a proxy that waits for the whole body before forwarding it forwards nothing at all, and the receiver never sees a session;
- **keep the connection open.** Mammoth Mail pings the receiver every 30 seconds and waits 5 seconds for the receiver to answer the session request before it calls the receiver unreachable, so a proxy idle timeout of a minute or more is enough.

Terminating TLS at layer 4 and passing the stream through untouched keeps HTTP/2 end to end. nginx cannot proxy upstream over HTTP/2 before version 1.29.4, so a layer-7 nginx in front of the session endpoint has to terminate TLS at layer 4 (a stream proxy) instead.

### Cleartext h2c and `DIALMX_TRUSTED_PROXIES`

A receiver serving cleartext h2c accepts a session only from a loopback peer or from an address listed in `DIALMX_TRUSTED_PROXIES` — a comma-separated list of IP addresses or CIDRs, where nothing wider than a /8 is accepted. In shared mode the receiver's TLS certificate is optional only when that list is set; in single mode a cleartext receiver is allowed by default, and `DIALMX_REQUIRE_TLS=true` holds it to the same rule. The shipped `dialmx/compose.yml` passes neither variable through, so add them to the receiver's `environment:` block yourself.

### Check the path before you send mail

```bash
curl --http2 -sS -o /dev/null -w '%{http_version}\n' https://mx.example.com/mx/v1/session
```

The command must print `2`. A `405` reply is expected and fine — the receiver serves the session on `POST` only — it proves the request reached the receiver over HTTP/2. Use `--http2-prior-knowledge` for a cleartext h2c receiver.

### What a broken path looks like

| What you see | What it means |
| --- | --- |
| Mammoth Mail reports the receiver **unreachable** and the receiver's log shows no session attempt at all | The proxy buffered the request body, or the connection never became HTTP/2. Mammoth Mail gives up after 5 seconds. |
| The receiver logs `client sent an HTTP request to an HTTPS server` | The proxy sent plain HTTP to the receiver's TLS port. |
| The receiver answers `426 HTTP/2 required` | The proxy downgraded the session to HTTP/1.1. |
| The domain's Receiving panel says **receiver speaks HTTP/1.1, not HTTP/2** | The proxy answered the session over HTTP/1.1. |
| The domain's Receiving panel says **receiver refused the session** | The receiver answered and declined it: check the shared core key (single mode) or the `_mammothmail-mx.<domain>` TXT proof (shared mode). |
| The domain's Receiving panel names an HTTP status, e.g. **receiver refused the session (HTTP 405)** or **receiver reported a server error (HTTP 502)** | Something answered at the receiver URL with that status. The core cannot tell the receiver from a proxy in front of it — both answer the same request — so read the proxy's log and the receiver's log for the detail. A `405` usually means the proxy does not forward the session `POST`; a `502` or `504` the proxy produced means it could not reach the receiver at all. |

The receiver's SMTP hostname and the session hostname do not have to be the same name — the receiver advertises the SMTP hostname while Mammoth Mail dials the session hostname — but using one name for both is the path that works.

## Check delivery

Check the receiving domain's DNS records and Mammoth Mail receiver status. For Dial MX, both the MX and domain TXT proof must match. For single-mode receivers, confirm the shared key and session URL.

Send a test email. If it does not arrive, check the receiver's container logs, the SMTP port and the domain's **Receiving** panel. Receiver containers do not store mailbox history; back up the Mammoth Mail core's `/data` as described in [Self-hosting](SELFHOSTING.md#data-and-backups).

If the receiver sits behind a reverse proxy, check the [proxy requirements](#reverse-proxies-in-front-of-the-receiver) first: a proxy that buffers the request body leaves the receiver unreachable, with no session attempt in the receiver's log.

## Antler MX restart and troubleshooting

[Antler MX](EMAIL_PROVIDERS.md#inbound-antler-mx) is the hosted shared receiver, so there is no receiver container on your host to restart. Mammoth Mail keeps an outbound session to the receivers in the domain's saved setup and reconnects without operator action.

### Reconnecting and authentication

Restarting Mammoth Mail ends the session; the new process redials the same saved receiver set. Reconnection retries indefinitely while Mammoth Mail runs, with jittered exponential backoff capped at 60 seconds, so there is no guaranteed recovery time after an outage — watch the status rather than waiting for a fixed interval.

The domain's **Receiving** panel reports the receiver status:

- **ready** — the receiver accepted this domain's key over a live session.
- **connecting**, **disconnected**, **deferred** or **unavailable** — Mammoth Mail is reconnecting, waiting out a short cooldown, or the receiver ended the binding. No restart is needed.
- **unreachable**, with a DNS or TLS reason — no session could be established. Check outbound HTTPS to the receiver, DNS resolution, and the host clock and certificate chain.
- **unreachable**, with no session attempt in the receiver's own log — a receiver of your own behind a reverse proxy is the likely cause: the proxy buffered the request body, or the connection never became HTTP/2. See [Reverse proxies in front of the receiver](#reverse-proxies-in-front-of-the-receiver).
- a status that names an HTTP code, e.g. **receiver refused the session (HTTP 405)** or **receiver reported a server error (HTTP 502)** — something answered at the receiver URL with that status. Read the proxy's log and the receiver's log for the detail; the core cannot tell the two apart.
- **rejected** — the receiver answered and refused the domain. The `_mammothmail-mx.<domain>` TXT value must be the key currently shown in the Receiving panel, and the MX records must point at the receiver. A receiver set is chosen from the Antler manifest when a setup is saved, and the domain keeps that snapshot; the manifest cache does not re-point or re-authenticate a live session, so changing it is not a repair step.

### Temporary SMTP failures and sender retries

There is no SMTP queue in Mammoth Mail or Antler to replay a message later. If Mammoth Mail is offline, the receiver cannot hand the message over and the sending server must retry according to its own policy. A 4xx reply is a temporary failure and may be retried; a 5xx reply is permanent for that attempt. If a test message does not arrive, check the sending server's queue and retry state first.

A `250` reply to `RCPT TO` only means the receiver accepted that recipient for the current transaction. It is not proof that the message was stored. Also check the final reply to `DATA`, and then Mammoth Mail's ingestion record: the arrival in the inbox (including Spam) and the domain's **Activity log**, which names the receiver as the source. `docker compose logs mammothmail` shows ingestion errors at the core.
