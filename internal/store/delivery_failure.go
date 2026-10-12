package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/dellarb/mailmoose/internal/idgen"
	"github.com/dellarb/mailmoose/internal/model"
)

// deliveryFailure is the material needed to compose a local delivery-failure
// notice for a message that failed terminally.
type deliveryFailure struct {
	AccountID   string
	InboxID     string
	MessageID   string
	Provider    string
	FromAddress string
	To          []string
	Subject     string
	LastError   string
	Attempts    int
	// domainID is the sending domain (possibly empty for a standalone inbox);
	// it is used only to derive the notice's MAILER-DAEMON sender domain.
	domainID string
}

// mailerDaemonFallbackDomain is the fallback sender domain used when a message
// has no associated managed domain (a standalone inbox, or a domain deleted
// while a send was in flight). It is deliberately non-routable so a notice can
// never be mistaken for a real address.
const mailerDaemonFallbackDomain = "mailer-daemon.invalid"

// writeDeliveryFailureTx records a delivery-failure notice for a message that
// failed terminally, inside the caller's transaction. It performs the two
// coordinated writes that make a terminal failure visible:
//
//  1. a normal inbound message (direction 'inbound', from MAILER-DAEMON@<domain>
//     to the failed message's sender) is written into the same inbox, so the
//     failure appears in the mailbox, in search and in retention exactly like a
//     traditional bounce; and
//  2. the failed outbound message is trashed, so it leaves the outbox queue
//     (which lists only pending/failed rows) instead of sitting there forever.
//
// It returns the notice message id and the durable events to publish. The
// notice is never enqueued for delivery and is never sent over any transport: it
// is written straight into the store as inbound, so no daemon-to-daemon loop is
// possible.
//
// It is a no-op (empty id, no events) when a notice for this exact failure has
// already been written, so re-marking the same terminal failure cannot spam the
// mailbox. Callers must invoke it only for a terminal failure, never for a
// retryable one that has been rescheduled.
func (s *Store) writeDeliveryFailureTx(ctx context.Context, tx *sql.Tx, f deliveryFailure) (string, []model.Event, error) {
	// One notice per failed message: the original is trashed as part of writing
	// the notice, so a message that is already trashed (or gone) has been
	// notified already.
	var deleted sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT deleted_at FROM messages WHERE id=? AND account_id=?`, f.MessageID, f.AccountID).Scan(&deleted)
	if err == sql.ErrNoRows {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	if deleted.Valid && deleted.String != "" {
		return "", nil, nil
	}

	domain := mailerDaemonFallbackDomain
	if strings.TrimSpace(f.domainID) != "" {
		var name string
		switch err := tx.QueryRowContext(ctx, `SELECT name FROM domains WHERE id=? AND account_id=?`, f.domainID, f.AccountID).Scan(&name); err {
		case nil:
			if strings.TrimSpace(name) != "" {
				domain = strings.TrimSpace(name)
			}
		case sql.ErrNoRows:
			// Domain deleted mid-send; keep the non-routable fallback.
		default:
			return "", nil, err
		}
	}
	fromAddress := "MAILER-DAEMON@" + domain

	// The notice goes to the failed message's sender (the traditional bounce
	// recipient); the failed recipient is named in the body. When the outbound
	// message carries no From, fall back to the inbox's own address so the notice
	// still lands somewhere a human reads.
	var to []string
	if a := normalizeAddress(f.FromAddress); a != "" {
		to = []string{a}
	}
	if len(to) == 0 {
		var addr string
		if err := tx.QueryRowContext(ctx, `SELECT address FROM inboxes WHERE id=? AND account_id=?`, f.InboxID, f.AccountID).Scan(&addr); err != nil && err != sql.ErrNoRows {
			return "", nil, err
		}
		if a := normalizeAddress(addr); a != "" {
			to = []string{a}
		}
	}

	subject := "Undeliverable"
	if s := strings.TrimSpace(f.Subject); s != "" {
		subject = "Undeliverable: " + s
	}
	textBody, htmlBody := deliveryFailureBody(f, fromAddress)

	now := nowText()
	threadID := idgen.New("thr")
	if _, err = tx.ExecContext(ctx, `INSERT INTO threads(id,account_id,inbox_id,subject,created_at,updated_at) VALUES(?,?,?,?,?,?)`, threadID, f.AccountID, f.InboxID, subject, now, now); err != nil {
		return "", nil, err
	}

	id := idgen.New("msg")
	size := int64(len(textBody) + len(htmlBody))
	_, err = tx.ExecContext(ctx, `INSERT INTO messages(id,account_id,inbox_id,thread_id,direction,provider,from_address,to_json,envelope_to_json,envelope_from,subject,text_body,html_body,size_bytes,is_read,received_at,created_at,is_spam) VALUES(?,?,?,?,'inbound','mailer-daemon',?,?,?,?,?,?,?,?,0,?,?,0)`,
		id, f.AccountID, f.InboxID, threadID, fromAddress, jsonString(to), jsonString(to), fromAddress, subject, textBody, htmlBody, size, now, now)
	if err != nil {
		return "", nil, err
	}
	if err := cacheLocalPreview(ctx, tx, id, textBody, htmlBody); err != nil {
		return "", nil, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO message_fts(message_id,account_id,inbox_id,subject,from_address,recipients,body,attachment_names) VALUES(?,?,?,?,?,?,?,'')`, id, f.AccountID, f.InboxID, subject, fromAddress, strings.Join(to, " "), textBody); err != nil {
		return "", nil, err
	}
	if err = adjustStorageTx(ctx, tx, f.AccountID, f.InboxID, size); err != nil {
		return "", nil, err
	}
	ev, err := insertEventTx(ctx, tx, f.AccountID, f.InboxID, "message.received", id, receivedPayload(id, f.InboxID, threadID, false, "", ""))
	if err != nil {
		return "", nil, err
	}
	events := []model.Event{ev}

	// Clear the failed original from the outbox. It stays recoverable in Trash.
	res, err := tx.ExecContext(ctx, `UPDATE messages SET deleted_at=? WHERE id=? AND account_id=? AND deleted_at IS NULL`, now, f.MessageID, f.AccountID)
	if err != nil {
		return "", nil, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		tev, terr := insertEventTx(ctx, tx, f.AccountID, f.InboxID, "message.trashed", f.MessageID, map[string]any{"message_id": f.MessageID, "inbox_id": f.InboxID})
		if terr != nil {
			return "", nil, terr
		}
		events = append(events, tev)
	}
	return id, events, nil
}

// deliveryFailureBody renders the notice's text and HTML parts. The body names
// the recipient, the provider path, the diagnostic and the original message id,
// so the failure is actionable without opening a log. The diagnostic is our own
// outbound error text (one line, already lowercased and free of provider free
// text by the transport adapters), so it is safe to include verbatim.
func deliveryFailureBody(f deliveryFailure, fromAddress string) (string, string) {
	recipients := strings.Join(f.To, ", ")
	if recipients == "" {
		recipients = "(unknown recipient)"
	}
	provider := strings.TrimSpace(f.Provider)
	if provider == "" {
		provider = "outbound provider"
	}
	diagnostic := strings.TrimSpace(f.LastError)
	if diagnostic == "" {
		diagnostic = "the delivery attempt failed and no further retries are scheduled"
	} else {
		diagnostic = strings.Join(strings.Fields(diagnostic), " ")
	}
	attempts := f.Attempts
	if attempts < 1 {
		attempts = 1
	}
	subject := strings.TrimSpace(f.Subject)
	if subject == "" {
		subject = "(no subject)"
	}

	var b strings.Builder
	b.WriteString("This is an automatic delivery-failure notice.\r\n\r\n")
	b.WriteString("MailMoose could not deliver the following message and will not retry it.\r\n\r\n")
	fmt.Fprintf(&b, "  To:       %s\r\n", recipients)
	fmt.Fprintf(&b, "  Subject:  %s\r\n", subject)
	fmt.Fprintf(&b, "  Sender:   %s\r\n", f.FromAddress)
	fmt.Fprintf(&b, "  Message:  %s\r\n", f.MessageID)
	fmt.Fprintf(&b, "  Attempts: %d\r\n\r\n", attempts)
	fmt.Fprintf(&b, "Delivery path: %s\r\n\r\n", provider)
	b.WriteString("Diagnostic:\r\n")
	fmt.Fprintf(&b, "  %s\r\n\r\n", diagnostic)
	b.WriteString("The original message has been moved out of the outbox and is retained in Trash.\r\n")
	fmt.Fprintf(&b, "This notice was generated locally by MailMoose; do not reply to %s.\r\n", fromAddress)

	html := "<p>This is an automatic delivery-failure notice.</p>" +
		"<p>MailMoose could not deliver the following message and will not retry it.</p>" +
		"<table>" +
		"<tr><td><strong>To</strong></td><td>" + htmlEscape(recipients) + "</td></tr>" +
		"<tr><td><strong>Subject</strong></td><td>" + htmlEscape(subject) + "</td></tr>" +
		"<tr><td><strong>Sender</strong></td><td>" + htmlEscape(f.FromAddress) + "</td></tr>" +
		"<tr><td><strong>Message</strong></td><td>" + htmlEscape(f.MessageID) + "</td></tr>" +
		fmt.Sprintf("<tr><td><strong>Attempts</strong></td><td>%d</td></tr>", attempts) +
		"</table>" +
		"<p>Delivery path: " + htmlEscape(provider) + "</p>" +
		"<p>Diagnostic: " + htmlEscape(diagnostic) + "</p>" +
		"<p>The original message has been moved out of the outbox and is retained in Trash.</p>"
	return b.String(), html
}

func htmlEscape(v string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(v)
}
