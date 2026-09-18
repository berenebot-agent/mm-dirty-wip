# Gatehouse Mail — V1 Acceptance Tests

## A. Account and authorization

1. Create account.
2. Create `hermes@example.com`.
3. Create `accounts@example.com`.
4. Create `travel@example.com`.
5. Create one API key with:
   - Owner on `hermes@example.com`
   - Assistant on `accounts@example.com`
   - Read on `travel@example.com`
6. Verify Owner can read, delete, draft, send, reply, and manage Hermes mailbox settings.
7. Verify Assistant can read, delete, and create/edit drafts for Accounts.
8. Verify Assistant cannot send/reply or administer the account.
9. Verify Read can only read/search/download attachments for Travel.
10. Create a fourth inbox with no assignment to the key and verify it is inaccessible.
11. Create an Admin key.
12. Verify Admin can create/delete inboxes, manage domains, keys, provider settings, Hermes connections, and all mailbox content.
13. Revoke a key and verify access ends.

## A2. Human UI security

1. Verify state-changing cookie-authenticated requests require a valid CSRF token.
2. Verify session cookies are HttpOnly, Secure when externally HTTPS, and SameSite.
3. Verify a fresh self-hosted instance creates the initial Admin from `INITIAL_ADMIN_EMAIL`/`INITIAL_ADMIN_PASSWORD`, ignores those values once a user exists, and leaves public registration closed by default.
4. Verify an HTML attachment downloads rather than executing inline in the authenticated application origin.

## A3. Draft approval workflow

1. As Assistant, create a draft and request send. Verify the draft becomes `pending_approval` and edits are rejected with `409`.
2. Verify a Read key cannot create drafts or request send.
3. Verify an Assistant key cannot approve or reject.
4. As Owner, reject with feedback. Verify the draft becomes `rejected`, feedback is stored, and editing returns it to `draft`.
5. As Owner, approve a resubmitted draft. Verify the exact frozen content is enqueued through the outbox, the draft is consumed, and `GET /v1/drafts/{id}/send-request` reports `approved` with the resulting message id.
6. Verify a second approval of the same request cannot send again.
7. Verify a direct `POST /v1/drafts/{id}/send` by an Owner resolves an outstanding request as `approved` with `decision_method=api`.
8. Verify the `draft.*` events appear in `GET /v1/events`.
9. Verify upload/list/delete of draft attachments, that uploads are frozen while pending, and that an approved send carries the attachments.
10. In the web UI, verify the dashboard unsent-drafts count, the inbox Draft send requests section with a quick Send action, and the review page Approve/Reject/Cancel actions.
11. With an inbox approver configured, request send and verify the request reports `notification_status=queued` before handoff and `sent` with a `token_expires_at` after the worker delivers the approval email. Verify the approval email creates no thread, consumes no account storage, and is absent from `/v1/messages` and `/v1/outbox`.
12. Verify that when the domain has no outbound provider, the approval notification is not marked `sent` and the draft is not presented as successfully awaiting approval.

## B. Inbound Mailgun delivery

1. Configure a Mailgun receiving provider on the domain and a catch-all inbound route to the Mailgun webhook endpoint.
2. Send a real external message to `hermes@example.com`.
3. Verify Mailgun receives an HTTP success only after durable local persistence.
4. Verify raw MIME exists under `/data/messages`.
5. Verify normalized sender/recipient/subject/body data.
6. Verify original envelope recipient is preserved.
7. Send to an unknown address with a configured domain catch-all and verify it reaches that inbox.
8. Remove the catch-all, send to an unknown address, and verify the provider receives a terminal `406` response and an audit entry is created.

## C. Idempotent inbound retry

1. Submit the same authenticated Mailgun delivery token twice.
2. Verify one logical message and one `message.received` event exist.
3. Verify a failed attempt that did not commit can be retried with the same token.

## D. Threading

1. Receive message A.
2. Send reply B.
3. Receive reply C referencing the thread.
4. Verify A/B/C share a stable thread.
5. Verify an unrelated message creates a separate thread.
6. Submit a message to another account/inbox with forged `In-Reply-To`/`References` values and verify it cannot join the original thread.

## E. Search

1. Store messages across several inboxes.
2. Search subject/body/sender/attachment filename.
3. Verify results respect key inbox scopes.

## F. Attachments

1. Receive a MIME message with PDF and image attachments.
2. List attachment metadata.
3. Download each attachment.
4. Verify bytes/hash match source fixtures.
5. Confirm HTML display remains sanitized.

## G. Replayable event history

1. Connect to SSE after cursor N.
2. Receive event N+1.
3. Disconnect.
4. Ingest events N+2 and N+3.
5. Reconnect after N+1.
6. Verify N+2 and N+3 replay in order.
7. Ingest N+4.
8. Verify N+4 arrives live on the same stream.

## H. Long poll

1. Call `/v1/events/wait`.
2. Ingest a new message before timeout.
3. Verify request returns immediately with the new event.

## I. BYO outbound

1. Configure Mailgun as the domain's sending provider.
2. Send external email.
3. Verify provider message ID is recorded.
4. Replace it with Brevo as the domain's sending provider.
5. Send external email and verify the Brevo provider message ID is recorded.
6. Configure generic SMTP as the sending provider on another test account/domain.
7. Send external email successfully.
8. By default (and always in hosted mode), configure an HTTP provider API base or SMTP host that is loopback/private/link-local and verify the connection is rejected before dialing; then set `ALLOW_PRIVATE_OUTBOUND=true` in self-hosted mode and verify a private gateway is accepted.
9. Send with a base64-JSON attachment and verify the attachment is delivered and stored, then downloadable from the sent message.

## J. Send idempotency

1. Submit `/v1/send` with idempotency key X.
2. Repeat identical request with X.
3. Verify only one provider delivery exists.
4. Verify both calls resolve to the same logical send result.

## K. Hermes Relay

1. Generate one-time enrollment token.
2. Run `hermes gateway enroll` on a Hermes host with outbound internet only.
3. Establish Relay connection.
4. Send external email to its assigned inbox.
5. Verify Hermes receives the message immediately through Relay.
6. Verify stable thread/session mapping.
7. Generate Hermes reply.
8. Verify reply reaches external sender using BYO outbound.
9. Disconnect Hermes.
10. Ingest another message.
11. Reconnect and verify ordered buffered/replayed delivery according to Relay semantics.

## L. Self-discovery

Give a fresh client only:

```text
base URL
API key
```

Verify it can discover:

- API base
- accessible inboxes
- message retrieval
- search
- events
- send/reply instructions
- Python/curl examples

## M. Backup and restore

1. Quiesce writes or take a filesystem-consistent snapshot.
2. Back up `/data` and the separately stored `APP_ENCRYPTION_KEY`.
3. Restore onto a clean instance.
4. Start application.
5. Verify accounts, inboxes, messages, search, event cursor history, and credentials/config required for recovery.

## N. Resource profile

Measure:

- idle RSS
- idle CPU
- RSS during max-size MIME ingest
- 1,000 idle SSE connections
- 1,000 idle Relay/SSE-equivalent connections as applicable
- sustained inbound fixture ingestion

Record results in release notes and investigate any unexpected growth.
