package mx_test

import (
	"testing"

	"github.com/dellarb/mailmoose/internal/dkimsign"
	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/mx"
)

// dkimFields returns the schema fields for the Direct MX outbound config.
func dkimFields(t *testing.T) map[string]transport.ConfigField {
	t.Helper()
	schema, ok := transport.LookupOutbound("mx")
	if !ok {
		t.Fatal("mx outbound provider is not registered")
	}
	sp, ok := schema.(transport.ConfigSchemaProvider)
	if !ok {
		t.Fatal("mx outbound provider has no config schema")
	}
	out := map[string]transport.ConfigField{}
	for _, f := range sp.ConfigFields() {
		out[f.Name] = f
	}
	return out
}

// TestDKIMSchemaShape pins the operator-facing surface: an on/off choice and a
// generated key. Signing is a single decision, so there is deliberately no
// selector, signing-domain or key input to fill in.
func TestDKIMSchemaShape(t *testing.T) {
	fields := dkimFields(t)
	enabled, ok := fields[mx.FieldDKIMEnabled]
	if !ok {
		t.Fatal("the DKIM signing toggle is not in the schema")
	}
	if enabled.Type != "select" || enabled.Default != "off" {
		t.Errorf("dkim_enabled = %q default %q, want a select defaulting to off", enabled.Type, enabled.Default)
	}
	key, ok := fields[mx.FieldDKIMPrivateKey]
	if !ok {
		t.Fatal("the generated key field is not in the schema")
	}
	if !key.Secret || !key.Generated {
		t.Error("the DKIM private key must be secret and generated")
	}
	if key.Overridable {
		t.Error("the DKIM key is generated only; it must not be operator-supplied")
	}
	// There is no field for the selector or signing domain: both are fixed.
	if _, ok := fields[mx.FieldDKIMSelector]; ok {
		t.Error("a selector field is exposed, but the selector is fixed")
	}
	if _, ok := fields[mx.FieldDKIMDomain]; ok {
		t.Error("a signing-domain field is exposed, but the domain is the sending domain")
	}
	if _, ok := fields[mx.FieldDKIMPublicKey]; ok {
		t.Error("a public-key input is exposed, but the key is never supplied by a caller")
	}
}

func TestResolveDKIMDisabled(t *testing.T) {
	for name, cfg := range map[string]map[string]any{
		"nil config":     nil,
		"empty":          {},
		"explicitly off": {"dkim_enabled": "off"},
		"unrecognised":   {"dkim_enabled": "yes"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := mx.ResolveDKIM(cfg, "example.com")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Enabled {
				t.Error("signing reported enabled")
			}
		})
	}
}

// TestResolveDKIMEnabledAppliesDefaults proves the values an operator gets by
// switching signing on and nothing else.
func TestResolveDKIMEnabledAppliesDefaults(t *testing.T) {
	privPEM, _, err := dkimsign.GenerateRSA2048()
	if err != nil {
		t.Fatal(err)
	}
	got, err := mx.ResolveDKIM(map[string]any{
		"dkim_enabled":     "on",
		"dkim_private_key": privPEM,
	}, "example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Selector != dkimsign.DefaultSelector {
		t.Errorf("selector = %q, want the default %q", got.Selector, dkimsign.DefaultSelector)
	}
	if got.Domain != "example.com" {
		t.Errorf("domain = %q, want example.com", got.Domain)
	}
	if _, err := got.Signer(); err != nil {
		t.Errorf("resolved key does not parse: %v", err)
	}
}

// TestResolveDKIMEnabledRequiresUsableKey proves signing cannot be on without a
// usable key: a missing key or no domain is an error, never a silent unsigned
// send.
func TestResolveDKIMEnabledRequiresUsableKey(t *testing.T) {
	t.Run("no key", func(t *testing.T) {
		if _, err := mx.ResolveDKIM(map[string]any{"dkim_enabled": "on"}, "example.com"); err == nil {
			t.Fatal("expected an error when signing is on with no key")
		}
	})
	t.Run("no domain available", func(t *testing.T) {
		privPEM, _, err := dkimsign.GenerateRSA2048()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := mx.ResolveDKIM(map[string]any{"dkim_enabled": "on", "dkim_private_key": privPEM}, ""); err == nil {
			t.Fatal("expected an error when no signing domain can be resolved")
		}
	})
	t.Run("unparseable key", func(t *testing.T) {
		got, err := mx.ResolveDKIM(map[string]any{"dkim_enabled": "on", "dkim_private_key": "garbage"}, "example.com")
		if err == nil {
			if _, serr := got.Signer(); serr == nil {
				t.Fatal("Signer accepted an unparseable key")
			}
		}
	})
}
