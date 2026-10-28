package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/idgen"
	"github.com/dellarb/mailmoose/internal/limits"
	"github.com/dellarb/mailmoose/internal/model"
)

// ErrClaimTaken reports that the claim under which an outcome is being recorded
// no longer owns the job: another attempt re-claimed it after the lease lapsed
// mid-send. The caller must not record its outcome (the job belongs to the new
// attempt now) and the send that produced the outcome must be treated as
// ambiguous.
var ErrClaimTaken = errors.New("outbox claim taken over by another attempt")

// ErrUncertainDelivery means operator confirmation is required before retrying.
var ErrUncertainDelivery = errors.New("delivery uncertain: acknowledge possible duplicate delivery before retry")

// quarantinedReasonPrefix marks a terminal failure that a quarantined send was
// resolved with: the provider may already have accepted the message, so a later
// manual retry is only offered with an explicit duplicate warning. It is a plain
// message rather than an exported state because the durable "was this ever
// attempted" fact is the send_attempt_state marker; the prefix survives the
// marker's own clearing so an operator retry can still be warned.
const quarantinedReasonPrefix = "uncertain delivery: "

// IsUncertainDelivery identifies the durable warning exposed by Outbox reads.
func IsUncertainDelivery(lastError string) bool {
	return strings.HasPrefix(lastError, quarantinedReasonPrefix)
}

// claimTaken reports whether the stored claim owner invalidates the caller's
// claim. An exact match, or an empty owner (the claim was released without a
// replacement attempt taking over, for example by a requeue or a startup
// recovery), still allows the outcome through. Any other non-empty owner means
// a live re-claim and blocks the stale attempt's outcome.
func claimTaken(owner, claim string) bool {
	return ClaimTaken(owner, claim)
}

// ClaimTaken reports whether a stored (non-empty) claim owner invalidates the
// caller's claim token. It is the shared predicate between the store's outcome
// guards and the app layer's pre-send ownership verification.
func ClaimTaken(owner, claim string) bool {
	return owner != "" && (claim == "" || owner != claim)
}

// sendClaimClause is the ownership predicate shared by the pre-send marker
// write and the outcome writes. An exact match, or an empty stored owner (the
// claim was released without a replacement attempt), is owned by the caller;
// any other non-empty owner means a live re-claim. It mirrors ClaimTaken.
const sendClaimClause = `(claim_owner=? OR claim_owner='')`

// HoldPending records why a pending message is not being delivered and defers
// its next attempt without counting a retry. It is used when a domain has no
// outbound provider yet: the message stays queued and delivers once a provider
// is assigned. claim guards against a stale attempt holding an expired claim;
// an empty token can update only an unclaimed message, never bypass a live claim.
// A message carrying a durable pre-send marker is never held: its claim is left
// attached so ownership is not silently dropped on a row whose provider outcome
// is unknown (it waits for operator review instead).
func (s *Store) HoldPending(ctx context.Context, accountID, id, reason string, next time.Time, claim string) error {
	res, err := s.write.ExecContext(ctx, `UPDATE messages SET last_error=?,next_attempt_at=?,claim_owner='',claim_expires_at='' WHERE id=? AND account_id=? AND status='pending' AND send_attempt_state='' AND `+sendClaimClause, reason, timeText(next), id, accountID, claim)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CommitOutbound(ctx context.Context, r OutboundRecord) (model.Message, model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.Message{}, model.Event{}, err
	}
	defer tx.Rollback()
	// Consume the draft first so its freed bytes count against the quota check
	// and the draft can never be double-counted alongside the sent message.
	if r.DraftID != "" {
		d, err := getDraftTx(ctx, tx, r.Inbox.AccountID, r.DraftID)
		if err != nil {
			return model.Message{}, model.Event{}, err
		}
		// A request may have frozen the draft since the sender last read it.
		if d.Status == model.DraftStatusPendingApproval && r.SendRequestID == "" {
			return model.Message{}, model.Event{}, ErrConflict
		}
		_, attTotal, err := draftAttachmentPathsTx(ctx, tx, r.DraftID)
		if err != nil {
			return model.Message{}, model.Event{}, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM drafts WHERE id=? AND account_id=?`, r.DraftID, r.Inbox.AccountID); err != nil {
			return model.Message{}, model.Event{}, err
		}
		if err = adjustStorageTx(ctx, tx, r.Inbox.AccountID, d.InboxID, -(draftBodyBytes(d) + attTotal)); err != nil {
			return model.Message{}, model.Event{}, err
		}
	}
	var quota, used int64
	if err = tx.QueryRowContext(ctx, `SELECT storage_quota_bytes,storage_used_bytes FROM accounts WHERE id=?`, r.Inbox.AccountID).Scan(&quota, &used); err != nil {
		return model.Message{}, model.Event{}, err
	}
	if quota > 0 && used+r.SizeBytes > quota {
		return model.Message{}, model.Event{}, ErrQuota
	}
	// Per-inbox cap, layered on the account cap. inboxUsedTx initializes a
	// NULL counter from a one-time SUM before comparing.
	inboxUsed, err := inboxUsedTx(ctx, tx, r.Inbox.AccountID, r.Inbox.ID)
	if err != nil {
		return model.Message{}, model.Event{}, err
	}
	var inboxQuota sql.NullInt64
	if err = tx.QueryRowContext(ctx, `SELECT storage_quota_bytes FROM inboxes WHERE id=? AND account_id=?`, r.Inbox.ID, r.Inbox.AccountID).Scan(&inboxQuota); err != nil {
		return model.Message{}, model.Event{}, err
	}
	if inboxQuota.Valid && inboxQuota.Int64 > 0 && inboxUsed+r.SizeBytes > inboxQuota.Int64 {
		return model.Message{}, model.Event{}, ErrQuota
	}
	draftEvent := model.Event{}
	newReqID := ""
	if r.NewSendRequest != nil {
		created, cerr := createSendRequestTx(ctx, tx, r.Inbox.AccountID, *r.NewSendRequest)
		if cerr != nil {
			return model.Message{}, model.Event{}, cerr
		}
		newReqID = created.ID
	}
	threadID := r.ThreadID
	now := nowText()
	if threadID == "" {
		threadID, err = findThreadTx(ctx, tx, r.Inbox.AccountID, r.Inbox.ID, r.InReplyTo, r.References)
		if err != nil {
			return model.Message{}, model.Event{}, err
		}
		if threadID == "" {
			threadID = idgen.New("thr")
			if _, err = tx.ExecContext(ctx, `INSERT INTO threads(id,account_id,inbox_id,subject,created_at,updated_at) VALUES(?,?,?,?,?,?)`, threadID, r.Inbox.AccountID, r.Inbox.ID, r.Subject, now, now); err != nil {
				return model.Message{}, model.Event{}, err
			}
		}
	}
	id := idgen.New("msg")
	// The message is enqueued as pending; the worker marks it sent after the
	// provider accepts it. sent_at is left NULL until delivery succeeds.
	_, err = tx.ExecContext(ctx, `INSERT INTO messages(id,account_id,inbox_id,thread_id,direction,provider,provider_message_id,rfc_message_id,in_reply_to,references_json,from_name,from_address,to_json,cc_json,bcc_json,envelope_to_json,client_label,client_id,subject,text_body,html_body,raw_path,size_bytes,is_read,status,idem_key,last_error,internal,sending_domain_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1,'pending',?,?,?,?,?)`, id, r.Inbox.AccountID, r.Inbox.ID, threadID, "outbound", r.Provider, r.ProviderMessageID, r.RFCMessageID, r.InReplyTo, jsonString(r.References), r.From.Name, r.From.Address, jsonString(r.To), jsonString(r.CC), jsonString(r.BCC), `[]`, r.ClientLabel, r.ClientID, r.Subject, r.Text, r.HTML, r.RawPath, r.SizeBytes, r.IdemKey, r.LastError, boolInt(r.Internal), nullString(r.SendingDomainID), now)
	if err != nil {
		return model.Message{}, model.Event{}, err
	}
	if err := cacheLocalPreview(ctx, tx, id, r.Text, r.HTML); err != nil {
		return model.Message{}, model.Event{}, err
	}
	var attachmentNames []string
	for _, a := range r.Attachments {
		aid := idgen.New("att")
		if _, err = tx.ExecContext(ctx, `INSERT INTO attachments(id,message_id,filename,content_type,size_bytes,part_index,content_id) VALUES(?,?,?,?,?,?,?)`, aid, id, a.Filename, a.ContentType, a.Size, a.PartIndex, a.ContentID); err != nil {
			return model.Message{}, model.Event{}, err
		}
		attachmentNames = append(attachmentNames, a.Filename)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO message_fts(message_id,account_id,inbox_id,subject,from_address,recipients,body,attachment_names) VALUES(?,?,?,?,?,?,?,?)`, id, r.Inbox.AccountID, r.Inbox.ID, r.Subject, r.From.Address, strings.Join(append(append([]string{}, r.To...), r.CC...), " "), r.Text+" "+stripHTMLText(r.HTML), strings.Join(attachmentNames, " ")); err != nil {
		return model.Message{}, model.Event{}, err
	}
	if err = adjustStorageTx(ctx, tx, r.Inbox.AccountID, r.Inbox.ID, r.SizeBytes); err != nil {
		return model.Message{}, model.Event{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE threads SET updated_at=? WHERE id=?`, now, threadID); err != nil {
		return model.Message{}, model.Event{}, err
	}
	// Record the key-to-message mapping as part of enqueueing, so a retry with
	// the same key replays the queued message regardless of delivery outcome.
	// The reservation's request fingerprint is kept, not reset: a draft send
	// replays through IdempotentReplay after its draft row is gone, and only the
	// stored hash can prove the retry matches.
	if r.IdemKey != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO outbound_idempotency(account_id,idem_key,message_id,inbox_id,result_json,status,created_at) VALUES(?,?,?,?,COALESCE((SELECT result_json FROM outbound_idempotency WHERE account_id=? AND idem_key=?),'{}'),'done',?) ON CONFLICT(account_id,idem_key) DO UPDATE SET message_id=excluded.message_id,inbox_id=excluded.inbox_id,status='done'`, r.Inbox.AccountID, r.IdemKey, id, r.Inbox.ID, r.Inbox.AccountID, r.IdemKey, now); err != nil {
			return model.Message{}, model.Event{}, err
		}
	}
	// An approved send claims its send request in the same transaction that
	// enqueues the message, so a request can never authorize two sends.
	if r.SendRequestID != "" {
		ev, aerr := approveSendRequestTx(ctx, tx, r.Inbox.AccountID, r.DraftID, r.SendRequestID, r.DecisionActor, r.DecisionActorID, r.DecisionMethod, r.DecisionFeedback, id)
		if aerr != nil {
			return model.Message{}, model.Event{}, aerr
		}
		draftEvent = ev
	}
	// A newly created external request links to its just-queued approval email
	// and emits the requested event in the same transaction.
	if newReqID != "" {
		if _, err = tx.ExecContext(ctx, `UPDATE draft_send_requests SET approval_message_id=?,updated_at=? WHERE id=? AND account_id=?`, id, now, newReqID, r.Inbox.AccountID); err != nil {
			return model.Message{}, model.Event{}, err
		}
		req, rerr := getSendRequestByIDTx(ctx, tx, r.Inbox.AccountID, newReqID)
		if rerr != nil {
			return model.Message{}, model.Event{}, rerr
		}
		ev, eerr := insertEventTx(ctx, tx, r.Inbox.AccountID, req.InboxID, model.EventDraftSendRequested, req.ID, sendRequestPayload(req))
		if eerr != nil {
			return model.Message{}, model.Event{}, eerr
		}
		draftEvent = ev
	}
	m, err := s.getMessageTx(ctx, tx, r.Inbox.AccountID, id)
	if err != nil {
		return model.Message{}, model.Event{}, err
	}
	if err = tx.Commit(); err != nil {
		// A commit error does not mean the transaction rolled back: SQLite can
		// report an error after the write was durably applied. The enqueue is
		// keyed on a freshly minted id, so an existing row with that id is
		// necessarily this enqueue's own committed row and may be returned. The
		// send request / draft event is not reconstructed from a post-commit
		// read (a second read cannot prove it was committed), so callers that
		// supplied a request or draft must treat the empty event as a lost
		// notification rather than a duplicate write.
		if existing, gerr := s.GetMessageByID(context.WithoutCancel(ctx), r.Inbox.AccountID, id); gerr == nil && existing.IdemKey == r.IdemKey && existing.CreatedAt.Equal(parseTime(now)) {
			return existing, model.Event{}, nil
		}
		return model.Message{}, model.Event{}, err
	}
	return m, draftEvent, nil
}

// MarkSent transitions a pending outbound message to sent after the provider
// accepts it, recording the provider message id and emitting the message.sent
// event. It is the durable truth that delivery succeeded. The delivery attempt
// is appended to the per-domain log in the same transaction. The domain is
// derived from the message's inbox in the same transaction, so deleting the
// domain config during an in-flight send cannot break outcome persistence.
func (s *Store) MarkSent(ctx context.Context, accountID, id, providerMessageID, provider, claim string) (model.Message, []model.Event, error) {
	if err := consumeOutboundWriteFault("MarkSent", id); err != nil {
		return model.Message{}, nil, err
	}
	// A completed send lives in the domain inbox's Sent folder. Seed the system
	// folders before opening the write transaction: an inbox can receive its
	// first completed send before anything has listed its folder tree, and the
	// seeding helper needs its own connection. A custom folder a user filed the
	// message in is kept.
	var targetInbox, targetKind string
	if qerr := s.read.QueryRowContext(ctx, `SELECT i.id,i.kind FROM messages m JOIN inboxes i ON i.id=m.inbox_id WHERE m.id=? AND m.account_id=?`, id, accountID).Scan(&targetInbox, &targetKind); qerr == nil && targetKind == model.InboxKindDomain {
		if _, ferr := s.EnsureSystemFolders(ctx, accountID, targetInbox); ferr != nil {
			return model.Message{}, nil, ferr
		}
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.Message{}, nil, err
	}
	defer tx.Rollback()
	var inboxID, threadID string
	var domainNull sql.NullString
	var internal int
	var owner string
	var status, marker string
	if err = tx.QueryRowContext(ctx, `SELECT m.inbox_id,m.thread_id,COALESCE(m.sending_domain_id,i.domain_id),m.internal,m.claim_owner,m.status,m.send_attempt_state FROM messages m JOIN inboxes i ON i.id=m.inbox_id WHERE m.id=? AND m.account_id=?`, id, accountID).Scan(&inboxID, &threadID, &domainNull, &internal, &owner, &status, &marker); err != nil {
		if err == sql.ErrNoRows {
			return model.Message{}, nil, ErrNotFound
		}
		return model.Message{}, nil, err
	}
	// A definitive "sent" is recorded for a message that has not been cancelled
	// (a trashed message must not be flipped back to sent); the deletion case is
	// reported as ErrNotFound below so it stays distinguishable from a send
	// error. An already-resolved message keeps its previous outcome semantics.
	if status != "pending" && status != "sent" {
		return model.Message{}, nil, ErrConflict
	}
	// The provider send belonging to this outcome may have outlived its claim
	// lease: if another attempt has re-claimed the message while this send was
	// in flight, recording "sent" here would race that attempt's own send. The
	// caller logs the ambiguity instead; the re-claiming attempt owns the
	// outcome from here on. An empty owner means the claim was released without
	// a replacement attempt, which is still ours to resolve.
	if claimTaken(owner, claim) {
		return model.Message{}, nil, ErrClaimTaken
	}
	// The pre-send marker, when present, must belong to this attempt. A marker
	// that belongs to a different, later attempt means this send is stale even
	// though the claim predicate passed (a requeue clears the claim but keeps
	// the marker); recording it here would resolve the later attempt's marker.
	if marker != "" && marker != claim {
		return model.Message{}, nil, ErrClaimTaken
	}
	// A standalone inbox has no managed domain, so sending_domain_id and
	// domain_id are both NULL; the delivery-log row is then keyed on an empty
	// domain rather than failing the whole send.
	domainID := domainNull.String
	now := nowText()
	// rfc_message_id is the stable Message-ID Mammoth Mail minted at enqueue time;
	// it is never rewritten, so a message id means the same thing whether the
	// send succeeded or failed. A provider that assigns its own wire id is
	// recorded in provider_message_id, and reply matching already looks in both
	// columns (see findThreadTx), so threading still works.
	update := `UPDATE messages SET status='sent',provider=?,provider_message_id=?,sent_at=?,attempts=attempts+1,last_error='',next_attempt_at='',claim_owner='',claim_expires_at='',send_attempt_state=''`
	args := []any{provider, providerMessageID, now}
	update += ` WHERE id=? AND account_id=?`
	args = append(args, id, accountID)
	if _, err = tx.ExecContext(ctx, update, args...); err != nil {
		return model.Message{}, nil, err
	}
	// A completed send lives in the domain inbox's Sent folder: file it there, the
	// same location the message reports, so folder=<Sent> lists it. A custom
	// folder a user already filed it in is kept.
	if _, err = tx.ExecContext(ctx, `UPDATE messages SET mailbox_id=(SELECT f.id FROM inbox_folders f WHERE f.inbox_id=messages.inbox_id AND f.role='sent' ORDER BY f.path LIMIT 1)
		WHERE id=? AND account_id=? AND COALESCE(mailbox_id,'')=''
		AND EXISTS(SELECT 1 FROM inbox_folders f WHERE f.inbox_id=messages.inbox_id AND f.role='sent')`, id, accountID); err != nil {
		return model.Message{}, nil, err
	}
	// A message cancelled (deleted) between the initial read and this write is
	// gone: do not append an unattributable delivery-log row, and report it as
	// not-found so the caller can distinguish cancellation from a send error.
	if ok, exErr := messageExistsTx(ctx, tx, accountID, id); exErr != nil {
		return model.Message{}, nil, exErr
	} else if !ok {
		return model.Message{}, nil, ErrNotFound
	}
	if err = s.insertDeliveryAttemptTx(ctx, tx, accountID, domainID, provider, id, "sent", providerMessageID, ""); err != nil {
		return model.Message{}, nil, err
	}
	events := []model.Event{}
	if internal == 0 {
		ev, eerr := insertEventTx(ctx, tx, accountID, inboxID, "message.sent", id, map[string]any{"message_id": id, "inbox_id": inboxID, "thread_id": threadID})
		if eerr != nil {
			return model.Message{}, nil, eerr
		}
		events = append(events, ev)
	}
	if dev, derr := sendRequestDeliveryTx(ctx, tx, accountID, id, model.SendDeliverySent, model.EventDraftSent); derr != nil {
		return model.Message{}, nil, derr
	} else if dev != nil {
		events = append(events, *dev)
	}
	m, err := s.getMessageTx(ctx, tx, accountID, id)
	if err != nil {
		return model.Message{}, nil, err
	}
	if err = tx.Commit(); err != nil {
		// A commit error may still have durably applied this transaction. The
		// outcome is only "sent" if the committed row actually says so: an
		// unconditional re-read would silently report success for a row that a
		// later attempt had already failed, or for one whose status is no longer
		// sent. Anything else leaves the outcome genuinely unknown.
		if existing, gerr := s.GetMessageByID(context.WithoutCancel(ctx), accountID, id); gerr == nil && existing.Status == "sent" && existing.SentAt != nil && existing.ProviderMessageID == providerMessageID {
			return existing, events, nil
		}
		return model.Message{}, nil, err
	}
	return m, events, nil
}

// MarkFailed records a failed delivery attempt. If attempts remain, the message
// is returned to pending with a next_attempt_at; otherwise it is marked failed.
// The failed attempt is appended to the per-domain log in the same transaction.
// The domain is derived from the message's inbox in the same transaction, so a
// config deletion during an in-flight send cannot break outcome persistence.
func (s *Store) MarkFailed(ctx context.Context, accountID, id, errText string, nextAttemptAt time.Time, maxAttempts int, provider, claim string) (model.Message, []model.Event, error) {
	if err := consumeOutboundWriteFault("MarkFailed", id); err != nil {
		return model.Message{}, nil, err
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.Message{}, nil, err
	}
	defer tx.Rollback()
	var attempts int
	var domainNull sql.NullString
	var inboxID, fromAddress, subject, toJSON, owner, status, marker string
	if err = tx.QueryRowContext(ctx, `SELECT m.attempts,COALESCE(m.sending_domain_id,i.domain_id),m.inbox_id,m.from_address,m.subject,m.to_json,m.claim_owner,m.status,m.send_attempt_state FROM messages m JOIN inboxes i ON i.id=m.inbox_id WHERE m.id=? AND m.account_id=?`, id, accountID).Scan(&attempts, &domainNull, &inboxID, &fromAddress, &subject, &toJSON, &owner, &status, &marker); err != nil {
		if err == sql.ErrNoRows {
			return model.Message{}, nil, ErrNotFound
		}
		return model.Message{}, nil, err
	}
	// A failure may be recorded for a pending message (a retry or a terminal
	// failure), or re-recorded for an already-failed one (an inert duplicate
	// outcome, which must not write a second notice). A sent or cancelled
	// message must not be dragged back into the failure path; the cancelled
	// (trashed) case is reported by the existence check below as ErrNotFound.
	if status != "pending" && status != "failed" {
		return model.Message{}, nil, ErrConflict
	}
	// A late failure cannot overwrite an already-failed or uncertain outcome.
	if status == "failed" {
		m, gerr := s.getMessageTx(ctx, tx, accountID, id)
		return m, nil, gerr
	}
	// A stale attempt's failure must not clear another attempt's live claim:
	// that attempt owns the outcome now, so this write is skipped entirely.
	if claimTaken(owner, claim) {
		return model.Message{}, nil, ErrClaimTaken
	}
	// The pre-send marker, when present, must belong to this attempt; a marker
	// left by a different, later attempt means this failure is stale.
	if marker != "" && marker != claim {
		return model.Message{}, nil, ErrClaimTaken
	}
	domainID := domainNull.String
	attempts++
	status = "pending"
	next := ""
	if attempts >= maxAttempts {
		status = "failed"
	} else {
		next = timeText(nextAttemptAt)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE messages SET status=?,attempts=?,last_error=?,next_attempt_at=?,claim_owner='',claim_expires_at='',send_attempt_state='' WHERE id=? AND account_id=? AND status='pending'`, status, attempts, errText, next, id, accountID); err != nil {
		return model.Message{}, nil, err
	}
	// A message cancelled (deleted) between the initial read and this write is
	// gone; do not append an unattributable delivery-log row for it.
	if ok, exErr := messageExistsTx(ctx, tx, accountID, id); exErr != nil {
		return model.Message{}, nil, exErr
	} else if !ok {
		return model.Message{}, nil, ErrNotFound
	}
	if err = s.insertDeliveryAttemptTx(ctx, tx, accountID, domainID, provider, id, "failed", "", errText); err != nil {
		return model.Message{}, nil, err
	}
	var events []model.Event
	if status == "failed" {
		if dev, derr := sendRequestDeliveryTx(ctx, tx, accountID, id, model.SendDeliveryFailed, model.EventDraftSendFailed); derr != nil {
			return model.Message{}, nil, derr
		} else if dev != nil {
			events = append(events, *dev)
		}
		// A terminal failure is surfaced as a local delivery-failure notice in
		// the inbox: a real inbound message the user (and any agent reading the
		// mailbox) will see, rather than only a row in the outbox queue. The
		// failed original is trashed as part of the same write.
		_, noticeEvents, nerr := s.writeDeliveryFailureTx(ctx, tx, deliveryFailure{
			AccountID:   accountID,
			InboxID:     inboxID,
			MessageID:   id,
			Provider:    provider,
			FromAddress: fromAddress,
			To:          decodeStrings(toJSON),
			Subject:     subject,
			LastError:   errText,
			Attempts:    attempts,
			domainID:    domainID,
		})
		if nerr != nil {
			return model.Message{}, nil, nerr
		}
		events = append(events, noticeEvents...)
	}
	m, err := s.getMessageTx(ctx, tx, accountID, id)
	if err != nil {
		return model.Message{}, nil, err
	}
	if err = tx.Commit(); err != nil {
		// A commit error may still have durably applied this transaction. The
		// failure is only resolved if the committed row actually carries this
		// attempt: an unconditional re-read would report a stale or unrelated
		// row as this outcome. Anything else leaves the outcome unknown, and the
		// durable pre-send marker keeps the row from being resent.
		if existing, gerr := s.GetMessageByID(context.WithoutCancel(ctx), accountID, id); gerr == nil && existing.Attempts == attempts && existing.Status == status && existing.LastError == errText {
			return existing, events, nil
		}
		return model.Message{}, nil, err
	}
	return m, events, nil
}

// ClaimNextPending atomically claims the next due pending message for delivery.
// It returns the message id and the claim token minted for this claim, or ""
// credentials when none is due. The claim uses a unique token (owner prefix plus
// a fresh random suffix) rather than the worker's process-wide identity, so two
// senders of the same worker are distinguishable: a stale attempt whose lease
// lapsed mid-send cannot pass the ownership check of the attempt that
// re-claimed the message, and the outcome writes can reject the stale attempt
// outright (ErrClaimTaken). The claim is separate from retry scheduling:
// next_attempt_at is left untouched and only the claim lease is recorded, so a
// crash can be recovered as soon as the lease lapses (bounded to the worker's
// delivery attempt timeout plus margin by NewOutboxWorker) rather than waiting
// out the message's full retry schedule.
func (s *Store) ClaimNextPending(ctx context.Context, now time.Time, owner string, lease time.Duration) (string, string, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE status='pending' AND deleted_at IS NULL AND send_attempt_state='' AND (next_attempt_at='' OR next_attempt_at<=?) AND (claim_owner='' OR claim_expires_at<=?) ORDER BY created_at ASC LIMIT 1`, timeText(now), timeText(now)).Scan(&id)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	claim := owner + ":" + idgen.New("clm")
	if _, err = tx.ExecContext(ctx, `UPDATE messages SET claim_owner=?,claim_expires_at=? WHERE id=?`, claim, timeText(now.Add(lease)), id); err != nil {
		return "", "", err
	}
	if err = tx.Commit(); err != nil {
		return "", "", err
	}
	return id, claim, nil
}

// RecoverAbandonedClaims clears claims left by a previous process. Under the
// single-process model every outstanding claim at startup is abandoned, so the
// row becomes claimable again.
//
// A row carrying a durable pre-send marker is deliberately excluded: its
// provider send was started but never resolved, so the mail may already have
// been accepted. Clearing its claim here (or letting startup recovery make it
// claimable) would resend it. Such rows are left with their claim and marker
// attached until an operator resolves them; see ListQuarantinedSends.
func (s *Store) RecoverAbandonedClaims(ctx context.Context) error {
	_, err := s.write.ExecContext(ctx, `UPDATE messages SET claim_owner='',claim_expires_at='' WHERE status='pending' AND claim_owner!='' AND send_attempt_state=''`)
	return err
}

// ReleaseMessageClaim clears a claim held by the given claim token so the
// message can be retried immediately instead of waiting for the lease to
// expire. The token and ownership guard make it a no-op for a re-claimed or
// resolved message. owner is the claim token minted by ClaimNextPending (an
// empty owner releases nothing).
//
// A row carrying a durable pre-send marker is a no-op as well: its claim is the
// record of the unresolved send in flight and must not be dropped on the way to
// a retry.
func (s *Store) ReleaseMessageClaim(ctx context.Context, id, owner string) error {
	if owner == "" {
		return nil
	}
	_, err := s.write.ExecContext(ctx, `UPDATE messages SET claim_owner='',claim_expires_at='' WHERE id=? AND claim_owner=? AND status='pending' AND send_attempt_state=''`, id, owner)
	return err
}

// MessageClaimOwner returns the current claim owner for a message.
func (s *Store) MessageClaimOwner(ctx context.Context, accountID, id string) (string, error) {
	var owner string
	err := s.read.QueryRowContext(ctx, `SELECT claim_owner FROM messages WHERE id=? AND account_id=?`, id, accountID).Scan(&owner)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return owner, err
}

// BeginMessageSend durably records that this attempt is about to hand the
// message to the provider. It is written immediately before the provider call,
// so there is no window in which a send is in flight with no marker: from this
// write onward the queue refuses to claim the row and outcome resolution is the
// only way the marker is cleared.
//
// The claim must still be owned by the caller (the same predicate the outcome
// writes use), so an attempt whose lease lapsed mid-flight cannot arm a marker
// over a live re-claim. A non-empty claim token is required: the marker IS the
// claim token, so an empty token cannot be stored and the empty-claim callers
// (tests and non-worker callers that skip claim scoping) do not participate in
// the pre-send safety protocol. The production worker always passes the token
// minted by ClaimNextPending. It returns ErrClaimTaken when the claim no longer
// owns the message and ErrNotFound when the message is no longer pending or does
// not exist.
func (s *Store) BeginMessageSend(ctx context.Context, accountID, id, claim string) error {
	if claim == "" {
		// A caller without a claim token (a direct, non-worker invocation) has no
		// competing attempt to fence off, so there is no marker to arm. The
		// guarantee applies to the worker path, which always holds a claim.
		return nil
	}
	if err := consumeOutboundWriteFault("BeginMessageSend", id); err != nil {
		return err
	}
	res, err := s.write.ExecContext(ctx, `UPDATE messages SET send_attempt_state=? WHERE id=? AND account_id=? AND status='pending' AND send_attempt_state='' AND `+sendClaimClause, claim, id, accountID, claim)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	// Nothing matched: distinguish a taken/mismatched claim from a message that
	// is gone or no longer pending, so the caller can tell an ambiguous send
	// from a cancelled one.
	var owner, state, status string
	if err := s.read.QueryRowContext(ctx, `SELECT claim_owner,send_attempt_state,status FROM messages WHERE id=? AND account_id=?`, id, accountID).Scan(&owner, &state, &status); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return err
	}
	if claimTaken(owner, claim) || (state != "" && state != claim) {
		return ErrClaimTaken
	}
	// The claim still matches but the marker is already set: the message was
	// armed by an earlier attempt of this token. Treat it as taken so a second
	// provider send is never started under the same claim.
	if state != "" {
		return ErrClaimTaken
	}
	return ErrNotFound
}

// MessageSendInFlight reports whether a message currently carries an unresolved
// pre-send marker. It is the read surface for the guard that stops a message
// with a provider send in flight from being automatically resent.
func (s *Store) MessageSendInFlight(ctx context.Context, accountID, id string) (bool, error) {
	var state string
	err := s.read.QueryRowContext(ctx, `SELECT send_attempt_state FROM messages WHERE id=? AND account_id=?`, id, accountID).Scan(&state)
	if err == sql.ErrNoRows {
		return false, ErrNotFound
	}
	return state != "", err
}

// QuarantinedSend is a queued outbound message whose provider send started but
// never reached a definitive outcome. The provider may already have accepted the
// mail, so it is never resent automatically; it waits for an operator to retry
// it explicitly (with a duplicate warning) or discard it.
type QuarantinedSend struct {
	MessageID   string
	InboxID     string
	To          []string
	Subject     string
	Provider    string
	Attempts    int
	LastError   string
	ClaimOwner  string
	ClaimExpire time.Time
	CreatedAt   time.Time
}

// ListQuarantinedSends returns queued outbound messages with an unresolved
// pre-send marker, so an operator can review the sends that are holding for
// explicit resolution rather than delivering or retrying. It is account-scoped
// and ordered oldest first, matching the order an operator would work them.
func (s *Store) ListQuarantinedSends(ctx context.Context, accountID string, limit int) ([]QuarantinedSend, error) {
	if limit <= 0 || limit > limits.PageSizeMaxList {
		limit = limits.PageSizeDefault
	}
	rows, err := s.read.QueryContext(ctx, `SELECT id,inbox_id,to_json,subject,provider,attempts,last_error,claim_owner,claim_expires_at,created_at FROM messages WHERE account_id=? AND direction='outbound' AND status='pending' AND send_attempt_state<>'' ORDER BY created_at ASC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QuarantinedSend{}
	for rows.Next() {
		var q QuarantinedSend
		var to, claimExp, created string
		if err = rows.Scan(&q.MessageID, &q.InboxID, &to, &q.Subject, &q.Provider, &q.Attempts, &q.LastError, &q.ClaimOwner, &claimExp, &created); err != nil {
			return nil, err
		}
		q.To = decodeStrings(to)
		q.ClaimExpire = parseTime(claimExp)
		q.CreatedAt = parseTime(created)
		out = append(out, q)
	}
	return out, rows.Err()
}

// ResolveQuarantinedSend is the operator's explicit resolution of a quarantined
// send. allowResend true clears the marker and returns the message to the queue
// (an explicit "send it anyway"; the provider may already have accepted it, so
// the caller must surface the duplicate risk). allowResend false discards the
// unresolved send by marking it failed with the given reason, clearing the
// marker and claim without scheduling a retry. It returns ErrConflict when the
// message is not a pending quarantined send.
func (s *Store) ResolveQuarantinedSend(ctx context.Context, p model.Principal, id, reason string, allowResend bool) error {
	m, err := s.GetMessage(ctx, p, id)
	if err != nil {
		return err
	}
	if m.Direction != "outbound" || m.Status != "pending" {
		return ErrConflict
	}
	if !p.CanOwn(m.InboxID) {
		return ErrForbidden
	}
	if !allowResend {
		res, err := s.write.ExecContext(ctx, `UPDATE messages SET status='failed',send_attempt_state='',claim_owner='',claim_expires_at='',next_attempt_at='',last_error=? WHERE id=? AND account_id=? AND status='pending' AND send_attempt_state<>''`, quarantinedReasonPrefix+reason, id, p.AccountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	}
	res, err := s.write.ExecContext(ctx, `UPDATE messages SET send_attempt_state='',attempts=0,last_error='',next_attempt_at='',claim_owner='',claim_expires_at='' WHERE id=? AND account_id=? AND status='pending' AND send_attempt_state<>''`, id, p.AccountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkMessageUncertain records an unresolved provider handoff as a failed
// Outbox item without generating a misleading delivery-failure notice.
// The pre-send marker remains durable until an explicitly confirmed retry.
func (s *Store) MarkMessageUncertain(ctx context.Context, accountID, id, reason, claim string) error {
	if strings.TrimSpace(reason) == "" {
		reason = "the provider outcome could not be verified"
	}
	res, err := s.write.ExecContext(ctx, `UPDATE messages SET status='failed',attempts=attempts+1,last_error=?,next_attempt_at='',claim_owner='',claim_expires_at=''
		WHERE id=? AND account_id=? AND direction='outbound' AND status='pending'
		AND send_attempt_state=? AND (claim_owner=? OR claim_owner='')`, quarantinedReasonPrefix+reason, id, accountID, claim, claim)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return nil
}

// ReconcileUncertainSends surfaces abandoned pre-send markers in the Outbox
// and approval requests. During normal operation it only affects expired
// leases; on restart there are no live provider calls in this process.
func (s *Store) ReconcileUncertainSends(ctx context.Context, now time.Time, afterRestart bool) (int64, int64, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	guard := " AND (claim_expires_at='' OR claim_expires_at<=?)"
	args := []any{quarantinedReasonPrefix + "previous send outcome could not be verified"}
	if afterRestart {
		guard = ""
	} else {
		args = append(args, timeText(now))
	}
	res, err := tx.ExecContext(ctx, `UPDATE messages SET status='failed',attempts=attempts+1,last_error=?,next_attempt_at='',claim_owner='',claim_expires_at=''
		WHERE direction='outbound' AND status='pending' AND send_attempt_state<>''`+guard, args...)
	if err != nil {
		return 0, 0, err
	}
	messageCount, _ := res.RowsAffected()
	workflowArgs := []any{quarantinedReasonPrefix + "approval notification outcome could not be verified", timeText(now)}
	if !afterRestart {
		workflowArgs = append(workflowArgs, timeText(now))
	}
	res, err = tx.ExecContext(ctx, `UPDATE outbound_workflow SET status='failed',attempts=attempts+1,last_error=?,terminal_at=?,next_attempt_at='',claim_owner='',claim_expires_at=''
		WHERE status='pending' AND send_attempt_state<>''`+guard, workflowArgs...)
	if err != nil {
		return 0, 0, err
	}
	workflowCount, _ := res.RowsAffected()
	if workflowCount > 0 {
		_, err = tx.ExecContext(ctx, `UPDATE draft_send_requests SET notification_status=?,updated_at=?
			WHERE status='pending' AND approval_workflow_id IN (
				SELECT id FROM outbound_workflow WHERE status='failed' AND send_attempt_state<>'' AND last_error LIKE 'uncertain delivery: %')`,
			model.NotificationUncertain, timeText(now))
		if err != nil {
			return 0, 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, err
	}
	return messageCount, workflowCount, nil
}

// RequeuePendingForDomain resets every pending outbound message for a domain so
// it retries immediately after the domain's sending configuration is saved.
// This covers both messages whose inbox is on the domain and send-as-alias
// messages that chose the domain's config. Attempts, retry backoff, the
// no-provider hold and the claim lease are all cleared, restoring the full
// retry budget. Sent and failed messages are untouched; failed messages still
// require an explicit retry.
//
// A message with an active send in flight is never touched: either its
// claim_owner is still set (an attempt holds it) or it carries a durable
// pre-send marker (an attempt started the provider call). In both cases the
// in-flight attempt finishes under the configuration it started with, and the
// new configuration applies to the next attempt if that one fails and is
// rescheduled. Without this guard the requeue would clear a live claim or its
// marker mid-send and let the message be delivered twice.
func (s *Store) RequeuePendingForDomain(ctx context.Context, accountID, domainID string) (int64, error) {
	res, err := s.write.ExecContext(ctx, `UPDATE messages SET attempts=0,last_error='',next_attempt_at='',claim_owner='',claim_expires_at='' WHERE account_id=? AND direction='outbound' AND status='pending' AND send_attempt_state='' AND claim_owner='' AND (sending_domain_id=? OR (sending_domain_id IS NULL AND inbox_id IN (SELECT id FROM inboxes WHERE domain_id=? AND account_id=?)))`, accountID, domainID, domainID, accountID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RequeueFailed resets a failed message to pending for a manual retry. A
// terminal failure is trashed as part of writing its delivery-failure notice, so
// the retry also restores the message to the mailbox (clearing deleted_at) and
// lets it rejoin the outbox queue. See RequeueFailedWithWarning for the
// operator-facing form that surfaces the duplicate risk of retrying a message
// whose send was never resolved.
func (s *Store) RequeueFailed(ctx context.Context, p model.Principal, id string) error {
	_, err := s.RequeueFailedConfirmed(ctx, p, id, false)
	return err
}

// RequeueFailedWithWarning is RequeueFailed with an operator warning. Retrying a
// message quarantined by an unresolved in-flight send (a durable pre-send
// marker) is the one way a marker is dropped without a definitive provider
// outcome, so it can resend mail a provider may already have accepted. The
// warning is non-empty in that case and the caller must surface it to the
// operator; it is empty for an ordinary retry.
func (s *Store) RequeueFailedWithWarning(ctx context.Context, p model.Principal, id string) (string, error) {
	return s.RequeueFailedConfirmed(ctx, p, id, true)
}

// RequeueFailedConfirmed refuses an uncertain send unless its possible
// duplicate delivery was explicitly acknowledged by the caller.
func (s *Store) RequeueFailedConfirmed(ctx context.Context, p model.Principal, id string, confirmDuplicateRisk bool) (string, error) {
	m, err := s.GetMessage(ctx, p, id)
	if err != nil {
		return "", err
	}
	if m.Direction != "outbound" || m.Status != "failed" {
		return "", ErrConflict
	}
	if !p.CanOwn(m.InboxID) {
		return "", ErrForbidden
	}
	var marker string
	if err := s.read.QueryRowContext(ctx, `SELECT send_attempt_state FROM messages WHERE id=? AND account_id=?`, id, p.AccountID).Scan(&marker); err != nil && err != sql.ErrNoRows {
		return "", err
	}
	// A quarantined send is one resolved as a discard: its provider outcome is
	// unknown, so the retry must warn about a possible duplicate. The marker
	// itself is cleared by that resolution, so the terminal failure reason is
	// what distinguishes it from an ordinary provider failure.
	quarantined := marker != "" || IsUncertainDelivery(m.LastError)
	if quarantined && !confirmDuplicateRisk {
		return "", ErrUncertainDelivery
	}
	res, err := s.write.ExecContext(ctx, `UPDATE messages SET status='pending',attempts=0,last_error='',next_attempt_at='',claim_owner='',claim_expires_at='',send_attempt_state='',deleted_at=NULL
		WHERE id=? AND account_id=? AND direction='outbound' AND status='failed' AND last_error=? AND send_attempt_state=?`, id, p.AccountID, m.LastError, marker)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrConflict
	}
	if quarantined {
		return "a previous attempt may already have been accepted by the provider; retrying this message can deliver a duplicate", nil
	}
	return "", nil
}

// ListOutbox lists pending and failed outbound messages for an account,
// optionally scoped to an inbox.
func (s *Store) ListOutbox(ctx context.Context, p model.Principal, inboxID string, limit int) ([]model.Message, error) {
	return s.ListOutboxBefore(ctx, p, inboxID, "", limit)
}

// ListOutboxBefore lists queued outbound messages (pending or failed) newest
// first, optionally older than the message id `before` (a keyset cursor). It
// backs the Outbox pager so more than one page of queued mail is reachable;
// ListOutbox is the unpaged form.
func (s *Store) ListOutboxBefore(ctx context.Context, p model.Principal, inboxID, before string, limit int) ([]model.Message, error) {
	q := messageSelect + ` FROM messages m WHERE m.account_id=? AND m.internal=0 AND m.deleted_at IS NULL AND m.direction='outbound' AND m.status IN ('pending','failed')`
	args := []any{p.AccountID}
	if inboxID != "" {
		if !p.CanRead(inboxID) {
			return nil, ErrForbidden
		}
		q += ` AND m.inbox_id=?`
		args = append(args, inboxID)
	} else if !p.Admin {
		ids := principalInboxIDs(p)
		if len(ids) == 0 {
			return []model.Message{}, nil
		}
		q += ` AND m.inbox_id IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if before != "" {
		var beforeCreated string
		err := s.read.QueryRowContext(ctx, `SELECT created_at FROM messages WHERE id=? AND account_id=?`, before, p.AccountID).Scan(&beforeCreated)
		if err == nil {
			q += ` AND (m.created_at < ? OR (m.created_at = ? AND m.rowid < (SELECT rowid FROM messages WHERE id=? AND account_id=?)))`
			args = append(args, beforeCreated, beforeCreated, before, p.AccountID)
		} else if err != sql.ErrNoRows {
			return nil, err
		}
	}
	if limit <= 0 || limit > limits.PageSizeMaxList {
		limit = limits.PageSizeDefault
	}
	q += ` ORDER BY m.created_at DESC, m.rowid DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	// Mark messages whose latest attempt is currently in flight, so the outbox
	// can show "Sending…" rather than a generic queued "Pending".
	if err = s.markOutboxSending(ctx, p.AccountID, out); err != nil {
		return nil, err
	}
	return out, nil
}

// markOutboxSending sets Sending on each message that has an in-flight
// ("sending") delivery-log row. It is a single query keyed by message id.
func (s *Store) markOutboxSending(ctx context.Context, accountID string, msgs []model.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(msgs))
	index := make(map[string]int, len(msgs))
	for i, m := range msgs {
		ids = append(ids, m.ID)
		index[m.ID] = i
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, accountID)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.read.QueryContext(ctx, `SELECT DISTINCT message_id FROM outbound_delivery_log WHERE account_id=? AND status='sending' AND message_id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return err
		}
		if i, ok := index[id]; ok {
			msgs[i].Sending = true
		}
	}
	return rows.Err()
}

// CountOutbox returns the number of pending or failed outbound messages for
// an inbox (or across all accessible inboxes when inboxID is empty).
func (s *Store) CountOutbox(ctx context.Context, p model.Principal, inboxID string) (int, error) {
	q := `SELECT count(*) FROM messages m WHERE m.account_id=? AND m.internal=0 AND m.deleted_at IS NULL AND m.direction='outbound' AND m.status IN ('pending','failed')`
	args := []any{p.AccountID}
	if inboxID != "" {
		if !p.CanRead(inboxID) {
			return 0, ErrForbidden
		}
		q += ` AND m.inbox_id=?`
		args = append(args, inboxID)
	} else if !p.Admin {
		ids := principalInboxIDs(p)
		if len(ids) == 0 {
			return 0, nil
		}
		q += ` AND m.inbox_id IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	var n int
	if err := s.read.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// DeleteOutboxMessage moves a pending or failed outbound message to Trash
// (cancelling a queued send or discarding a failed one). The message is hidden
// from the outbox immediately but retained until purged or expired.
func (s *Store) DeleteOutboxMessage(ctx context.Context, p model.Principal, id string) (model.Message, *model.Event, error) {
	m, err := s.GetMessage(ctx, p, id)
	if err != nil {
		return model.Message{}, nil, err
	}
	if m.Direction != "outbound" || (m.Status != "pending" && m.Status != "failed") {
		return model.Message{}, nil, ErrConflict
	}
	if !p.CanOwn(m.InboxID) {
		return model.Message{}, nil, ErrForbidden
	}
	return s.TrashMessage(ctx, p, id)
}

// idempotencyLease bounds how long a pending reservation may block the same
// key before it is treated as abandoned by a crashed process. It is generous
// relative to a provider send so a slow in-flight send is never reclaimed.
const idempotencyLease = 15 * time.Minute

// IdempotencyReserve atomically claims an idempotency key for an in-flight
// send. It returns (true, "", nil) if this caller won the reservation and may
// proceed to enqueue. It returns (false, messageID, nil) if the key was already
// completed, so the caller can return the existing message. It returns
// (false, "", ErrConflict) if another request currently holds the reservation
// (in-flight), which the caller should treat as a retryable conflict.
//
// A pending reservation older than idempotencyLease with no message linked to
// it is treated as abandoned (the process crashed between reserving and
// enqueueing) and is reclaimed, so a legitimate retry is not blocked forever.
// A reservation whose message exists is never reclaimed, because the send was
// durably enqueued and replaying it would be wrong.
// Reuse with a different inbox or request hash conflicts. Legacy keys without
// a request hash also conflict rather than risking a duplicate or wrong replay.
func (s *Store) IdempotencyReserve(ctx context.Context, accountID, key, inboxID, requestHash string) (bool, string, error) {
	if requestHash == "" {
		return false, "", ErrConflict
	}
	// The existing result metadata column was unused; retain the binding on enqueue.
	result, err := json.Marshal(map[string]string{"request_sha256": requestHash})
	if err != nil {
		return false, "", err
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return false, "", err
	}
	defer tx.Rollback()
	var mid, status, created, savedInbox, savedResult string
	err = tx.QueryRowContext(ctx, `SELECT message_id,status,created_at,inbox_id,result_json FROM outbound_idempotency WHERE account_id=? AND idem_key=?`, accountID, key).Scan(&mid, &status, &created, &savedInbox, &savedResult)
	if err == nil {
		// Pending. Reclaim it only when it is stale and has no enqueued message.
		if status == "pending" && mid == "" && !parseTime(created).IsZero() && time.Since(parseTime(created)) > idempotencyLease {
			if _, err = tx.ExecContext(ctx, `UPDATE outbound_idempotency SET status='pending',inbox_id=?,result_json=?,created_at=? WHERE account_id=? AND idem_key=? AND status='pending' AND message_id=''`, inboxID, string(result), nowText(), accountID, key); err != nil {
				return false, "", err
			}
			if err = tx.Commit(); err != nil {
				return false, "", err
			}
			return true, "", nil
		}
		if savedInbox != inboxID || savedResult != string(result) {
			return false, "", ErrConflict
		}
		if status == "done" {
			return false, mid, nil
		}
		// Another request is in flight.
		return false, "", ErrConflict
	}
	if err != sql.ErrNoRows {
		return false, "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO outbound_idempotency(account_id,idem_key,message_id,inbox_id,result_json,status,created_at) VALUES(?,?,?,?,?,?,?)`, accountID, key, "", inboxID, string(result), "pending", nowText()); err != nil {
		return false, "", err
	}
	if err = tx.Commit(); err != nil {
		return false, "", err
	}
	return true, "", nil
}

// IdempotencyRelease clears a pending reservation so a failed send can be
// retried with the same key.
func (s *Store) IdempotencyRelease(ctx context.Context, accountID, key string) error {
	_, err := s.write.ExecContext(ctx, `DELETE FROM outbound_idempotency WHERE account_id=? AND idem_key=? AND status='pending'`, accountID, key)
	return err
}

// IdempotentReplay returns the message an already-completed idempotency key
// enqueued, when the retry carries the same request fingerprint. ok is false
// for an unknown or still-pending key, so the caller proceeds normally. A
// different fingerprint is ErrConflict and never replays.
//
// It exists for the draft paths: a successful send deletes the draft row, so an
// identical retry would otherwise fail before its key is ever consulted.
func (s *Store) IdempotentReplay(ctx context.Context, accountID, key, requestHash string) (model.Message, bool, error) {
	if key == "" || requestHash == "" {
		return model.Message{}, false, nil
	}
	result, err := json.Marshal(map[string]string{"request_sha256": requestHash})
	if err != nil {
		return model.Message{}, false, err
	}
	var mid, savedResult string
	err = s.read.QueryRowContext(ctx, `SELECT message_id,result_json FROM outbound_idempotency WHERE account_id=? AND idem_key=? AND status='done'`, accountID, key).Scan(&mid, &savedResult)
	if err == sql.ErrNoRows {
		return model.Message{}, false, nil
	}
	if err != nil {
		return model.Message{}, false, err
	}
	if savedResult != string(result) {
		return model.Message{}, false, ErrConflict
	}
	m, err := s.GetMessageByID(ctx, accountID, mid)
	if errors.Is(err, ErrNotFound) {
		return model.Message{}, false, nil
	}
	return m, err == nil, err
}

// RecoverStaleIdempotency deletes pending reservations left by a crashed
// process: a pending key with no linked message older than the lease. Keys with
// an enqueued message are preserved (the send is durable and replayable).
func (s *Store) RecoverStaleIdempotency(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.write.ExecContext(ctx, `DELETE FROM outbound_idempotency WHERE status='pending' AND message_id='' AND created_at<=?`, timeText(now.Add(-idempotencyLease)))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *Store) LatestInboundMessageInThread(ctx context.Context, accountID, inboxID, threadID string) (model.Message, error) {
	m, err := scanMessage(s.read.QueryRowContext(ctx, messageSelect+` FROM messages m WHERE m.account_id=? AND m.inbox_id=? AND m.thread_id=? AND m.direction='inbound' AND m.deleted_at IS NULL ORDER BY m.created_at DESC LIMIT 1`, accountID, inboxID, threadID))
	if err == sql.ErrNoRows {
		return m, ErrNotFound
	}
	return m, err
}

// IdempotentMessage returns the message an already-completed idempotency key
// enqueued. ok is false for an unknown or still-pending key.
func (s *Store) IdempotentMessage(ctx context.Context, accountID, key string) (string, bool, error) {
	if key == "" {
		return "", false, nil
	}
	var mid, status string
	err := s.read.QueryRowContext(ctx, `SELECT message_id,status FROM outbound_idempotency WHERE account_id=? AND idem_key=?`, accountID, key).Scan(&mid, &status)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return mid, status == "done" && mid != "", nil
}
