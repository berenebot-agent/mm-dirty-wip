package store

import (
	"context"
	"database/sql"
	"strings"
)

// InboundBinding is the account/domain/config tuple resolved from an inbound
// envelope recipient. CredentialID is the receiving config row id; the service
// decrypts EncryptedConfig before handing it to a transport adapter.
type InboundBinding struct {
	AccountID, DomainID, CredentialID, Provider, EncryptedConfig, Recipient string
}

// ResolveInboundBinding maps an envelope recipient to the domain and its
// configured receiving provider. The recipient's domain is globally unique, so
// account ownership is derived from it rather than supplied by the caller. It
// returns ErrNotFound for unknown domains or domains with no matching receiving
// config.
func (s *Store) ResolveInboundBinding(ctx context.Context, provider, recipient string) (InboundBinding, error) {
	var b InboundBinding
	at := strings.LastIndex(recipient, "@")
	if at < 0 {
		return b, ErrNotFound
	}
	domain := normalizeDomain(recipient[at+1:])
	err := s.read.QueryRowContext(ctx, `SELECT d.account_id,d.id,c.id,c.provider,c.encrypted_config FROM domains d JOIN domain_receiving_configs c ON c.domain_id=d.id AND c.account_id=d.account_id WHERE d.name=? AND c.provider=?`, domain, strings.ToLower(strings.TrimSpace(provider))).Scan(&b.AccountID, &b.DomainID, &b.CredentialID, &b.Provider, &b.EncryptedConfig)
	if err == sql.ErrNoRows {
		return b, ErrNotFound
	}
	if err != nil {
		return b, err
	}
	b.Recipient = normalizeAddress(recipient)
	return b, nil
}
