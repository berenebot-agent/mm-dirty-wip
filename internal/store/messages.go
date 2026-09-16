package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"gatehouse-mail/internal/idgen"
	"gatehouse-mail/internal/limits"
	"gatehouse-mail/internal/model"
)

type AttachmentInput struct {
	Filename, ContentType, ContentID string
	Size                             int64
	PartIndex                        int
}
type InboundRecord struct {
	Inbox                                           model.Inbox
	Provider, ProviderDeliveryID, ProviderMessageID string
	EnvelopeRecipient                               string
	RFCMessageID, InReplyTo                         string
	References                                      []string
	From                                            model.Address
	To, CC, EnvelopeTo                              []string
	Subject, Text, HTML, RawPath                    string
	SizeBytes                                       int64
	ReceivedAt                                      time.Time
	Attachments                                     []AttachmentInput
	// Spam is the local auth-policy disposition for MX mail. SpamReason is the
	// bounded reason and AuthResults is the bounded normalized evidence. All
	// three are empty/false for provider webhook mail.
	Spam        bool
	SpamReason  string
	AuthResults string
	// DeliveryFingerprint, when set, is the versioned MX retry identity. It is
	// recorded as a durable receipt in the same transaction so a retry after
	// the message is deleted still deduplicates. Empty for webhook providers,
	// which rely on provider_delivery_id alone. ReceiptTTL overrides the default
	// receipt retention when non-zero.
	DeliveryFingerprint string
	ReceiptTTL          time.Duration
}
type OutboundRecord struct {
	Inbox                                                model.Inbox
	Provider, ProviderMessageID, RFCMessageID, InReplyTo string
	References                                           []string
	From                                                 model.Address
	// SendingDomainID is the domain whose sending configuration was used to
	// enqueue this message. It is the alias's own domain for a send-as-alias,
	// and may differ from the inbox's domain. Empty falls back to the inbox
	// domain at delivery time.
	SendingExternalAliasID       string
	SendingDomainID              string
	To, CC, BCC                  []string
	Subject, Text, HTML, RawPath string
	SizeBytes                    int64
	SentAt                       time.Time
	ThreadID                     string
	IdemKey                      string
	LastError                    string
	DraftID                      string
	// ClientLabel/ClientID snapshot the credential that enqueued the message.
	ClientLabel, ClientID string
	Attachments           []AttachmentInput
	// SendRequestID, when set, is the draft send request being authorized by
	// this send. CommitOutbound claims it atomically and records the decision.
	SendRequestID    string
	DecisionActor    string
	DecisionActorID  string
	DecisionMethod   string
	DecisionFeedback string
	// NewSendRequest, when set, creates a draft send request in the same
	// transaction that enqueues this message. It is used to queue the external
	// approval email atomically with the request that authorizes it. The draft
	// is frozen but not consumed.
	NewSendRequest *SendRequestInsert
	// Internal marks workflow mail (the approval-request email) that is queued
	// in the inbox but hidden from every mailbox read surface, because it
	// carries the one-time approval token.
	Internal bool
}

func (s *Store) CommitInbound(ctx context.Context, r InboundRecord) (model.Message, model.Event, bool, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.Message{}, model.Event{}, false, err
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE account_id=? AND provider=? AND envelope_recipient=? AND provider_delivery_id=?`, r.Inbox.AccountID, r.Provider, r.EnvelopeRecipient, r.ProviderDeliveryID).Scan(&existing)
	if err == nil {
		m, e := model.Message{}, model.Event{}
		_ = tx.Rollback()
		m, err = s.GetMessageByID(ctx, r.Inbox.AccountID, existing)
		return m, e, true, err
	}
	if err != sql.ErrNoRows {
		return model.Message{}, model.Event{}, false, err
	}
	var quota, used int64
	if err = tx.QueryRowContext(ctx, `SELECT storage_quota_bytes,storage_used_bytes FROM accounts WHERE id=?`, r.Inbox.AccountID).Scan(&quota, &used); err != nil {
		return model.Message{}, model.Event{}, false, err
	}
	if quota > 0 && used+r.SizeBytes > quota {
		return model.Message{}, model.Event{}, false, ErrQuota
	}
	threadID, err := findThreadTx(ctx, tx, r.Inbox.AccountID, r.Inbox.ID, r.InReplyTo, r.References)
	if err != nil {
		return model.Message{}, model.Event{}, false, err
	}
	now := nowText()
	if threadID == "" {
		threadID = idgen.New("thr")
		if _, err = tx.ExecContext(ctx, `INSERT INTO threads(id,account_id,inbox_id,subject,created_at,updated_at) VALUES(?,?,?,?,?,?)`, threadID, r.Inbox.AccountID, r.Inbox.ID, r.Subject, now, now); err != nil {
			return model.Message{}, model.Event{}, false, err
		}
	}
	id := idgen.New("msg")
	_, err = tx.ExecContext(ctx, `INSERT INTO messages(id,account_id,inbox_id,thread_id,direction,provider,provider_delivery_id,provider_message_id,rfc_message_id,in_reply_to,references_json,from_name,from_address,to_json,cc_json,envelope_to_json,envelope_recipient,subject,text_body,html_body,raw_path,size_bytes,is_read,is_archived,received_at,created_at,is_spam,auth_results_json,spam_reason) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,0,?,?,?,?,?)`, id, r.Inbox.AccountID, r.Inbox.ID, threadID, "inbound", r.Provider, r.ProviderDeliveryID, r.ProviderMessageID, r.RFCMessageID, r.InReplyTo, jsonString(r.References), r.From.Name, normalizeAddress(r.From.Address), jsonString(r.To), jsonString(r.CC), jsonString(r.EnvelopeTo), normalizeAddress(r.EnvelopeRecipient), r.Subject, r.Text, r.HTML, r.RawPath, r.SizeBytes, timeText(r.ReceivedAt), now, boolInt(r.Spam), firstJSON(r.AuthResults), r.SpamReason)
	if err != nil {
		return model.Message{}, model.Event{}, false, err
	}
	var names []string
	for _, a := range r.Attachments {
		aid := idgen.New("att")
		if _, err = tx.ExecContext(ctx, `INSERT INTO attachments(id,message_id,filename,content_type,size_bytes,part_index,content_id) VALUES(?,?,?,?,?,?,?)`, aid, id, a.Filename, a.ContentType, a.Size, a.PartIndex, a.ContentID); err != nil {
			return model.Message{}, model.Event{}, false, err
		}
		names = append(names, a.Filename)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO message_fts(message_id,account_id,inbox_id,subject,from_address,recipients,body,attachment_names) VALUES(?,?,?,?,?,?,?,?)`, id, r.Inbox.AccountID, r.Inbox.ID, r.Subject, r.From.Address, strings.Join(append(append([]string{}, r.To...), r.CC...), " "), r.Text+" "+stripHTMLText(r.HTML), strings.Join(names, " ")); err != nil {
		return model.Message{}, model.Event{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE accounts SET storage_used_bytes=storage_used_bytes+? WHERE id=?`, r.SizeBytes, r.Inbox.AccountID); err != nil {
		return model.Message{}, model.Event{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE threads SET updated_at=? WHERE id=?`, now, threadID); err != nil {
		return model.Message{}, model.Event{}, false, err
	}
	ev, err := insertEventTx(ctx, tx, r.Inbox.AccountID, r.Inbox.ID, "message.received", id, receivedPayload(id, r.Inbox.ID, threadID, r.Spam, r.SpamReason, r.AuthResults))
	if err != nil {
		return model.Message{}, model.Event{}, false, err
	}
	if strings.TrimSpace(r.DeliveryFingerprint) != "" {
		disp := string(DispositionStored)
		if r.Spam {
			disp = string(DispositionSpam)
		}
		reason := r.SpamReason
		if reason == "" {
			reason = "accepted"
		}
		if err = recordMXReceiptTx(ctx, tx, MXReceipt{
			AccountID: r.Inbox.AccountID, Provider: r.Provider, EnvelopeRecipient: r.EnvelopeRecipient,
			DeliveryFingerprint: r.DeliveryFingerprint, Disposition: disp, MessageID: id, Reason: reason,
			ExpiresAt: receiptExpiry(r.ReceiptTTL),
		}); err != nil {
			return model.Message{}, model.Event{}, false, err
		}
	}
	m, err := s.getMessageTx(ctx, tx, r.Inbox.AccountID, id)
	if err != nil {
		return model.Message{}, model.Event{}, false, err
	}
	if err = tx.Commit(); err != nil {
		// The commit may have reached disk before reporting an error. Only
		// treat it as failed if the row is genuinely absent.
		if existing, gerr := s.GetMessageByID(context.WithoutCancel(ctx), r.Inbox.AccountID, id); gerr == nil {
			return existing, ev, false, nil
		}
		return model.Message{}, model.Event{}, false, err
	}
	return m, ev, false, nil
}

func findThreadTx(ctx context.Context, tx *sql.Tx, accountID, inboxID, inReply string, refs []string) (string, error) {
	ids := make([]string, 0, len(refs)+1)
	if strings.TrimSpace(inReply) != "" {
		ids = append(ids, strings.TrimSpace(inReply))
	}
	for _, r := range refs {
		if strings.TrimSpace(r) != "" {
			ids = append(ids, strings.TrimSpace(r))
		}
	}
	if len(ids) == 0 {
		return "", nil
	}
	// Match against both the stored RFC Message-ID and the provider's returned
	// wire id. Providers that assign their own Message-ID (Brevo, Mailgun)
	// return it as provider_message_id; the provider id is promoted to
	// rfc_message_id on send, but the fallback also threads replies to rows
	// persisted before that promotion existed.
	q := `SELECT thread_id FROM messages WHERE account_id=? AND inbox_id=? AND (rfc_message_id IN (` + placeholders(len(ids)) + `) OR provider_message_id IN (` + placeholders(len(ids)) + `)) ORDER BY created_at DESC LIMIT 1`
	args := make([]any, 0, 2+2*len(ids))
	args = append(args, accountID, inboxID)
	for _, v := range ids {
		args = append(args, v)
	}
	for _, v := range ids {
		args = append(args, v)
	}
	var thread string
	err := tx.QueryRowContext(ctx, q, args...).Scan(&thread)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return thread, err
}

func firstJSON(v string) string {
	if strings.TrimSpace(v) == "" {
		return "{}"
	}
	return v
}

// receivedPayload builds the message.received event payload, including the
// Spam flag and auth evidence so durable consumers and Relay can opt out of
// automated processing for Spam (Relay rebuilds its own payload from the
// message, so the flag is also persisted on the row).
func receivedPayload(messageID, inboxID, threadID string, spam bool, reason, authResults string) map[string]any {
	p := map[string]any{"message_id": messageID, "inbox_id": inboxID, "thread_id": threadID}
	if spam {
		p["is_spam"] = true
		p["spam_reason"] = reason
	}
	if strings.TrimSpace(authResults) != "" && authResults != "{}" {
		p["auth_results"] = json.RawMessage(authResults)
	}
	return p
}

func insertEventTx(ctx context.Context, tx *sql.Tx, accountID, inboxID, typ, entity string, payload map[string]any) (model.Event, error) {
	now := nowText()
	res, err := tx.ExecContext(ctx, `INSERT INTO events(account_id,inbox_id,type,entity_id,payload_json,created_at) VALUES(?,?,?,?,?,?)`, accountID, nullString(inboxID), typ, entity, jsonString(payload), now)
	if err != nil {
		return model.Event{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return model.Event{}, err
	}
	return model.Event{ID: id, Cursor: fmt.Sprintf("evt_%d", id), AccountID: accountID, InboxID: inboxID, Type: typ, EntityID: entity, Payload: payload, CreatedAt: parseTime(now)}, nil
}
func stripHTMLText(v string) string {
	r := strings.NewReplacer("<br>", " ", "<br/>", " ", "<br />", " ", "</p>", " ", "</div>", " ")
	v = r.Replace(v)
	var b strings.Builder
	inside := false
	for _, ch := range v {
		if ch == '<' {
			inside = true
			continue
		}
		if ch == '>' {
			inside = false
			continue
		}
		if !inside {
			b.WriteRune(ch)
		}
	}
	return b.String()
}

func scanMessage(row interface{ Scan(...any) error }) (model.Message, error) {
	var m model.Message
	var refs, to, cc, bcc, env, labels, created string
	var received, sent sql.NullString
	var read, arch int
	var has, internal, spam int
	var authResults string
	err := row.Scan(&m.ID, &m.AccountID, &m.InboxID, &m.ThreadID, &m.Direction, &m.Provider, &m.ProviderMessageID, &m.RFCMessageID, &m.InReplyTo, &refs, &m.From.Name, &m.From.Address, &to, &cc, &bcc, &env, &m.Client, &m.Subject, &m.Text, &m.HTML, &m.RawPath, &m.SizeBytes, &read, &arch, &received, &sent, &created, &has, &m.Status, &m.Attempts, &m.LastError, &m.NextRetry, &m.IdemKey, &internal, &labels, &spam, &authResults, &m.SpamReason)
	if err != nil {
		return m, err
	}
	m.Internal = internal != 0
	m.Spam = spam != 0
	if strings.TrimSpace(authResults) != "" && authResults != "{}" {
		m.AuthResults = json.RawMessage(authResults)
	}
	m.References = decodeStrings(refs)
	m.To = decodeStrings(to)
	m.CC = decodeStrings(cc)
	m.BCC = decodeStrings(bcc)
	m.EnvelopeTo = decodeStrings(env)
	m.Read = read != 0
	m.Archived = arch != 0
	m.ReceivedAt = nullableTime(received)
	m.SentAt = nullableTime(sent)
	m.CreatedAt = parseTime(created)
	m.HasAttachments = has != 0
	m.Labels = decodeStrings(labels)
	sort.Slice(m.Labels, func(i, j int) bool {
		return strings.ToLower(m.Labels[i]) < strings.ToLower(m.Labels[j])
	})
	return m, nil
}

const messageSelect = `SELECT m.id,m.account_id,m.inbox_id,m.thread_id,m.direction,m.provider,m.provider_message_id,m.rfc_message_id,m.in_reply_to,m.references_json,m.from_name,m.from_address,m.to_json,m.cc_json,m.bcc_json,m.envelope_to_json,m.client_label,m.subject,m.text_body,m.html_body,m.raw_path,m.size_bytes,m.is_read,m.is_archived,m.received_at,m.sent_at,m.created_at,EXISTS(SELECT 1 FROM attachments a WHERE a.message_id=m.id),m.status,m.attempts,m.last_error,m.next_attempt_at,m.idem_key,m.internal,COALESCE((SELECT json_group_array(label) FROM message_labels WHERE message_id=m.id),'[]'),m.is_spam,m.auth_results_json,m.spam_reason`

func (s *Store) GetMessageByID(ctx context.Context, accountID, id string) (model.Message, error) {
	m, err := scanMessage(s.read.QueryRowContext(ctx, messageSelect+` FROM messages m WHERE m.id=? AND m.account_id=?`, id, accountID))
	if err == sql.ErrNoRows {
		return m, ErrNotFound
	}
	return m, err
}

// getMessageTx reads a message on the caller's transaction, so a write path can
// return the exact committed row without a fallible read after commit.
func (s *Store) getMessageTx(ctx context.Context, tx *sql.Tx, accountID, id string) (model.Message, error) {
	m, err := scanMessage(tx.QueryRowContext(ctx, messageSelect+` FROM messages m WHERE m.id=? AND m.account_id=?`, id, accountID))
	if err == sql.ErrNoRows {
		return m, ErrNotFound
	}
	return m, err
}

// MessageAccountID resolves the account id for a message id. Used by the
// outbox worker, which claims messages without a principal.
func (s *Store) MessageAccountID(ctx context.Context, id string) (string, error) {
	var accountID string
	err := s.read.QueryRowContext(ctx, `SELECT account_id FROM messages WHERE id=?`, id).Scan(&accountID)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return accountID, err
}
func (s *Store) GetMessage(ctx context.Context, p model.Principal, id string) (model.Message, error) {
	m, err := s.GetMessageByID(ctx, p.AccountID, id)
	if err != nil {
		return m, err
	}
	if !p.CanRead(m.InboxID) || m.Internal {
		return model.Message{}, ErrForbidden
	}
	return m, nil
}

type MessageFilter struct {
	InboxID, ThreadID, From, To, Direction string
	Unread, HasAttachment                  *bool
	// Labels, when non-empty, restricts results to messages carrying every
	// listed label (AND). Matching is case-insensitive.
	Labels []string
	// SpamOnly lists only Spam messages; IncludeSpam includes both. The default
	// (both false) excludes Spam from ordinary reads. The explicit Spam view
	// sets SpamOnly.
	SpamOnly    bool
	IncludeSpam bool
	Before      string
	Limit       int
}

func (s *Store) ListMessages(ctx context.Context, p model.Principal, f MessageFilter) ([]model.Message, error) {
	q := messageSelect + ` FROM messages m WHERE m.account_id=? AND m.internal=0`
	args := []any{p.AccountID}
	q += spamClause("m", f.SpamOnly, f.IncludeSpam)
	if f.InboxID != "" {
		if !p.CanRead(f.InboxID) {
			return nil, ErrForbidden
		}
		q += ` AND m.inbox_id=?`
		args = append(args, f.InboxID)
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
	if f.ThreadID != "" {
		q += ` AND m.thread_id=?`
		args = append(args, f.ThreadID)
	}
	if f.Direction != "" {
		q += ` AND m.direction=?`
		args = append(args, f.Direction)
	}
	if f.From != "" {
		q += ` AND m.from_address LIKE ?`
		args = append(args, "%"+normalizeAddress(f.From)+"%")
	}
	if f.To != "" {
		q += ` AND (m.to_json LIKE ? OR m.cc_json LIKE ?)`
		like := "%" + normalizeAddress(f.To) + "%"
		args = append(args, like, like)
	}
	if f.Unread != nil {
		q += ` AND m.is_read=?`
		args = append(args, boolInt(!*f.Unread))
	}
	if f.HasAttachment != nil {
		if *f.HasAttachment {
			q += ` AND EXISTS(SELECT 1 FROM attachments aa WHERE aa.message_id=m.id)`
		} else {
			q += ` AND NOT EXISTS(SELECT 1 FROM attachments aa WHERE aa.message_id=m.id)`
		}
	}
	for _, label := range f.Labels {
		if label = strings.TrimSpace(label); label == "" {
			continue
		}
		q += ` AND EXISTS(SELECT 1 FROM message_labels ml WHERE ml.message_id=m.id AND ml.label=?)`
		args = append(args, label)
	}
	if f.Before != "" {
		var beforeCreated string
		err := s.read.QueryRowContext(ctx, `SELECT created_at FROM messages WHERE id=? AND account_id=?`, f.Before, p.AccountID).Scan(&beforeCreated)
		if err == nil {
			q += ` AND (m.created_at < ? OR (m.created_at = ? AND m.rowid < (SELECT rowid FROM messages WHERE id=? AND account_id=?)))`
			args = append(args, beforeCreated, beforeCreated, f.Before, p.AccountID)
		} else if err != sql.ErrNoRows {
			return nil, err
		}
	}
	limit := f.Limit
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
	return out, rows.Err()
}

// CountSpam returns the number of Spam messages in an inbox, used by the
// mailbox Spam tab count.
func (s *Store) CountSpam(ctx context.Context, p model.Principal, inboxID string) (int, error) {
	if !p.CanRead(inboxID) {
		return 0, ErrForbidden
	}
	var n int
	err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE account_id=? AND inbox_id=? AND internal=0 AND is_spam=1`, p.AccountID, inboxID).Scan(&n)
	return n, err
}

func (s *Store) UnreadCounts(ctx context.Context, p model.Principal) (map[string]int, error) {
	q := `SELECT inbox_id,COUNT(*) FROM messages WHERE account_id=? AND is_read=0 AND is_archived=0 AND direction='inbound' AND is_spam=0`
	args := []any{p.AccountID}
	if !p.Admin {
		ids := principalInboxIDs(p)
		if len(ids) == 0 {
			return map[string]int{}, nil
		}
		q += ` AND inbox_id IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	q += ` GROUP BY inbox_id`
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var inboxID string
		var n int
		if err = rows.Scan(&inboxID, &n); err != nil {
			return nil, err
		}
		out[inboxID] = n
	}
	return out, rows.Err()
}

// MessageSizesByInbox returns the stored message bytes for each inbox.
func (s *Store) MessageSizesByInbox(ctx context.Context, p model.Principal) (map[string]int64, error) {
	q := `SELECT inbox_id,COALESCE(SUM(size_bytes),0) FROM messages WHERE account_id=? AND internal=0`
	args := []any{p.AccountID}
	if !p.Admin {
		ids := principalInboxIDs(p)
		if len(ids) == 0 {
			return map[string]int64{}, nil
		}
		q += ` AND inbox_id IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	q += ` GROUP BY inbox_id`
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var inboxID string
		var size int64
		if err = rows.Scan(&inboxID, &size); err != nil {
			return nil, err
		}
		out[inboxID] = size
	}
	return out, rows.Err()
}

func (s *Store) UpdateMessageState(ctx context.Context, p model.Principal, id string, read, archived *bool) error {
	m, err := s.GetMessage(ctx, p, id)
	if err != nil {
		return err
	}
	if !p.CanAssist(m.InboxID) {
		return ErrForbidden
	}
	if read != nil {
		_, err = s.write.ExecContext(ctx, `UPDATE messages SET is_read=? WHERE id=?`, boolInt(*read), id)
		if err != nil {
			return err
		}
	}
	if archived != nil {
		_, err = s.write.ExecContext(ctx, `UPDATE messages SET is_archived=? WHERE id=?`, boolInt(*archived), id)
	}
	return err
}

// ReplaceMessageLabels sets the exact label set on a message. It requires
// Assistant or Owner on the message's inbox. Unknown labels are created
// implicitly (there is no catalogue); an empty slice clears all labels. It
// returns the durable message.labels_changed event.
func (s *Store) ReplaceMessageLabels(ctx context.Context, p model.Principal, id string, labels []string) (model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.Event{}, err
	}
	defer tx.Rollback()
	m, err := s.getMessageTx(ctx, tx, p.AccountID, id)
	if err != nil {
		return model.Event{}, err
	}
	if !p.CanAssist(m.InboxID) {
		return model.Event{}, ErrForbidden
	}
	seen := map[string]bool{}
	cleaned := make([]string, 0, len(labels))
	for _, raw := range labels {
		v, ok := model.NormalizeLabel(raw)
		if !ok {
			return model.Event{}, fmt.Errorf("invalid label: %q", raw)
		}
		key := strings.ToLower(v)
		if seen[key] {
			continue
		}
		seen[key] = true
		cleaned = append(cleaned, v)
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM message_labels WHERE message_id=?`, id); err != nil {
		return model.Event{}, err
	}
	now := nowText()
	for _, v := range cleaned {
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO message_labels(message_id,label,created_at) VALUES(?,?,?)`, id, v, now); err != nil {
			return model.Event{}, err
		}
	}
	sort.Strings(cleaned)
	ev, err := insertEventTx(ctx, tx, p.AccountID, m.InboxID, model.EventMessageLabelsChanged, id, map[string]any{"message_id": id, "inbox_id": m.InboxID, "thread_id": m.ThreadID, "labels": cleaned})
	if err != nil {
		return model.Event{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Event{}, err
	}
	return ev, nil
}

// ListLabels returns the distinct labels visible to the principal, ordered
// case-insensitively. There is no catalogue; the set is derived from usage.
func (s *Store) ListLabels(ctx context.Context, p model.Principal) ([]string, error) {
	q := `SELECT DISTINCT ml.label FROM message_labels ml JOIN messages m ON m.id=ml.message_id WHERE m.account_id=? AND m.internal=0`
	args := []any{p.AccountID}
	if !p.Admin {
		ids := principalInboxIDs(p)
		if len(ids) == 0 {
			return []string{}, nil
		}
		q += ` AND m.inbox_id IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	q += ` ORDER BY ml.label COLLATE NOCASE`
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) DeleteMessage(ctx context.Context, p model.Principal, id string) (string, int64, model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, model.Event{}, err
	}
	defer tx.Rollback()
	// Load and authorise inside the serialized write transaction so a
	// concurrent duplicate delete cannot subtract storage twice or emit a
	// second event.
	m, err := s.getMessageTx(ctx, tx, p.AccountID, id)
	if err != nil {
		return "", 0, model.Event{}, err
	}
	if !p.CanAssist(m.InboxID) {
		return "", 0, model.Event{}, ErrForbidden
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM message_fts WHERE message_id=?`, id); err != nil {
		return "", 0, model.Event{}, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id=? AND account_id=?`, id, p.AccountID)
	if err != nil {
		return "", 0, model.Event{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", 0, model.Event{}, ErrNotFound
	}
	if _, err = tx.ExecContext(ctx, `UPDATE accounts SET storage_used_bytes=MAX(0,storage_used_bytes-?) WHERE id=?`, m.SizeBytes, p.AccountID); err != nil {
		return "", 0, model.Event{}, err
	}
	ev, err := insertEventTx(ctx, tx, p.AccountID, m.InboxID, "message.deleted", id, map[string]any{"message_id": id})
	if err != nil {
		return "", 0, model.Event{}, err
	}
	if err = tx.Commit(); err != nil {
		return "", 0, model.Event{}, err
	}
	return m.RawPath, m.SizeBytes, ev, nil
}

// SetMessageSpam moves a message between Spam and non-Spam and commits the
// durable state-change event with old/new state. It requires Assistant or Owner
// on the message's inbox. A no-op transition still returns the current message
// with no event.
func (s *Store) SetMessageSpam(ctx context.Context, p model.Principal, id string, spam bool) (model.Message, *model.Event, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.Message{}, nil, err
	}
	defer tx.Rollback()
	m, err := s.getMessageTx(ctx, tx, p.AccountID, id)
	if err != nil {
		return model.Message{}, nil, err
	}
	if !p.CanAssist(m.InboxID) {
		return model.Message{}, nil, ErrForbidden
	}
	if m.Internal {
		return model.Message{}, nil, ErrNotFound
	}
	if m.Spam == spam {
		if err = tx.Commit(); err != nil {
			return model.Message{}, nil, err
		}
		return m, nil, nil
	}
	reason := ""
	if spam {
		reason = "manual"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE messages SET is_spam=?,spam_reason=? WHERE id=? AND account_id=?`, boolInt(spam), reason, id, p.AccountID); err != nil {
		return model.Message{}, nil, err
	}
	ev, err := insertEventTx(ctx, tx, p.AccountID, m.InboxID, model.EventMessageSpamChanged, id, map[string]any{
		"message_id": id, "inbox_id": m.InboxID, "thread_id": m.ThreadID,
		"old": m.Spam, "new": spam, "is_spam": spam,
	})
	if err != nil {
		return model.Message{}, nil, err
	}
	m.Spam = spam
	m.SpamReason = reason
	if err = tx.Commit(); err != nil {
		return model.Message{}, nil, err
	}
	return m, &ev, nil
}

func (s *Store) ListAttachments(ctx context.Context, p model.Principal, messageID string) ([]model.Attachment, error) {
	m, err := s.GetMessage(ctx, p, messageID)
	if err != nil {
		return nil, err
	}
	rows, err := s.read.QueryContext(ctx, `SELECT id,message_id,filename,content_type,size_bytes,part_index,content_id FROM attachments WHERE message_id=? ORDER BY part_index`, m.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Attachment{}
	for rows.Next() {
		var a model.Attachment
		if err = rows.Scan(&a.ID, &a.MessageID, &a.Filename, &a.ContentType, &a.Size, &a.PartIndex, &a.ContentID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAttachmentsInternal lists a message's attachments without a principal.
func (s *Store) ListAttachmentsInternal(ctx context.Context, accountID, messageID string) ([]model.Attachment, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT id,message_id,filename,content_type,size_bytes,part_index,content_id FROM attachments WHERE message_id=? ORDER BY part_index`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Attachment{}
	for rows.Next() {
		var a model.Attachment
		if err = rows.Scan(&a.ID, &a.MessageID, &a.Filename, &a.ContentType, &a.Size, &a.PartIndex, &a.ContentID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetAttachment(ctx context.Context, p model.Principal, id string) (model.Attachment, model.Message, error) {
	var a model.Attachment
	err := s.read.QueryRowContext(ctx, `SELECT id,message_id,filename,content_type,size_bytes,part_index,content_id FROM attachments WHERE id=?`, id).Scan(&a.ID, &a.MessageID, &a.Filename, &a.ContentType, &a.Size, &a.PartIndex, &a.ContentID)
	if err == sql.ErrNoRows {
		return a, model.Message{}, ErrNotFound
	}
	if err != nil {
		return a, model.Message{}, err
	}
	m, err := s.GetMessage(ctx, p, a.MessageID)
	return a, m, err
}

// ListThreads returns threads with their non-internal, non-Spam message count
// and latest activity. Spam-only threads are suppressed (the LEFT JOIN count
// excludes them and the thread is dropped when it has no visible message);
// hidden Spam must not bump normal thread ordering or choose a visible subject.
func (s *Store) ListThreads(ctx context.Context, p model.Principal, inboxID string, limit int) ([]model.Thread, error) {
	if inboxID != "" && !p.CanRead(inboxID) {
		return nil, ErrForbidden
	}
	q := `SELECT t.id,t.inbox_id,t.subject,count(m.id),t.updated_at FROM threads t LEFT JOIN messages m ON m.thread_id=t.id AND m.internal=0 AND m.is_spam=0 WHERE t.account_id=?`
	args := []any{p.AccountID}
	if inboxID != "" {
		q += ` AND t.inbox_id=?`
		args = append(args, inboxID)
	} else if !p.Admin {
		ids := principalInboxIDs(p)
		if len(ids) == 0 {
			return []model.Thread{}, nil
		}
		q += ` AND t.inbox_id IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	q += ` GROUP BY t.id HAVING count(m.id) > 0 ORDER BY MAX(m.created_at) DESC LIMIT ?`
	if limit <= 0 || limit > limits.PageSizeMaxList {
		limit = limits.PageSizeDefault
	}
	args = append(args, limit)
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Thread{}
	for rows.Next() {
		var t model.Thread
		var last string
		if err = rows.Scan(&t.ID, &t.InboxID, &t.Subject, &t.MessageCount, &last); err != nil {
			return nil, err
		}
		t.LastMessageAt = parseTime(last)
		out = append(out, t)
	}
	return out, rows.Err()
}

// spamClause returns the SQL fragment that applies the Spam visibility rule:
// an explicit Spam view selects only Spam, an "include" read selects
// everything, and the ordinary path excludes Spam. alias is the messages table
// alias (or empty).
func spamClause(alias string, spamOnly, includeSpam bool) string {
	prefix := ""
	if alias != "" {
		prefix = alias + "."
	}
	switch {
	case spamOnly:
		return " AND " + prefix + "is_spam=1"
	case includeSpam:
		return ""
	default:
		return " AND " + prefix + "is_spam=0"
	}
}

func ftsQuery(q string) string {
	fields := strings.Fields(q)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.ReplaceAll(f, `"`, `""`)
		out = append(out, `"`+f+`"`)
	}
	return strings.Join(out, " AND ")
}

type BlockedRecord struct {
	AccountID, InboxID, Provider, ProviderDeliveryID string
	EnvelopeRecipient                                string
	From                                             model.Address
	To                                               []string
	Subject, Reason                                  string
	SizeBytes                                        int64
	ReceivedAt                                       time.Time
}

// CommitBlockedInbound records metadata for mail rejected by an inbox's
// allowed-senders list. It deliberately writes no message row, no raw file, no
// FTS entry and no event, so the blocked mail can never reach the inbox view,
// the API or the relay connector.
func (s *Store) CommitBlockedInbound(ctx context.Context, r BlockedRecord) (model.BlockedMessage, bool, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.BlockedMessage{}, false, err
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT id FROM blocked_messages WHERE account_id=? AND provider=? AND envelope_recipient=? AND provider_delivery_id=?`, r.AccountID, r.Provider, r.EnvelopeRecipient, r.ProviderDeliveryID).Scan(&existing)
	if err == nil {
		_ = tx.Rollback()
		m, e := s.GetBlockedMessage(ctx, r.AccountID, existing)
		return m, true, e
	}
	if err != sql.ErrNoRows {
		return model.BlockedMessage{}, false, err
	}
	id := idgen.New("blk")
	now := nowText()
	if _, err = tx.ExecContext(ctx, `INSERT INTO blocked_messages(id,account_id,inbox_id,provider,provider_delivery_id,envelope_recipient,from_name,from_address,to_json,subject,size_bytes,reason,received_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, r.AccountID, r.InboxID, r.Provider, r.ProviderDeliveryID, normalizeAddress(r.EnvelopeRecipient), r.From.Name, normalizeAddress(r.From.Address), jsonString(r.To), r.Subject, r.SizeBytes, r.Reason, timeText(r.ReceivedAt), now); err != nil {
		return model.BlockedMessage{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return model.BlockedMessage{}, false, err
	}
	m, err := s.GetBlockedMessage(ctx, r.AccountID, id)
	return m, false, err
}

const blockedSelect = `SELECT id,account_id,inbox_id,from_name,from_address,to_json,subject,size_bytes,reason,received_at,created_at FROM blocked_messages`

func scanBlockedMessage(row interface{ Scan(...any) error }) (model.BlockedMessage, error) {
	var m model.BlockedMessage
	var to string
	var received, created sql.NullString
	err := row.Scan(&m.ID, &m.AccountID, &m.InboxID, &m.From.Name, &m.From.Address, &to, &m.Subject, &m.SizeBytes, &m.Reason, &received, &created)
	if err != nil {
		return m, err
	}
	m.To = decodeStrings(to)
	m.ReceivedAt = nullableTime(received)
	m.CreatedAt = parseTime(created.String)
	return m, nil
}

func (s *Store) GetBlockedMessage(ctx context.Context, accountID, id string) (model.BlockedMessage, error) {
	m, err := scanBlockedMessage(s.read.QueryRowContext(ctx, blockedSelect+` WHERE id=? AND account_id=?`, id, accountID))
	if err == sql.ErrNoRows {
		return m, ErrNotFound
	}
	return m, err
}

func (s *Store) ListBlockedMessages(ctx context.Context, p model.Principal, limit int) ([]model.BlockedMessage, error) {
	q := blockedSelect + ` WHERE account_id=?`
	args := []any{p.AccountID}
	if !p.Admin {
		ids := principalInboxIDs(p)
		if len(ids) == 0 {
			return []model.BlockedMessage{}, nil
		}
		q += ` AND inbox_id IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if limit <= 0 || limit > limits.PageSizeMaxList {
		limit = limits.PageSizeDefault
	}
	q += ` ORDER BY created_at DESC, rowid DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.BlockedMessage{}
	for rows.Next() {
		m, err := scanBlockedMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) SearchMessages(ctx context.Context, p model.Principal, q, inboxID string, limit int) ([]model.Message, error) {
	return s.SearchMessagesFiltered(ctx, p, q, MessageFilter{InboxID: inboxID, Limit: limit})
}

// SearchMessagesFiltered searches FTS content and applies the same optional
// filters as ListMessages (from, to, before, has_attachment).
func (s *Store) SearchMessagesFiltered(ctx context.Context, p model.Principal, q string, f MessageFilter) ([]model.Message, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return []model.Message{}, nil
	}
	sqlq := messageSelect + ` FROM message_fts JOIN messages m ON m.id=message_fts.message_id WHERE message_fts MATCH ? AND m.account_id=? AND m.internal=0`
	args := []any{ftsQuery(q), p.AccountID}
	sqlq += spamClause("m", f.SpamOnly, f.IncludeSpam)
	if f.InboxID != "" {
		if !p.CanRead(f.InboxID) {
			return nil, ErrForbidden
		}
		sqlq += ` AND m.inbox_id=?`
		args = append(args, f.InboxID)
	} else if !p.Admin {
		ids := principalInboxIDs(p)
		if len(ids) == 0 {
			return []model.Message{}, nil
		}
		sqlq += ` AND m.inbox_id IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	if f.From != "" {
		sqlq += ` AND m.from_address LIKE ?`
		args = append(args, "%"+normalizeAddress(f.From)+"%")
	}
	if f.To != "" {
		sqlq += ` AND (m.to_json LIKE ? OR m.cc_json LIKE ?)`
		like := "%" + normalizeAddress(f.To) + "%"
		args = append(args, like, like)
	}
	if f.HasAttachment != nil {
		if *f.HasAttachment {
			sqlq += ` AND EXISTS(SELECT 1 FROM attachments aa WHERE aa.message_id=m.id)`
		} else {
			sqlq += ` AND NOT EXISTS(SELECT 1 FROM attachments aa WHERE aa.message_id=m.id)`
		}
	}
	for _, label := range f.Labels {
		if label = strings.TrimSpace(label); label == "" {
			continue
		}
		sqlq += ` AND EXISTS(SELECT 1 FROM message_labels ml WHERE ml.message_id=m.id AND ml.label=?)`
		args = append(args, label)
	}
	if f.Before != "" {
		var beforeCreated string
		err := s.read.QueryRowContext(ctx, `SELECT created_at FROM messages WHERE id=? AND account_id=?`, f.Before, p.AccountID).Scan(&beforeCreated)
		if err == nil {
			sqlq += ` AND (m.created_at < ? OR (m.created_at = ? AND m.rowid < (SELECT rowid FROM messages WHERE id=? AND account_id=?)))`
			args = append(args, beforeCreated, beforeCreated, f.Before, p.AccountID)
		} else if err != sql.ErrNoRows {
			return nil, err
		}
	}
	limit := f.Limit
	if limit <= 0 || limit > limits.PageSizeMaxList {
		limit = limits.PageSizeDefault
	}
	sqlq += ` ORDER BY m.created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.read.QueryContext(ctx, sqlq, args...)
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
	return out, rows.Err()
}
