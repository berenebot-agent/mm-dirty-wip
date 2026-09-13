package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"gatehouse-mail/internal/idgen"
)

// DomainConfig is the single optional sending or receiving provider
// configuration owned by one domain. A domain has at most one of each, and the
// encrypted bytes belong to that domain alone (migrated shared credentials are
// stored as independent copies).
type DomainConfig struct {
	ID              string    `json:"id"`
	AccountID       string    `json:"account_id"`
	DomainID        string    `json:"domain_id"`
	Provider        string    `json:"provider"`
	EncryptedConfig string    `json:"-"`
	Revision        int64     `json:"revision"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// DomainSendingConfig and DomainReceivingConfig are the two config slots. They
// share one shape because the lifecycle and CAS semantics are identical.
type (
	DomainSendingConfig   = DomainConfig
	DomainReceivingConfig = DomainConfig
)

// ConfigVersion is the optimistic-concurrency token a save must present. A zero
// value means "create only".
type ConfigVersion struct {
	ID       string
	Revision int64
}

const (
	domainSendingTable   = "domain_sending_configs"
	domainReceivingTable = "domain_receiving_configs"
)

func (s *Store) GetDomainSendingConfig(ctx context.Context, accountID, domainID string) (DomainSendingConfig, error) {
	return s.getDomainConfig(ctx, domainSendingTable, accountID, domainID)
}

func (s *Store) GetDomainReceivingConfig(ctx context.Context, accountID, domainID string) (DomainReceivingConfig, error) {
	return s.getDomainConfig(ctx, domainReceivingTable, accountID, domainID)
}

// SaveDomainSendingConfig creates (zero expected) or CAS-updates the domain's
// sending configuration. The read, write and hydration happen in one
// transaction; a concurrent change yields ErrConflict with no retry.
func (s *Store) SaveDomainSendingConfig(ctx context.Context, accountID, domainID, provider, encrypted string, expected ConfigVersion) (DomainSendingConfig, error) {
	return s.saveDomainConfig(ctx, domainSendingTable, "dsc", accountID, domainID, provider, encrypted, expected)
}

// SaveDomainReceivingConfig creates (zero expected) or CAS-updates the domain's
// receiving configuration.
func (s *Store) SaveDomainReceivingConfig(ctx context.Context, accountID, domainID, provider, encrypted string, expected ConfigVersion) (DomainReceivingConfig, error) {
	return s.saveDomainConfig(ctx, domainReceivingTable, "drc", accountID, domainID, provider, encrypted, expected)
}

// DeleteDomainSendingConfig removes the domain's sending configuration. It is
// idempotent for a domain that exists but has no config, and returns ErrNotFound
// for a missing or foreign domain.
func (s *Store) DeleteDomainSendingConfig(ctx context.Context, accountID, domainID string) error {
	return s.deleteDomainConfig(ctx, domainSendingTable, accountID, domainID)
}

// DeleteDomainReceivingConfig removes the domain's receiving configuration.
func (s *Store) DeleteDomainReceivingConfig(ctx context.Context, accountID, domainID string) error {
	return s.deleteDomainConfig(ctx, domainReceivingTable, accountID, domainID)
}

// DomainSendingConfigForMessage resolves the sending configuration for an
// existing message. It uses the message's recorded sending domain (the alias's
// own domain for a send-as-alias), falling back to the inbox's domain for
// messages enqueued before that column existed. A missing message or foreign
// account is ErrNotFound; a message whose domain has no config is ErrNoProvider.
func (s *Store) DomainSendingConfigForMessage(ctx context.Context, accountID, messageID string) (DomainSendingConfig, error) {
	var domainID string
	err := s.read.QueryRowContext(ctx, `SELECT COALESCE(m.sending_domain_id,i.domain_id) FROM messages m JOIN inboxes i ON i.id=m.inbox_id WHERE m.id=? AND m.account_id=?`, messageID, accountID).Scan(&domainID)
	if err == sql.ErrNoRows {
		return DomainSendingConfig{}, ErrNotFound
	}
	if err != nil {
		return DomainSendingConfig{}, err
	}
	return s.GetDomainSendingConfig(ctx, accountID, domainID)
}

// LastSentByDomain returns the most recent successful send time for each domain
// in the account, keyed by domain id. Domains that have never sent are absent.
func (s *Store) LastSentByDomain(ctx context.Context, accountID string) (map[string]time.Time, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT domain_id, MAX(created_at) FROM outbound_delivery_log WHERE account_id=? AND status='sent' AND domain_id IS NOT NULL GROUP BY domain_id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id, created string
		if err = rows.Scan(&id, &created); err != nil {
			return nil, err
		}
		out[id] = parseTime(created)
	}
	return out, rows.Err()
}

// LastReceivedByDomain returns the most recent inbound message time for each
// domain in the account, keyed by domain id. Domains with no mail are absent.
func (s *Store) LastReceivedByDomain(ctx context.Context, accountID string) (map[string]time.Time, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT i.domain_id, MAX(COALESCE(m.received_at, m.created_at)) FROM messages m JOIN inboxes i ON i.id=m.inbox_id WHERE m.account_id=? AND m.direction='inbound' GROUP BY i.domain_id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id, when string
		if err = rows.Scan(&id, &when); err != nil {
			return nil, err
		}
		out[id] = parseTime(when)
	}
	return out, rows.Err()
}

func (s *Store) getDomainConfig(ctx context.Context, table, accountID, domainID string) (DomainConfig, error) {
	exists, err := s.domainExists(ctx, accountID, domainID)
	if err != nil {
		return DomainConfig{}, err
	}
	if !exists {
		return DomainConfig{}, ErrNotFound
	}
	return scanDomainConfig(s.read.QueryRowContext(ctx, domainConfigSelect+` FROM `+table+` WHERE domain_id=? AND account_id=?`, domainID, accountID))
}

func (s *Store) saveDomainConfig(ctx context.Context, table, prefix, accountID, domainID, provider, encrypted string, expected ConfigVersion) (DomainConfig, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return DomainConfig{}, err
	}
	defer tx.Rollback()
	exists, err := domainExistsTx(ctx, tx, accountID, domainID)
	if err != nil {
		return DomainConfig{}, err
	}
	if !exists {
		return DomainConfig{}, ErrNotFound
	}
	now := nowText()
	if expected.ID == "" && expected.Revision == 0 {
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE domain_id=?`, domainID).Scan(&n); err != nil {
			return DomainConfig{}, err
		}
		if n != 0 {
			return DomainConfig{}, ErrConflict
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO `+table+`(id,domain_id,account_id,provider,encrypted_config,revision,created_at,updated_at) VALUES(?,?,?,?,?,1,?,?)`, idgen.New(prefix), domainID, accountID, provider, encrypted, now, now); err != nil {
			return DomainConfig{}, err
		}
	} else {
		res, err := tx.ExecContext(ctx, `UPDATE `+table+` SET provider=?,encrypted_config=?,revision=revision+1,updated_at=? WHERE id=? AND domain_id=? AND account_id=? AND revision=?`, provider, encrypted, now, expected.ID, domainID, accountID, expected.Revision)
		if err != nil {
			return DomainConfig{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// The caller supplied a version that no longer matches the stored
			// row (rotated, replaced or removed concurrently): conflict.
			return DomainConfig{}, ErrConflict
		}
	}
	cfg, err := scanDomainConfig(tx.QueryRowContext(ctx, domainConfigSelect+` FROM `+table+` WHERE domain_id=? AND account_id=?`, domainID, accountID))
	if err != nil {
		return DomainConfig{}, err
	}
	if err = tx.Commit(); err != nil {
		return DomainConfig{}, err
	}
	return cfg, nil
}

func (s *Store) deleteDomainConfig(ctx context.Context, table, accountID, domainID string) error {
	exists, err := s.domainExists(ctx, accountID, domainID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	_, err = s.write.ExecContext(ctx, `DELETE FROM `+table+` WHERE domain_id=? AND account_id=?`, domainID, accountID)
	return err
}

func (s *Store) domainExists(ctx context.Context, accountID, domainID string) (bool, error) {
	var n int
	if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM domains WHERE id=? AND account_id=?`, domainID, accountID).Scan(&n); err != nil {
		return false, err
	}
	return n == 1, nil
}

func domainExistsTx(ctx context.Context, tx *sql.Tx, accountID, domainID string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM domains WHERE id=? AND account_id=?`, domainID, accountID).Scan(&n); err != nil {
		return false, err
	}
	return n == 1, nil
}

const domainConfigSelect = `SELECT id,account_id,domain_id,provider,encrypted_config,revision,created_at,updated_at`

func scanDomainConfig(row interface{ Scan(...any) error }) (DomainConfig, error) {
	var c DomainConfig
	var created, updated string
	if err := row.Scan(&c.ID, &c.AccountID, &c.DomainID, &c.Provider, &c.EncryptedConfig, &c.Revision, &created, &updated); err != nil {
		if err == sql.ErrNoRows {
			return DomainConfig{}, ErrNoProvider
		}
		return DomainConfig{}, err
	}
	c.CreatedAt = parseTime(created)
	c.UpdatedAt = parseTime(updated)
	return c, nil
}
