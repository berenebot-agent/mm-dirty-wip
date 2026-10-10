package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/netutil"
)

type WebhookWorker struct {
	svc    *Service
	client *http.Client
}

func NewWebhookWorker(svc *Service) *WebhookWorker {
	return &WebhookWorker{svc: svc, client: netutil.HTTPClientLong()}
}

// SetHTTPClient replaces the outbound client (used by tests, which cannot dial
// a public destination).
func (w *WebhookWorker) SetHTTPClient(c *http.Client) { w.client = c }

// RunOnce delivers the head event that is due for one webhook client. Events
// whose message is no longer deliverable — currently Spam, internal, or since
// deleted — are terminally skipped with no network call and the cursor advances,
// so they cannot pin the head of the queue forever. RunOnce drains any run of
// such events and then dispatches the first genuinely deliverable one; it
// returns store.ErrNotFound when nothing is left to deliver.
func (w *WebhookWorker) RunOnce(ctx context.Context) error {
	for {
		d, err := w.svc.Store.NextWebhookDelivery(ctx, time.Now().UTC())
		if err != nil {
			return err
		}
		m, arrival, skip, err := w.deliverableMessage(ctx, d)
		if err != nil {
			return err
		}
		if skip {
			if err := w.svc.Store.RecordWebhookSkipped(ctx, d.Client.ID, d.EventID); err != nil {
				return err
			}
			continue
		}
		return w.dispatch(ctx, d, m, arrival)
	}
}

// deliverableMessage reports whether the event at the head of the queue should
// be delivered. It applies the accepted policy (D062): both payload modes skip
// currently-Spam mail. It also skips a message that is internal (workflow mail
// hidden from every read surface) or has been trashed or purged, none of which
// can be forwarded. The event's own spam state is authoritative for a
// message.spam_state_changed event, so a stale "moved to Spam" transition is
// skipped even if the message was later released (which enqueues its own,
// deliverable event).
//
// A remote-arrival event (payload remote=true) has no local message row: it is
// resolved to its durable arrival record instead. A control or handoff-skipped
// arrival is never forwarded.
func (w *WebhookWorker) deliverableMessage(ctx context.Context, d store.PendingWebhookDelivery) (model.Message, *store.RemoteArrival, bool, error) {
	if IsRemotePayload(d.Payload) {
		arrival, err := w.svc.Store.GetRemoteArrival(ctx, d.Client.AccountID, d.EntityID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return model.Message{}, nil, true, nil
			}
			return model.Message{}, nil, false, err
		}
		if arrival.Control || arrival.DeliveryState == store.RemoteArrivalSkipped {
			return model.Message{}, nil, true, nil
		}
		if arrival.InboxID != d.Client.InboxID {
			return model.Message{}, nil, true, nil
		}
		m := model.Message{
			ID:           arrival.ID,
			InboxID:      arrival.InboxID,
			Direction:    "inbound",
			From:         model.Address{Name: arrival.FromName, Address: arrival.FromAddress},
			Subject:      arrival.Subject,
			SizeBytes:    arrival.SizeBytes,
			RFCMessageID: arrival.RFCMessageID,
			CreatedAt:    arrival.CreatedAt,
		}
		return m, &arrival, false, nil
	}
	if eventIsSpam(d.Payload) {
		return model.Message{}, nil, true, nil
	}
	m, err := w.svc.Store.GetMessageByID(ctx, d.Client.AccountID, d.EntityID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return model.Message{}, nil, true, nil
		}
		return model.Message{}, nil, false, err
	}
	if m.Internal || m.Spam || m.DeletedAt != nil {
		return model.Message{}, nil, true, nil
	}
	return m, nil, false, nil
}

// eventIsSpam reports whether a durable event payload marks the message as Spam.
// message.received sets is_spam only when the message was classified Spam;
// message.spam_state_changed sets is_spam (and new) to the state the transition
// established. A payload that cannot be parsed is treated as not Spam and the
// current message state decides.
func eventIsSpam(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	var p struct {
		IsSpam *bool `json:"is_spam"`
		New    *bool `json:"new"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return false
	}
	if p.IsSpam != nil {
		return *p.IsSpam
	}
	if p.New != nil {
		return *p.New
	}
	return false
}

// dispatch performs one HTTP delivery for an already-validated event. A remote
// arrival's raw body is fetched from the live server on demand (only when the
// client's mode is forward); the fetch is bounded to a transient temp file that is
// removed immediately after the request body is read.
func (w *WebhookWorker) dispatch(ctx context.Context, d store.PendingWebhookDelivery, m model.Message, arrival *store.RemoteArrival) error {
	var body []byte
	if d.Client.Mode == "forward" {
		var raw []byte
		if arrival != nil {
			var err error
			raw, err = w.remoteArrivalRaw(ctx, d.Client.InboxID, *arrival)
			if err != nil {
				return err
			}
		} else {
			path, err := w.svc.dataPath(m.RawPath)
			if err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			raw, err = io.ReadAll(io.LimitReader(f, w.svc.Config.MaxMessageBytes+1))
			_ = f.Close()
			if err != nil {
				return err
			}
			if int64(len(raw)) > w.svc.Config.MaxMessageBytes {
				return fmt.Errorf("raw message exceeds configured maximum")
			}
		}
		body = raw
	} else {
		var err error
		body, err = json.Marshal(remoteAwareJSON(d, arrival))
		if err != nil {
			return err
		}
	}
	secret, err := w.svc.DecryptSecret(d.Client.SecretEncrypted)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.Client.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if d.Client.Mode == "forward" {
		req.Header.Set("Content-Type", "message/rfc822")
		// The relay-supplied envelope metadata is carried as two dedicated
		// percent-encoded headers so a consumer can read the original envelope
		// sender and recipient without parsing the raw MIME. An empty sender is
		// still emitted as an empty header, so a consumer that requires one
		// fails closed rather than falling back to the MIME From header.
		req.Header.Set(HeaderEnvelopeFrom, percentEncodeHeaderValue(m.EnvelopeFrom))
		req.Header.Set(HeaderEnvelopeTo, percentEncodeHeaderValue(m.EnvelopeRecipient))
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-MailMoose-Event", d.Type)
	req.Header.Set("X-MailMoose-Delivery", d.Client.ID+":"+d.Cursor)
	req.Header.Set("X-MailMoose-Message-Id", d.EntityID)
	req.Header.Set("X-MailMoose-Cursor", d.Cursor)
	if d.Client.AuthMode == "bearer" {
		req.Header.Set("Authorization", "Bearer "+string(secret))
	} else {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, secret)
		_, _ = mac.Write([]byte(ts + "."))
		_, _ = mac.Write(body)
		req.Header.Set("X-MailMoose-Signature", "t="+ts+",v1="+hex.EncodeToString(mac.Sum(nil)))
	}
	// Reserve the pass's remaining budget for other clients and persist the
	// outcome independently of a cancelled network attempt.
	attemptCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := w.client.Do(req.WithContext(attemptCtx))
	outcomeCtx, outcomeCancel := outcomeContext(ctx)
	defer outcomeCancel()
	now := time.Now().UTC()
	deadline := now.Add(w.svc.Config.WebhookRetryWindow)
	if err == nil {
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return w.svc.Store.RecordWebhookDelivery(outcomeCtx, d.Client.ID, d.EventID, true, "", now, deadline)
		}
		err = fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	var previous int
	_ = w.svc.Store.WebhookAttemptCount(outcomeCtx, d.Client.ID, d.EventID, &previous)
	delay := webhookBackoff(previous, d.Client.ID, d.EventID)
	return w.svc.Store.RecordWebhookDelivery(outcomeCtx, d.Client.ID, d.EventID, false, truncateWebhookError(err.Error()), now.Add(delay), deadline)
}

// webhookBackoff returns the retry delay for the next attempt. It is the
// exponential base (2^attempts, capped at an hour) plus up to +/-25% jitter,
// so many endpoints that failed together do not all retry in lockstep. Jitter
// is derived deterministically from the client and event id, so a retry's
// scheduled time is stable across processes and a test can assert bounds.
func webhookBackoff(previous int, clientID string, eventID int64) time.Duration {
	base := time.Second * time.Duration(1<<min(previous, 10))
	if base > time.Hour {
		base = time.Hour
	}
	span := int64(base / 4)
	if span <= 0 {
		return base
	}
	h := sha256.Sum256([]byte(clientID + ":" + strconv.FormatInt(eventID, 10)))
	// Map the low 8 bytes into [-span, +span].
	var u uint64
	for i := 0; i < 8; i++ {
		u = u<<8 | uint64(h[i])
	}
	offset := int64(u%uint64(2*span+1)) - span
	jittered := base + time.Duration(offset)
	if jittered < time.Second {
		jittered = time.Second
	}
	return jittered
}

// HeaderEnvelopeFrom and HeaderEnvelopeTo are the exact shared outbound forward
// webhook envelope-metadata contract. Values are percent-encoded UTF-8 using
// RFC 3986 escaping, so a space is %20, a literal plus is %2B and @ is %40 (not
// application/x-www-form-urlencoded, where + means space). A receiver decodes
// strictly and must treat a missing header as "no envelope metadata" rather
// than falling back to the MIME headers.
const (
	HeaderEnvelopeFrom = "X-MailMoose-Envelope-From"
	HeaderEnvelopeTo   = "X-MailMoose-Envelope-To"

	// maxEncodedEnvelopeHeader bounds the encoded header value. A well-formed
	// address is at most 254 octets (RFC 5321), and percent-encoding every byte
	// at most triples it, so the decoded bound the receiver enforces is 254
	// while the encoded header never exceeds this. A malformed or oversized
	// value is sent as empty.
	maxEncodedEnvelopeHeader = 3 * 254
)

// percentEncodeHeaderValue applies RFC 3986 percent-encoding: only the
// unreserved set A-Z a-z 0-9 - . _ ~ is left literal; every other byte of the
// UTF-8 input is written as %XX. It returns the empty string for a value whose
// encoding would exceed maxEncodedEnvelopeHeader.
func percentEncodeHeaderValue(v string) string {
	if v == "" {
		return ""
	}
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		c := v[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		if b.Len()+3 > maxEncodedEnvelopeHeader {
			return ""
		}
		b.WriteByte('%')
		b.WriteByte(upperhex[c>>4])
		b.WriteByte(upperhex[c&0x0f])
	}
	return b.String()
}

func truncateWebhookError(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 512 {
		return s[:512]
	}
	return s
}

// IsRemotePayload reports whether a durable event payload describes a remote
// arrival (payload remote=true) rather than a locally-persisted message.
func IsRemotePayload(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	var p struct {
		Remote bool `json:"remote"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return false
	}
	return p.Remote
}

// remoteAwareJSON builds the JSON delivery body for an event. A remote arrival
// carries its remote locator and no local message id, so a consumer can fetch it
// live; a local event keeps its original shape.
func remoteAwareJSON(d store.PendingWebhookDelivery, arrival *store.RemoteArrival) map[string]any {
	if arrival == nil {
		return map[string]any{"event": d.Type, "cursor": d.Cursor, "inbox_id": d.Client.InboxID, "message_id": d.EntityID}
	}
	return map[string]any{
		"event": d.Type, "cursor": d.Cursor, "inbox_id": d.Client.InboxID, "message_id": arrival.ID,
		"remote": true, "remote_uid": arrival.UID, "uid_validity": arrival.UIDValidity,
		"folder_path": arrival.FolderPath, "rfc_message_id": arrival.RFCMessageID,
		"from": arrival.FromAddress, "subject": arrival.Subject, "size_bytes": arrival.SizeBytes,
	}
}

// remoteArrivalRaw streams a remote arrival's raw MIME to a transient file, reads
// it back bounded by the configured maximum, and removes the file. It is only
// called for a forward-mode client, so no body is fetched on a metadata-only
// delivery. The remote forwarder is installed once at startup; when it is absent
// (a deployment with no remote bridge), a forward of a remote arrival is a
// terminal error rather than a silent empty body.
func (w *WebhookWorker) remoteArrivalRaw(ctx context.Context, inboxID string, arrival store.RemoteArrival) ([]byte, error) {
	if w.svc.RemoteForwarder == nil {
		return nil, fmt.Errorf("remote forwarder is not installed")
	}
	inbox, err := w.svc.Store.GetInboxInternal(ctx, arrival.AccountID, inboxID)
	if err != nil {
		return nil, err
	}
	path, size, err := w.svc.RemoteForwarder.FetchArrivalRaw(ctx, inbox, arrival)
	if err != nil {
		return nil, err
	}
	defer w.svc.RemoteForwarder.CleanupRemoteRaw(path)
	if size > w.svc.Config.MaxMessageBytes {
		return nil, fmt.Errorf("raw message exceeds configured maximum")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, w.svc.Config.MaxMessageBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > w.svc.Config.MaxMessageBytes {
		return nil, fmt.Errorf("raw message exceeds configured maximum")
	}
	return body, nil
}
