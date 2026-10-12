package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dellarb/mailmoose/internal/dkimsign"
	"github.com/dellarb/mailmoose/internal/transport/mx"
)

// applyDKIMSendingKey resolves the DKIM signing key for a Direct MX sending
// configuration, in place.
//
// Three cases are handled, in this order:
//
//   - Signing off: any stored key is discarded. A key retained while signing is
//     off would silently reappear if signing were switched back on later,
//     against DNS that may since have been republished for a different key.
//   - Signing on with a key already resolved into merged: either the operator
//     pasted one or validateConfig retained the stored one. A pasted key is
//     validated; a retained key was validated when it was first stored.
//   - Signing on with no key: MailMoose mints an RSA-2048 keypair.
//
// validateConfig has already applied "blank retains the stored secret", so a
// key absent from merged means there is genuinely none to reuse.
func (s *Service) applyDKIMSendingKey(ctx context.Context, accountID, domainID, provider string, merged map[string]any) error {
	if !strings.EqualFold(provider, mx.Provider) {
		return nil
	}
	enabled := strings.EqualFold(strings.TrimSpace(asString(merged[mx.FieldDKIMEnabled])), "on")
	if !enabled {
		// Drop every DKIM field, including any retained private key, so a
		// disabled config carries no key material at rest.
		delete(merged, mx.FieldDKIMPrivateKey)
		delete(merged, mx.FieldDKIMPublicKey)
		return nil
	}

	keyPEM := asString(merged[mx.FieldDKIMPrivateKey])
	if strings.TrimSpace(keyPEM) == "" {
		privPEM, _, err := dkimsign.GenerateRSA2048()
		if err != nil {
			return fmt.Errorf("generate DKIM signing key: %w", err)
		}
		keyPEM = privPEM
	}
	key, err := dkimsign.ParsePrivateKey(keyPEM)
	if err != nil {
		// A pasted key that does not parse is the operator's error, reported
		// against the field rather than as an internal failure.
		return invalidConfig("Private key (PEM): %s", err.Error())
	}
	// Store the key in a canonical form so a save is idempotent: the operator's
	// paste is preserved, but surrounding whitespace and a missing trailing
	// newline are normalised, so re-saving without re-pasting does not rewrite
	// the stored value.
	keyPEM = dkimsign.CanonicalPEM(keyPEM)
	// Validate the selector and signing domain now, so a malformed value is
	// rejected at save rather than failing every delivery later.
	selector, err := validateDKIMSelector(asString(merged[mx.FieldDKIMSelector]))
	if err != nil {
		return err
	}
	domain, err := s.validateDKIMDomain(ctx, accountID, asString(merged[mx.FieldDKIMDomain]))
	if err != nil {
		return err
	}
	merged[mx.FieldDKIMPrivateKey] = keyPEM
	merged[mx.FieldDKIMSelector] = selector
	if domain != "" {
		merged[mx.FieldDKIMDomain] = domain
	}
	// The public key is derived, never accepted from the caller: a stored public
	// key could disagree with the stored private key and send the operator to a
	// DNS record that does not match what MailMoose signs with. It is stored so
	// a read does not have to re-parse the private key, and it is not secret.
	publicPEM, err := dkimsign.PublicKeyPEM(key)
	if err != nil {
		return fmt.Errorf("derive DKIM public key: %w", err)
	}
	merged[mx.FieldDKIMPublicKey] = publicPEM
	return nil
}

// validateDKIMSelector applies the default selector when none is given, and
// otherwise checks the value can form a DNS name. A selector is a DNS label, so
// spaces, dots and separators would produce a record name that cannot exist.
func validateDKIMSelector(raw string) (string, error) {
	selector := strings.TrimSpace(raw)
	if selector == "" {
		return dkimsign.DefaultSelector, nil
	}
	if len(selector) > 63 {
		return "", invalidConfig("DKIM selector must be at most 63 characters")
	}
	for i := 0; i < len(selector); i++ {
		c := selector[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return "", invalidConfig("DKIM selector may contain only letters, digits, hyphens and underscores")
		}
	}
	return selector, nil
}

// validateDKIMDomain checks an explicit signing domain. A blank value is valid
// and means "sign as the sending domain", resolved per message at delivery
// time. An explicit value must be one of the account's own domains: signing as
// a domain the account does not control produces a signature no receiver can
// verify, and would let one account claim another's identity.
func (s *Service) validateDKIMDomain(ctx context.Context, accountID, raw string) (string, error) {
	domain := strings.TrimSpace(raw)
	if domain == "" {
		return "", nil
	}
	if len(domain) > 253 || strings.ContainsAny(domain, " \t/@:") {
		return "", invalidConfig("Signing domain must be a bare DNS name")
	}
	for _, label := range strings.Split(strings.TrimSuffix(domain, "."), ".") {
		if label == "" || len(label) > 63 {
			return "", invalidConfig("Signing domain has an empty or over-long label")
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return "", invalidConfig("Signing domain contains an invalid character")
			}
		}
	}
	canonical := strings.ToLower(strings.TrimSuffix(domain, "."))
	domains, err := s.Store.ListDomains(ctx, accountID)
	if err != nil {
		return "", err
	}
	for _, d := range domains {
		if strings.EqualFold(strings.TrimSuffix(d.Name, "."), canonical) {
			return canonical, nil
		}
	}
	return "", invalidConfig("Signing domain %s is not a domain on this account", canonical)
}

// signMXMessage applies a DKIM signature to a Direct MX message. fromAddress is
// the address mail is being sent from; when no explicit signing domain is
// configured the address's domain is used, so a signature aligns with the From
// header without the operator configuring anything.
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
