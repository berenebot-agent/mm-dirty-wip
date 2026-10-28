package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dellarb/mammothmail/internal/dkimsign"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/internal/transport/mx"
)

// domainDKIMAAD binds a domain's encrypted DKIM private key to its account and
// domain, so a key cannot be replayed against another domain.
func domainDKIMAAD(accountID, domainID string) string {
	return "domain_dkim:" + accountID + ":" + domainID
}

// DKIMKey is the resolved (decrypted) DKIM signing material for one domain.
type DKIMKey struct {
	PrivateKey string
	PublicKey  string
	Selector   string
	Domain     string
}

// EnsureDomainDKIMKey makes sure a domain has a DKIM signing key, generating and
// storing one when it has none. It is idempotent: an existing, parseable key is
// left untouched and returned as-is. The key is derived and stored rather than
// passed in, so the stored public half can never disagree with the private half.
func (s *Service) EnsureDomainDKIMKey(ctx context.Context, accountID, domainID string) (DKIMKey, error) {
	existing, err := s.ServiceDomainDKIM(ctx, accountID, domainID)
	if err != nil {
		return DKIMKey{}, err
	}
	if strings.TrimSpace(existing.PrivateKey) != "" {
		if _, perr := dkimsign.ParsePrivateKey(existing.PrivateKey); perr == nil {
			return existing, nil
		}
		// A stored key that no longer parses is a corrupted row, not operator
		// error. Replace it so signing keeps working.
	}
	d, err := s.Store.GetDomain(ctx, accountID, domainID)
	if err != nil {
		return DKIMKey{}, err
	}
	privPEM, pubPEM, err := dkimsign.GenerateRSA2048()
	if err != nil {
		return DKIMKey{}, fmt.Errorf("generate domain DKIM key: %w", err)
	}
	enc, err := s.EncryptSecretAAD(domainDKIMAAD(accountID, domainID), []byte(privPEM))
	if err != nil {
		return DKIMKey{}, err
	}
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d.Name), "."))
	rec := store.DomainDKIM{
		PrivateKey: enc,
		PublicKey:  pubPEM,
		Selector:   dkimsign.DefaultSelector,
		Domain:     domain,
	}
	if err := s.Store.SetDomainDKIM(ctx, accountID, domainID, rec); err != nil {
		return DKIMKey{}, err
	}
	return DKIMKey{PrivateKey: privPEM, PublicKey: pubPEM, Selector: rec.Selector, Domain: rec.Domain}, nil
}

// ServiceDomainDKIM returns a domain's decrypted DKIM signing material, or the
// zero value when the domain has none yet. A missing or foreign domain returns
// ErrNotFound.
func (s *Service) ServiceDomainDKIM(ctx context.Context, accountID, domainID string) (DKIMKey, error) {
	rec, err := s.Store.GetDomainDKIM(ctx, accountID, domainID)
	if err != nil {
		return DKIMKey{}, err
	}
	out := DKIMKey{PublicKey: rec.PublicKey, Selector: rec.Selector, Domain: rec.Domain}
	if strings.TrimSpace(rec.PrivateKey) == "" {
		return out, nil
	}
	b, err := s.DecryptSecretAAD(domainDKIMAAD(accountID, domainID), rec.PrivateKey)
	if err != nil {
		return DKIMKey{}, err
	}
	out.PrivateKey = string(b)
	return out, nil
}

// RotateDomainDKIMKey mints a fresh DKIM signing key for a domain, replacing the
// stored one. The selector is unchanged, so the operator republishes the same
// record name with the new value. It is available for any domain, whether or not
// it currently signs.
func (s *Service) RotateDomainDKIMKey(ctx context.Context, accountID, domainID string) (DKIMKey, error) {
	_, err := s.Store.GetDomain(ctx, accountID, domainID)
	if err != nil {
		return DKIMKey{}, err
	}
	privPEM, pubPEM, err := dkimsign.GenerateRSA2048()
	if err != nil {
		return DKIMKey{}, fmt.Errorf("rotate domain DKIM key: %w", err)
	}
	enc, err := s.EncryptSecretAAD(domainDKIMAAD(accountID, domainID), []byte(privPEM))
	if err != nil {
		return DKIMKey{}, err
	}
	if err := s.Store.SetDomainDKIM(ctx, accountID, domainID, store.DomainDKIM{
		PrivateKey: enc,
		PublicKey:  pubPEM,
		Selector:   dkimsign.DefaultSelector,
		Domain:     strings.ToLower(strings.TrimSuffix(strings.TrimSpace(mustDomainName(s, ctx, accountID, domainID)), ".")),
	}); err != nil {
		return DKIMKey{}, err
	}
	// A Direct MX sender signs with a copy of this key held in its sending
	// configuration, so refresh that copy now the key has changed.
	if err := s.syncDirectMXDKIM(ctx, accountID, domainID); err != nil {
		return DKIMKey{}, err
	}
	return s.ServiceDomainDKIM(ctx, accountID, domainID)
}

// BackfillDomainDKIMKeys ensures every domain in every account has a DKIM key,
// generating any that are missing. It runs at startup so a pre-existing
// installation gets keys for its domains too.
func (s *Service) BackfillDomainDKIMKeys(ctx context.Context) error {
	refs, err := s.Store.DomainsMissingDKIM(ctx)
	if err != nil {
		return err
	}
	generated := 0
	for _, ref := range refs {
		if _, err := s.EnsureDomainDKIMKey(ctx, ref.AccountID, ref.ID); err != nil {
			s.Log.Warn("could not generate domain DKIM key at startup", "domain_id", ref.ID, "error", err)
			continue
		}
		generated++
	}
	if generated > 0 {
		s.Log.Info("generated missing domain DKIM keys", "count", generated)
	}
	return nil
}

func mustDomainName(s *Service, ctx context.Context, accountID, domainID string) string {
	d, err := s.Store.GetDomain(ctx, accountID, domainID)
	if err != nil {
		return ""
	}
	return d.Name
}

// syncDirectMXDKIM refreshes the DKIM copy inside a domain's Direct MX sending
// configuration from the domain's own key. It is a no-op when the domain has no
// Direct MX sending configuration.
func (s *Service) syncDirectMXDKIM(ctx context.Context, accountID, domainID string) error {
	existing, exists, err := s.getSendingConfig(ctx, accountID, domainID)
	if err != nil || !exists {
		return err
	}
	if !strings.EqualFold(normalizeProvider(existing.Provider), mx.Provider) {
		return nil
	}
	cfg, err := s.DecryptDomainSendingConfig(existing)
	if err != nil {
		return err
	}
	key, err := s.ServiceDomainDKIM(ctx, accountID, domainID)
	if err != nil {
		return err
	}
	if err := s.applyDomainDKIMToSendingConfig(cfg, key); err != nil {
		return err
	}
	enc, err := s.encryptConfig(configAAD(accountID, domainID), cfg)
	if err != nil {
		return err
	}
	_, err = s.Store.SaveDomainSendingConfig(ctx, accountID, domainID, mx.Provider, enc, store.ConfigVersion{ID: existing.ID, Revision: existing.Revision})
	if errors.Is(err, store.ErrConflict) {
		return nil
	}
	return err
}
