package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"gatehouse-mail/internal/idgen"
)

type InboundCredential struct {
	ID, AccountID, Name, Provider, EncryptedConfig string
	CreatedAt, UpdatedAt                           time.Time
}

// InboundBinding is the account/domain/credential tuple resolved from an
// inbound envelope recipient. EncryptedConfig is decrypted by the service
// before it is handed to a transport adapter.
type InboundBinding struct {
	AccountID, DomainID, CredentialID, Provider, EncryptedConfig, Recipient string
}

func (s *Store) SaveInboundCredential(ctx context.Context, accountID, id, name, provider, encrypted string) (InboundCredential, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	now := nowText()
	if id == "" {
		id = idgen.New("inb")
		_, err := s.write.ExecContext(ctx, `INSERT INTO inbound_credentials(id,account_id,name,provider,encrypted_config,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, id, accountID, name, provider, encrypted, now, now)
		if err != nil {
			return InboundCredential{}, err
		}
	} else {
		res, err := s.write.ExecContext(ctx, `UPDATE inbound_credentials SET name=?,encrypted_config=?,updated_at=? WHERE id=? AND account_id=?`, name, encrypted, now, id, accountID)
		if err != nil {
			return InboundCredential{}, err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return InboundCredential{}, ErrNotFound
		}
	}
	return s.GetInboundCredential(ctx, accountID, id)
}

func (s *Store) GetInboundCredential(ctx context.Context, accountID, id string) (InboundCredential, error) {
	var c InboundCredential
	var created, updated string
	err := s.read.QueryRowContext(ctx, `SELECT id,account_id,name,provider,encrypted_config,created_at,updated_at FROM inbound_credentials WHERE id=? AND account_id=?`, id, accountID).Scan(&c.ID, &c.AccountID, &c.Name, &c.Provider, &c.EncryptedConfig, &created, &updated)
	if err == sql.ErrNoRows {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.CreatedAt = parseTime(created)
	c.UpdatedAt = parseTime(updated)
	return c, nil
}

func (s *Store) ListInboundCredentials(ctx context.Context, accountID string) ([]InboundCredential, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT id,account_id,name,provider,encrypted_config,created_at,updated_at FROM inbound_credentials WHERE account_id=? ORDER BY name`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InboundCredential
	for rows.Next() {
		var c InboundCredential
		var cr, up string
		if err = rows.Scan(&c.ID, &c.AccountID, &c.Name, &c.Provider, &c.EncryptedConfig, &cr, &up); err != nil {
			return nil, err
		}
		c.CreatedAt = parseTime(cr)
		c.UpdatedAt = parseTime(up)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) DeleteInboundCredential(ctx context.Context, accountID, id string) error {
	res, err := s.write.ExecContext(ctx, `DELETE FROM inbound_credentials WHERE id=? AND account_id=?`, id, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RenameInboundCredential changes the display name of a receive path without
// touching its provider or encrypted configuration.
func (s *Store) RenameInboundCredential(ctx context.Context, accountID, id, name string) error {
	res, err := s.write.ExecContext(ctx, `UPDATE inbound_credentials SET name=?,updated_at=? WHERE id=? AND account_id=?`, name, nowText(), id, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// LastReceivedByInboundCredential returns the most recent inbound message time
// for each inbound credential, inferred from the domains currently assigned to
// it. Credentials with no assigned domain or no mail are absent from the map.
func (s *Store) LastReceivedByInboundCredential(ctx context.Context, accountID string) (map[string]time.Time, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT d.inbound_credential_id, MAX(COALESCE(m.received_at, m.created_at)) FROM messages m JOIN inboxes i ON i.id=m.inbox_id JOIN domains d ON d.id=i.domain_id WHERE m.account_id=? AND m.direction='inbound' AND d.inbound_credential_id IS NOT NULL GROUP BY d.inbound_credential_id`, accountID)
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

// SetDomainInboundCredential assigns (or clears, with an empty id) the receive
// connection for a domain. The credential must belong to the same account.
func (s *Store) SetDomainInboundCredential(ctx context.Context, accountID, domainID, credentialID string) error {
	credentialID = strings.TrimSpace(credentialID)
	if credentialID != "" {
		var n int
		if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM inbound_credentials WHERE id=? AND account_id=?`, credentialID, accountID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrForbidden
		}
	}
	res, err := s.write.ExecContext(ctx, `UPDATE domains SET inbound_credential_id=? WHERE id=? AND account_id=?`, nullString(credentialID), domainID, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DomainInboundCredential resolves the receive connection assigned to a domain.
// It returns ErrNoProvider when the domain has no receive path configured.
func (s *Store) DomainInboundCredential(ctx context.Context, accountID, domainID string) (InboundCredential, error) {
	var id string
	err := s.read.QueryRowContext(ctx, `SELECT COALESCE(inbound_credential_id,'') FROM domains WHERE id=? AND account_id=?`, domainID, accountID).Scan(&id)
	if err == sql.ErrNoRows {
		return InboundCredential{}, ErrNotFound
	}
	if err != nil {
		return InboundCredential{}, err
	}
	if id == "" {
		return InboundCredential{}, ErrNoProvider
	}
	return s.GetInboundCredential(ctx, accountID, id)
}

// ResolveInboundBinding maps an envelope recipient to the domain and assigned
// inbound credential for the given provider. The recipient's domain is globally
// unique, so account ownership is derived from it rather than supplied by the
// caller. It returns ErrNotFound for unknown domains or unconfigured domains.
func (s *Store) ResolveInboundBinding(ctx context.Context, provider, recipient string) (InboundBinding, error) {
	var b InboundBinding
	at := strings.LastIndex(recipient, "@")
	if at < 0 {
		return b, ErrNotFound
	}
	domain := normalizeDomain(recipient[at+1:])
	err := s.read.QueryRowContext(ctx, `SELECT d.account_id,d.id,c.id,c.provider,c.encrypted_config FROM domains d JOIN inbound_credentials c ON c.id=d.inbound_credential_id WHERE d.name=? AND c.provider=?`, domain, strings.ToLower(strings.TrimSpace(provider))).Scan(&b.AccountID, &b.DomainID, &b.CredentialID, &b.Provider, &b.EncryptedConfig)
	if err == sql.ErrNoRows {
		return b, ErrNotFound
	}
	if err != nil {
		return b, err
	}
	b.Recipient = normalizeAddress(recipient)
	return b, nil
}
