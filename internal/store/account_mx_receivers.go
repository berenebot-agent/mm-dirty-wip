package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// AccountMXReceiver is the per-account Remote MX receiver configuration. It
// mirrors MXSettings but is scoped to one account (account_id is the primary
// key): exactly one receiver per account, editable by an account admin. The
// core dials the account's own standalone Dial MX receiver in single mode using
// the stored bearer credential.
//
// ReceiverURL is stored in cleartext so a status read (which must not need the
// application key) can name the destination. EncryptedSecret holds the encrypted
// bearer credential and EncryptedConfig the encrypted auxiliary configuration (a
// private CA bundle and the private-destination opt-in); both are meaningless
// without the application key and are redacted on read. Revision is the CAS
// token a save must present and the runtime reconciliation generation.
type AccountMXReceiver struct {
	AccountID       string
	ReceiverURL     string
	EncryptedSecret string
	EncryptedConfig string
	Revision        int64
	CreatedAt       string
	UpdatedAt       string
}

// GetAccountMXReceiver returns the account's Remote MX configuration, or
// ErrNotFound when it has never been saved.
func (s *Store) GetAccountMXReceiver(ctx context.Context, accountID string) (AccountMXReceiver, error) {
	var m AccountMXReceiver
	err := s.read.QueryRowContext(ctx, `SELECT account_id,receiver_url,encrypted_secret,encrypted_config,revision,created_at,updated_at FROM account_mx_receivers WHERE account_id=?`, accountID).Scan(
		&m.AccountID, &m.ReceiverURL, &m.EncryptedSecret, &m.EncryptedConfig, &m.Revision, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountMXReceiver{}, ErrNotFound
	}
	if err != nil {
		return AccountMXReceiver{}, err
	}
	return m, nil
}

// SaveAccountMXReceiverCAS creates the account's receiver (zero expected) or
// updates it under optimistic concurrency. A zero ConfigVersion means "create
// only" and fails with ErrConflict if a row already exists. A non-zero version
// must match the stored revision. The read-back and write happen in one
// transaction.
func (s *Store) SaveAccountMXReceiverCAS(ctx context.Context, accountID, receiverURL, encryptedSecret, encryptedConfig string, expected ConfigVersion) (AccountMXReceiver, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return AccountMXReceiver{}, err
	}
	defer tx.Rollback()
	var used int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM account_mx_receivers WHERE account_id<>? AND receiver_url=? COLLATE NOCASE AND receiver_url<>''`, accountID, receiverURL).Scan(&used); err != nil {
		return AccountMXReceiver{}, err
	}
	if used != 0 {
		return AccountMXReceiver{}, ErrConflict
	}
	now := nowText()
	if expected.Revision == 0 {
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM account_mx_receivers WHERE account_id=?`, accountID).Scan(&n); err != nil {
			return AccountMXReceiver{}, err
		}
		if n != 0 {
			return AccountMXReceiver{}, ErrConflict
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO account_mx_receivers(account_id,receiver_url,encrypted_secret,encrypted_config,revision,created_at,updated_at) VALUES(?,?,?,?,1,?,?)`,
			accountID, receiverURL, encryptedSecret, encryptedConfig, now, now); err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				return AccountMXReceiver{}, ErrConflict
			}
			return AccountMXReceiver{}, err
		}
	} else {
		res, err := tx.ExecContext(ctx, `UPDATE account_mx_receivers SET receiver_url=?,encrypted_secret=?,encrypted_config=?,revision=revision+1,updated_at=? WHERE account_id=? AND revision=?`,
			receiverURL, encryptedSecret, encryptedConfig, now, accountID, expected.Revision)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				return AccountMXReceiver{}, ErrConflict
			}
			return AccountMXReceiver{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return AccountMXReceiver{}, ErrConflict
		}
	}
	m, err := scanAccountMXReceiver(tx.QueryRowContext(ctx, `SELECT account_id,receiver_url,encrypted_secret,encrypted_config,revision,created_at,updated_at FROM account_mx_receivers WHERE account_id=?`, accountID))
	if err != nil {
		return AccountMXReceiver{}, err
	}
	if err = tx.Commit(); err != nil {
		return AccountMXReceiver{}, err
	}
	return m, nil
}

// ClearAccountMXReceiverCAS removes the account's configured receiver while
// keeping the row, so a cleared configuration remains a deliberate decision and
// the runtime observes the change. ReceiverURL is emptied and the encrypted
// fields dropped; revision advances. Clearing an already-empty configuration
// with a zero expected version is a no-op; a non-zero expected version must
// match the stored revision.
func (s *Store) ClearAccountMXReceiverCAS(ctx context.Context, accountID string, expected ConfigVersion) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var revision int64
	var receiverURL string
	err = tx.QueryRowContext(ctx, `SELECT revision,receiver_url FROM account_mx_receivers WHERE account_id=?`, accountID).Scan(&revision, &receiverURL)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if expected.Revision != 0 && expected.Revision != revision {
		return ErrConflict
	}
	if receiverURL == "" {
		return tx.Commit()
	}
	res, err := tx.ExecContext(ctx, `UPDATE account_mx_receivers SET receiver_url='',encrypted_secret='',encrypted_config='',revision=revision+1,updated_at=? WHERE account_id=? AND revision=?`,
		nowText(), accountID, revision)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return tx.Commit()
}

// ListRemoteMXAccounts returns the account ids that have at least one domain
// configured to receive through the account Remote MX provider. It is the
// runtime's "in use" set: an account receiver is dialed only while a domain
// routes to it.
func (s *Store) ListRemoteMXAccounts(ctx context.Context) ([]string, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT DISTINCT account_id FROM domain_receiving_configs WHERE provider='remotemx' ORDER BY account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RemoteMXReceiverInUse reports whether any domain is configured to receive
// through the given account's Remote MX receiver. It gates a clear/delete so the
// caller can refuse or confirm rather than silently breaking receiving.
func (s *Store) RemoteMXReceiverInUse(ctx context.Context, accountID string) (bool, error) {
	var used bool
	err := s.read.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM domain_receiving_configs WHERE provider='remotemx' AND account_id=?)`, accountID).Scan(&used)
	return used, err
}

// ListAccountMXReceiverURLs returns the normalized receiver URLs of every
// account except excludeAccount. It backs the global (url) uniqueness check: one
// physical single-mode receiver may be registered by exactly one account.
func (s *Store) ListAccountMXReceiverURLs(ctx context.Context, excludeAccount string) ([]string, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT receiver_url FROM account_mx_receivers WHERE account_id<>? AND receiver_url<>''`, excludeAccount)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func scanAccountMXReceiver(row interface{ Scan(...any) error }) (AccountMXReceiver, error) {
	var m AccountMXReceiver
	if err := row.Scan(&m.AccountID, &m.ReceiverURL, &m.EncryptedSecret, &m.EncryptedConfig, &m.Revision, &m.CreatedAt, &m.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AccountMXReceiver{}, ErrNotFound
		}
		return AccountMXReceiver{}, err
	}
	return m, nil
}
