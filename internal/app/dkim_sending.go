package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dellarb/mailmoose/internal/dkimsign"
	"github.com/dellarb/mailmoose/internal/transport/mx"
)

// applyDKIMSendingKey resolves the DKIM signing material for a Direct MX sending
// configuration, in place.
//
// Signing is a single on/off choice: MailMoose generates the key. Two cases are
// handled:
//
//   - Signing off: every DKIM field is dropped, including any stored key, so a
//     disabled config carries no key material at rest and a key cannot silently
//     reappear later against DNS that has since been republished.
//   - Signing on: the existing key is kept, or a fresh RSA-2048 keypair is
//     generated when there is none. The selector and signing domain are fixed
//     here, so the record shown to an operator and the signature that is
//     produced are always derived from the same values.
//
// validateConfig has already applied "blank retains the stored secret", so a key
// absent from merged means there is genuinely none to reuse.
func (s *Service) applyDKIMSendingKey(ctx context.Context, accountID, domainID, provider, storedKey string, merged map[string]any) error {
	if !strings.EqualFold(provider, mx.Provider) {
		return nil
	}
	enabled := strings.EqualFold(strings.TrimSpace(asString(merged[mx.FieldDKIMEnabled])), "on")
	if !enabled {
		// Drop the whole set, so a disabled config carries no key material (and
		// no stale selector or signing domain) at rest.
		for _, field := range []string{
			mx.FieldDKIMPrivateKey, mx.FieldDKIMPublicKey,
			mx.FieldDKIMSelector, mx.FieldDKIMDomain,
		} {
			delete(merged, field)
		}
		return nil
	}

	// The signing key is always MailMoose-generated. validateConfig has already
	// merged any previously stored key (blank field) with anything a client
	// posted, so the only value that may be trusted is the one already stored:
	// a freshly posted key is discarded, because the key shown for DNS must be
	// the key that signs.
	keyPEM := strings.TrimSpace(storedKey)
	if keyPEM == "" {
		privPEM, _, err := dkimsign.GenerateRSA2048()
		if err != nil {
			return fmt.Errorf("generate DKIM signing key: %w", err)
		}
		keyPEM = privPEM
	}
	key, err := dkimsign.ParsePrivateKey(keyPEM)
	if err != nil {
		// The key is MailMoose-generated, so a stored key that no longer parses
		// is a corrupted config rather than operator error. Replacing it is the
		// only way to keep signing working.
		privPEM, _, gerr := dkimsign.GenerateRSA2048()
		if gerr != nil {
			return fmt.Errorf("regenerate DKIM signing key: %w", gerr)
		}
		if key, err = dkimsign.ParsePrivateKey(privPEM); err != nil {
			return fmt.Errorf("parse regenerated DKIM signing key: %w", err)
		}
		keyPEM = privPEM
	}

	// The signing domain is the domain mail is sent from, so an aligned
	// signature is produced without the operator choosing anything. The selector
	// is a fixed label: rotating a key means republishing the record, not
	// changing the selector.
	domain, err := s.sendingDomainName(ctx, accountID, domainID)
	if err != nil {
		return err
	}
	merged[mx.FieldDKIMPrivateKey] = dkimsign.CanonicalPEM(keyPEM)
	merged[mx.FieldDKIMSelector] = dkimsign.DefaultSelector
	merged[mx.FieldDKIMDomain] = domain

	// The public key is derived, never accepted from a caller: a stored public
	// key could disagree with the stored private key and send the operator to a
	// DNS record that does not match what MailMoose signs with.
	publicPEM, err := dkimsign.PublicKeyPEM(key)
	if err != nil {
		return fmt.Errorf("derive DKIM public key: %w", err)
	}
	merged[mx.FieldDKIMPublicKey] = publicPEM
	return nil
}

// sendingDomainName returns the domain's DNS name, which is both the sending
// domain and the DKIM signing domain.
func (s *Service) sendingDomainName(ctx context.Context, accountID, domainID string) (string, error) {
	d, err := s.Store.GetDomain(ctx, accountID, domainID)
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(d.Name), ".")), nil
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

// asString reads a config value as a string without caring whether the caller
// supplied it as a string or a richer type.
func asString(v any) string {
	s, _ := v.(string)
	return s
}

// ErrDKIMUnavailable reports that signing was requested for a message but the
// configuration cannot produce a signature. Delivery fails rather than sending
// unsigned mail the operator believes is signed.
var ErrDKIMUnavailable = errors.New("DKIM signing is enabled but the configuration is unusable")
