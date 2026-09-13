package store

import (
	"context"
	"database/sql"
	"time"

	"gatehouse-mail/internal/idgen"
)

// MXReceipt is the durable per-recipient delivery receipt for the optional MX
// edge. It records the successful disposition for a versioned delivery
// fingerprint so a sender retry (including one that arrives after the message
// row was deleted) deduplicates instead of creating a second message, quota
// charge or event. message_id is an opaque reference, not a foreign key, so the
// receipt survives message deletion for its retention horizon.
type MXReceipt struct {
	ID                  string
	AccountID           string
	Provider            string
	EnvelopeRecipient   string
	DeliveryFingerprint string
	Disposition         string
	MessageID           string
	Reason              string
	CreatedAt           time.Time
	ExpiresAt           time.Time
}

// LookupMXReceipt returns the recorded receipt for a delivery fingerprint, or
// ErrNotFound. It is used by the MX ingest path to return a recorded
// disposition for a duplicate.
func (s *Store) LookupMXReceipt(ctx context.Context, accountID, provider, recipient, fingerprint string) (MXReceipt, error) {
	var r MXReceipt
	var created, expires string
	err := s.read.QueryRowContext(ctx, `SELECT id,account_id,provider,envelope_recipient,delivery_fingerprint,disposition,message_id,reason,created_at,expires_at FROM mx_receipts WHERE account_id=? AND provider=? AND envelope_recipient=? AND delivery_fingerprint=?`,
		accountID, provider, normalizeAddress(recipient), fingerprint).Scan(
		&r.ID, &r.AccountID, &r.Provider, &r.EnvelopeRecipient, &r.DeliveryFingerprint, &r.Disposition, &r.MessageID, &r.Reason, &created, &expires)
	if err == sql.ErrNoRows {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	r.CreatedAt = parseTime(created)
	r.ExpiresAt = parseTime(expires)
	return r, nil
}

// recordMXReceiptTx writes the receipt inside the caller's transaction so it
// commits atomically with the message/quota/event persistence. A repeat
// fingerprint is left as-is (the first recorded disposition wins).
func recordMXReceiptTx(ctx context.Context, tx *sql.Tx, r MXReceipt) error {
	now := nowText()
	expires := r.ExpiresAt
	if expires.IsZero() {
		expires = time.Now().UTC().Add(DefaultMXReceiptTTL)
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO mx_receipts(id,account_id,provider,envelope_recipient,delivery_fingerprint,disposition,message_id,reason,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		idgen.New("mrc"), r.AccountID, r.Provider, normalizeAddress(r.EnvelopeRecipient), r.DeliveryFingerprint, r.Disposition, r.MessageID, r.Reason, now, timeText(expires))
	return err
}

// SweepMXReceipts deletes receipts whose retention horizon has passed and
// returns the number removed. Receipts never survive past their horizon because
// a sender retry beyond it is treated as a new delivery.
func (s *Store) SweepMXReceipts(ctx context.Context, now time.Time) (int, error) {
	res, err := s.write.ExecContext(ctx, `DELETE FROM mx_receipts WHERE expires_at < ?`, timeText(now))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DefaultMXReceiptTTL is the 7-day retention horizon recorded in D031. It
// covers the supported sender retry window, the HTTP replay window and expected
// outage recovery.
const DefaultMXReceiptTTL = 7 * 24 * time.Hour

// receiptExpiry returns the receipt expiry for a caller TTL, falling back to
// the default. A zero TTL means the operator kept the default.
func receiptExpiry(ttl time.Duration) time.Time {
	if ttl <= 0 {
		ttl = DefaultMXReceiptTTL
	}
	return time.Now().UTC().Add(ttl)
}

// Receipt dispositions mirror the mxwire disposition vocabulary. They are
// duplicated here so the provider-neutral store does not import the MX wire
// package.
const (
	DispositionStored = "stored"
	DispositionSpam   = "spam"
)
