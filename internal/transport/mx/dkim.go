package mx

import (
	"crypto"
	"fmt"
	"strings"

	"github.com/dellarb/mammothmail/internal/dkimsign"
)

// Field names for the DKIM signing settings. FieldDKIMSelector and
// FieldDKIMDomain are fixed rather than operator-supplied, but are still stored
// so the record shown to an operator and the signature actually produced are
// derived from the same values.
const (
	FieldDKIMEnabled    = "dkim_enabled"
	FieldDKIMSelector   = "dkim_selector"
	FieldDKIMDomain     = "dkim_domain"
	FieldDKIMPrivateKey = "dkim_private_key"
	FieldDKIMPublicKey  = "dkim_public_key"
)

// DKIMConfig is the resolved signing configuration for one message. It is
// derived from a stored Direct MX outbound config, never stored itself, so the
// effective selector and signing domain have exactly one definition.
type DKIMConfig struct {
	Enabled  bool
	Selector string
	Domain   string
	// PrivateKey is the PEM text of the signing key. It is populated only for
	// the delivery path, which must sign; the schema marks it secret so it is
	// never rendered.
	PrivateKey string
}

// ResolveDKIM reads the DKIM settings out of a decrypted Direct MX outbound
// config. defaultDomain is the domain mail is being sent from; it is used when
// no explicit signing domain is stored, so the common case signs as the domain
// the message is From.
//
// A config with signing off yields Enabled=false and no error. A config with
// signing on but an unusable key is an error: signing must never be silently
// skipped, because an operator who switched it on believes their mail is
// signed.
func ResolveDKIM(cfg map[string]any, defaultDomain string) (DKIMConfig, error) {
	out := DKIMConfig{
		Enabled:    strings.EqualFold(strings.TrimSpace(stringValue(cfg, FieldDKIMEnabled)), "on"),
		Selector:   strings.TrimSpace(stringValue(cfg, FieldDKIMSelector)),
		Domain:     strings.TrimSpace(stringValue(cfg, FieldDKIMDomain)),
		PrivateKey: stringValue(cfg, FieldDKIMPrivateKey),
	}
	if !out.Enabled {
		return out, nil
	}
	if out.Selector == "" {
		out.Selector = dkimsign.DefaultSelector
	}
	if out.Domain == "" {
		out.Domain = strings.TrimSpace(defaultDomain)
	}
	if out.Domain == "" {
		return DKIMConfig{}, fmt.Errorf("DKIM signing is enabled but no signing domain is available")
	}
	if strings.TrimSpace(out.PrivateKey) == "" {
		return DKIMConfig{}, fmt.Errorf("DKIM signing is enabled but no private key is configured")
	}
	return out, nil
}

// Signer parses the resolved private key. It is separate from ResolveDKIM so
// the parse cost is paid only on the delivery path, not on a config read.
func (c DKIMConfig) Signer() (crypto.Signer, error) {
	return dkimsign.ParsePrivateKey(c.PrivateKey)
}

func stringValue(cfg map[string]any, key string) string {
	v, _ := cfg[key].(string)
	return v
}
