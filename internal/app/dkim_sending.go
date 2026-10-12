package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dellarb/mailmoose/internal/dkimsign"
	"github.com/dellarb/mailmoose/internal/transport/mx"
)

// applyDKIMSendingKey writes the domain's DKIM signing material into a Direct MX
// sending configuration, in place.
//
// The key itself lives on the domain row (generated when the domain is created,
// or by the startup backfill), not in the sending configuration. This copies it
// into the config so the delivery path signs from the same values that the
// operator was shown: the enabled flag decides whether signing is used, but the
// key material is always present so the record can be shown before the first
// save. The public key is derived here and never accepted from a caller, so the
// stored public half can never disagree with the private half.
func (s *Service) applyDKIMSendingKey(ctx context.Context, accountID, domainID, provider string, merged map[string]any) error {
	if !strings.EqualFold(provider, mx.Provider) {
		return nil
	}
	key, err := s.EnsureDomainDKIMKey(ctx, accountID, domainID)
	if err != nil {
		return err
	}
	return s.applyDomainDKIMToSendingConfig(merged, key)
}

// applyDomainDKIMToSendingConfig writes already-resolved domain DKIM material into
// a sending configuration map, deriving the public key so it always matches the
// private key.
func (s *Service) applyDomainDKIMToSendingConfig(merged map[string]any, key DKIMKey) error {
	parsed, err := dkimsign.ParsePrivateKey(key.PrivateKey)
	if err != nil {
		return fmt.Errorf("parse domain DKIM signing key: %w", err)
	}
	publicPEM, err := dkimsign.PublicKeyPEM(parsed)
	if err != nil {
		return fmt.Errorf("derive DKIM public key: %w", err)
	}
	selector := strings.TrimSpace(key.Selector)
	if selector == "" {
		selector = dkimsign.DefaultSelector
	}
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(key.Domain), "."))
	merged[mx.FieldDKIMPrivateKey] = dkimsign.CanonicalPEM(key.PrivateKey)
	merged[mx.FieldDKIMSelector] = selector
	merged[mx.FieldDKIMDomain] = domain
	merged[mx.FieldDKIMPublicKey] = publicPEM
	return nil
}

// signMXMessage applies a DKIM signature to a Direct MX message. fromAddress is
// the address mail is being sent from; a config that stored no signing domain
// falls back to the address's domain, so a signature still aligns with the From
// header.
//
// A configuration that enables signing but cannot produce a signature is an
// error, never a silent skip: sending unsigned mail that the operator believes
// is signed would defeat the point of enabling it.
func signMXMessage(cfg map[string]any, fromAddress string, raw []byte) ([]byte, error) {
	resolved, err := mx.ResolveDKIM(cfg, addressDomain(fromAddress))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrDKIMUnavailable, err.Error())
	}
	if !resolved.Enabled {
		return raw, nil
	}
	key, err := resolved.Signer()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrDKIMUnavailable, err.Error())
	}
	signed, err := dkimsign.Sign(raw, resolved.Domain, resolved.Selector, key)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrDKIMUnavailable, err.Error())
	}
	return signed, nil
}

// addressDomain returns the domain part of an email address, or the input
// unchanged when it carries no @ (so a bare domain still yields a usable SDID).
func addressDomain(address string) string {
	if _, domain, ok := strings.Cut(strings.TrimSpace(address), "@"); ok {
		return domain
	}
	return strings.TrimSpace(address)
}

// ErrDKIMUnavailable reports that signing was requested for a message but the
// configuration cannot produce a signature. Delivery fails rather than sending
// unsigned mail the operator believes is signed.
var ErrDKIMUnavailable = errors.New("DKIM signing is enabled but the configuration is unusable")
