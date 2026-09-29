package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/idgen"
)

// DomainConfig is the single optional sending or receiving provider
// configuration owned by one domain. A domain has at most one of each, and the
// encrypted bytes belong to that domain alone (migrated shared credentials are
// stored as independent copies).
type DomainConfig struct {
	ExternalAliasID string    `json:"external_alias_id,omitempty"`
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
	return s.ResolveDomainSendingConfig(ctx, accountID, domainID)
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
// maxDomainAncestorDepth bounds a parent-domain walk so a corrupted or cyclic
// parent chain can never loop forever.
const maxDomainAncestorDepth = 16

// domainInheritanceState is the parent pointer and inheritance switches needed
// to walk from a subdomain toward its ancestors.
type domainInheritanceState struct {
	ParentID         string
	InheritReceiving bool
	InheritSending   bool
}

func (s *Store) domainInheritanceState(ctx context.Context, accountID, domainID string) (domainInheritanceState, error) {
	var st domainInheritanceState
	var parent sql.NullString
	var recv, send int
	err := s.read.QueryRowContext(ctx, `SELECT parent_domain_id, inherit_receiving, inherit_sending FROM domains WHERE id=? AND account_id=?`, domainID, accountID).Scan(&parent, &recv, &send)
	if err == sql.ErrNoRows {
		return st, ErrNotFound
	}
	if err != nil {
		return st, err
	}
	st.ParentID = parent.String
	st.InheritReceiving = recv != 0
	st.InheritSending = send != 0
	return st, nil
}

// ResolveDomainReceivingConfig returns the receiving configuration that applies
// to a domain for a provider: its own when configured for that provider,
// otherwise the nearest ancestor's while inheritance is enabled. A domain that
// is explicitly configured for a different provider does not fall through - it
// is simply not configured for the requested provider (ErrNoProvider).
func (s *Store) ResolveDomainReceivingConfig(ctx context.Context, accountID, domainID, provider string) (DomainReceivingConfig, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	id := domainID
	for depth := 0; depth < maxDomainAncestorDepth; depth++ {
		cfg, err := s.GetDomainReceivingConfig(ctx, accountID, id)
		if err == nil {
			if strings.EqualFold(cfg.Provider, provider) {
				return cfg, nil
			}
			return DomainReceivingConfig{}, ErrNoProvider
		}
		if !errors.Is(err, ErrNoProvider) {
			return DomainReceivingConfig{}, err
		}
		st, err := s.domainInheritanceState(ctx, accountID, id)
		if err != nil {
			return DomainReceivingConfig{}, err
		}
		if !st.InheritReceiving || st.ParentID == "" {
			return DomainReceivingConfig{}, ErrNoProvider
		}
		id = st.ParentID
	}
	return DomainReceivingConfig{}, ErrNoProvider
}

// ResolveDomainSendingConfig returns the sending configuration that applies to a
// domain: its own, otherwise the nearest ancestor's while inheritance is
// enabled. ErrNoProvider when no domain in the chain is configured.
func (s *Store) ResolveDomainSendingConfig(ctx context.Context, accountID, domainID string) (DomainSendingConfig, error) {
	id := domainID
	for depth := 0; depth < maxDomainAncestorDepth; depth++ {
		cfg, err := s.GetDomainSendingConfig(ctx, accountID, id)
		if err == nil {
			return cfg, nil
		}
		if !errors.Is(err, ErrNoProvider) {
			return DomainSendingConfig{}, err
		}
		st, err := s.domainInheritanceState(ctx, accountID, id)
		if err != nil {
			return DomainSendingConfig{}, err
		}
		if !st.InheritSending || st.ParentID == "" {
			return DomainSendingConfig{}, ErrNoProvider
		}
		id = st.ParentID
	}
	return DomainSendingConfig{}, ErrNoProvider
}

// effectiveProvider reports the provider a domain effectively uses for display,
// plus the name of the ancestor it was inherited from ("" when it is the
// domain's own). It does not filter by a requested provider.
func (s *Store) effectiveProvider(ctx context.Context, accountID, domainID string, sending bool) (provider, inheritedFrom string) {
	id := domainID
	for depth := 0; depth < maxDomainAncestorDepth; depth++ {
		var providerHere string
		var err error
		if sending {
			var c DomainSendingConfig
			c, err = s.GetDomainSendingConfig(ctx, accountID, id)
			if err == nil {
				providerHere = c.Provider
			}
		} else {
			var c DomainReceivingConfig
			c, err = s.GetDomainReceivingConfig(ctx, accountID, id)
			if err == nil {
				providerHere = c.Provider
			}
		}
		if err == nil {
			if id == domainID {
				return providerHere, ""
			}
			name, _ := s.domainName(ctx, accountID, id)
			return providerHere, name
		}
		if !errors.Is(err, ErrNoProvider) {
			return "", ""
		}
		st, serr := s.domainInheritanceState(ctx, accountID, id)
		if serr != nil {
			return "", ""
		}
		inherit := st.InheritSending
		if !sending {
			inherit = st.InheritReceiving
		}
		if !inherit || st.ParentID == "" {
			return "", ""
		}
		id = st.ParentID
	}
	return "", ""
}

