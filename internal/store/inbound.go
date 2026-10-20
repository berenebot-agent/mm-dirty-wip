package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// InboundBinding is the account/domain/config tuple resolved from an inbound
// envelope recipient. CredentialID is the receiving config row id; the service
// decrypts EncryptedConfig before handing it to a transport adapter.
type InboundBinding struct {
	AccountID, DomainID, CredentialID, Provider, EncryptedConfig, Recipient string
	// ConfigDomainID is the domain the encrypted config was actually stored
	// under, which may be an ancestor when receiving is inherited. It is the AAD
	// scope for EncryptedConfig, distinct from DomainID (the recipient's own
	// domain, used for authorization).
	ConfigDomainID string
}

// ResolveInboundBinding maps an envelope recipient to the domain and its
// configured receiving provider. The recipient's domain is globally unique, so
// account ownership is derived from it rather than supplied by the caller.
//
// The domain need not carry its own configuration: a subdomain that has been
// created with receiving inheritance resolves to the nearest configured
// ancestor (for example agent.example.com to example.com), so one receiver
// serves a whole zone. DomainID is always the recipient's own domain, so the
// downstream account/domain scope check still applies to that domain; only the
// credential is inherited. It returns ErrNotFound for unknown domains or
// domains with no applicable receiving config.
func (s *Store) ResolveInboundBinding(ctx context.Context, provider, recipient string) (InboundBinding, error) {
	var b InboundBinding
	at := strings.LastIndex(recipient, "@")
	if at < 0 {
		return b, ErrNotFound
	}
	domain := normalizeDomain(recipient[at+1:])
	var domainID, accountID string
	err := s.read.QueryRowContext(ctx, `SELECT id, account_id FROM domains WHERE name=?`, domain).Scan(&domainID, &accountID)
	if err == sql.ErrNoRows {
		return b, ErrNotFound
	}
	if err != nil {
		return b, err
	}
	cfg, err := s.ResolveDomainReceivingConfig(ctx, accountID, domainID, provider)
	if err != nil {
		if errors.Is(err, ErrNoProvider) {
			return b, ErrNotFound
		}
		return b, err
	}
	b.AccountID = accountID
	b.DomainID = domainID
	b.ConfigDomainID = cfg.DomainID
	b.CredentialID = cfg.ID
	b.Provider = cfg.Provider
	b.EncryptedConfig = cfg.EncryptedConfig
	b.Recipient = normalizeAddress(recipient)
	return b, nil
}
