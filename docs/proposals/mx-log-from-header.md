# Plan: log the message's real From header on the MX relay

Status: **plan only — nothing changed in the repo.** `internal/mxagent/server.go` is untouched.

## Why

A message reached `yeehaw@cowboy.agent.xiat.net` from **`Hermes Agentbox <hermes_agentbox@agent.xiat.net>`**.
The dashboard showed the sender as `bounces-479916382-866966820@gx.d.sender-sib.com`. Both are true
statements about different things, and only one of them is what a human calls "the sender":

- `from=` on `mx message accepted` is the **`MAIL FROM` the sending service supplied** — for bulk
  senders that is a bounce/return-path address, not the From the recipient sees.
- The real From (`Hermes Agentbox <hermes_agentbox@agent.xiat.net>`) lives in the **`From:` header**,
  and **the relay logs no header values at all.** Grepping the whole `containers` stream for
  `Reply-To` / `Subject` / `From:` returns zero real matches.

Corroboration from the same transaction: the `auth_results` on that message passed **`agent.xiat.net`**
— the domain of the header From, not of the supplied address. The evidence that the header is the
truth was already in the logs; the value itself was not.

The parser needed already exists: `FromHeaderDomain(raw []byte)` at `internal/mxagent/server.go:898`
reads the `From:` header, unfolds continuation lines, and parses with `net/mail` (display names and
angle addresses included). It is called once, at line 565, and **only its domain is kept** for DMARC —
the address and display name are parsed and thrown away. The change keeps two more values that are
already being computed.

## What to log

A **new** line, sibling to `mx auth evidence`, on every message:

**Decided: log both the header address and the supplied address. No display name, no Reply-To.**

```
mx from header spf_enabled=true dkim_enabled=true dmarc_enabled=true
              from_address=hermes_agentbox@agent.xiat.net
              supplied_from=bounces-479916382-866966820@gx.d.sender-sib.com
              from_domain=agent.xiat.net
```

Field rules:

- `from_address` — the address from the message's own `From:` header. This is the "real from".
- `supplied_from` — the `MAIL FROM` the sending service handed over. Re-logged here alongside the
  header value so the two read as a pair on one line, without changing `mx message accepted`.
- `from_domain` — the From header's domain; already computed today for DMARC, now also logged.
- **No `from_display`, no `reply_to`.** They are the fields that carry personal data beyond the
  address (a display name is often a person's name), and the dashboard needs neither: the address
  plus the supplied address answers "who really sent this, and what did the service claim".
- No truncation; `MaxLineLength` is already 2000.

### Why a new line and not more fields on `mx message accepted`

- It leaves an existing line's shape unchanged, so nothing already parsing it breaks.
- The values are **attacker-controlled**. Keeping them off the SMTP-decision lines means a hostile
  header can never sit on a line that reads as trusted delivery state.

### Privacy

Logging the From **address** is a small step beyond today's `from_domain`, and it is what makes the
dashboard truthful: without it there is no way to tell the real sender from the supplied one. The
display name and Reply-To are deliberately **not** logged — they carry more personal data (a display
name is frequently a person's name) and buy the dashboard nothing it needs. Stored in OpenObserve
for the stream retention period (currently 30 days on `containers`), as with all other log data.

## Implementation steps

All in `internal/mxagent/server.go`:

1. **Generalise the header scan.** Add a sibling to `FromHeaderDomain` that returns any header's
   unfolded value, reusing the same bounded scan (header block only; CRLF and LF; continuation lines
   unfolded):

   ```go
   // headerValue returns the unfolded value of the first occurrence of the named
   // header, or "" when absent. Header block only, same bounded scan as
   // FromHeaderDomain.
   func headerValue(raw []byte, name string) string
   ```

   Then reduce `FromHeaderDomain` to `addressDomain(headerValue(raw, "From"))` so there is a single
   implementation rather than two copies of the unfold/scan logic.

2. **Extract just the address from the From header.** Reuse the same `net/mail` handling that
   `addressDomain` already relies on:

   ```go
   // addressFrom returns the addr-spec from a header value, or "" when it cannot
   // be parsed. Uses net/mail, so display names and angle addresses are handled.
   func addressFrom(v string) string
   ```

   No display-name extraction is needed, so this is the smallest useful addition — and
   `addressDomain` can be expressed in terms of it. The only remaining caller of a bare-domain
   helper is the DMARC path, which keeps its current behaviour.

3. **Capture the values** where the message is already in hand, next to existing line 565:

   ```go
   fromHeader := headerValue(raw, "From")
   fromAddress := addressFrom(fromHeader)
   fromDomain := addressDomain(fromHeader)   // replaces the FromHeaderDomain call
   ```

   `fromDomain` must keep feeding `s.srv.verify.Verify(...)` unchanged — DMARC evaluation must not
   change behaviour in this change.

4. **Emit the line** immediately after `s.logAuthEvidence(txID, auth, time.Since(verifyStart))`
   (line 570), via a new method styled like the existing ones:

   ```go
   func (s *session) logFromHeader(txID, fromAddress, suppliedFrom, fromDomain string)
   ```

   using `s.srv.log.Info("mx from header", …)`. `suppliedFrom` is `s.from`, the value already bound
   at line 584 and already used on `mx message accepted` — no new state, just logged here too.

## Tests

`tests/unit/dialmx/receiver/logging_test.go` already pins log line shapes — extend it there:

- **The real case:** `From: Hermes Agentbox <hermes_agentbox@agent.xiat.net>` with
  `MAIL FROM:<bounces-479916382-866966820@gx.d.sender-sib.com>` →
  `from_address=hermes_agentbox@agent.xiat.net`,
  `supplied_from=bounces-479916382-866966820@gx.d.sender-sib.com`,
  `from_domain=agent.xiat.net`. This is the exact message that prompted the change, so it belongs in
  the suite as the primary assertion.
- Quoted display name containing a comma and spaces (`"Cowboy, Yee-Haw" <hello@example.com>`) → the
  **address** is extracted correctly (`hello@example.com`); the display name must not leak into any
  field.
- Bare address, no display (`From: hello@example.com`) → `from_address=hello@example.com`.
- No `From:` header at all → `from_address` empty, line still emitted, no panic.
- Malformed `From:` (`From: <<<>>>`) → no panic, address empty, line still emitted — the existing
  `addressDomain` fallback already tolerates this shape.
- Assert **no display name or Reply-To value appears anywhere** in the emitted line, even when both
  are present in the message.
- **Regression assertion:** `mx message accepted` output is byte-for-byte unchanged by this change.

## Verification before the dashboard depends on it

1. `go test ./tests/unit/dialmx/...` green.
2. Send one real message through a relay, then confirm the new line lands in OpenObserve:
   `oo.py grep "mx from header" 5`.
3. Only then add the dashboard columns.

## Dashboard changes once it ships (not part of this plan)

- `from_address` column pulled off the new line, grouped alongside `supplied_from` so the real from
  and the supplied address are visible together. Both are already present in the mail log as
  `supplied_from` + `auth_domain`; the header line adds the missing piece.
- Keep `auth_domain` — it was the only trustworthy signal before this change and remains a useful
  cross-check against the header.

## Repo hygiene

`git status` in `~/projects/mailmoose/repo` shows `internal/app/service.go` and
`tests/unit/app/domain_outbound_test.go` **already modified** — not by this work. Leave them alone;
stage only the files this change touches.
