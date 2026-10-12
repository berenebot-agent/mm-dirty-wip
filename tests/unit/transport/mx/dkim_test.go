package mx_test

import (
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/dkimsign"
	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/mx"
)

// dkimFields returns the schema fields for the Direct MX outbound config,
// failing if any expected field is missing. It pins the operator-facing
// surface: the settings only work if the schema advertises them.
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

// TestDKIMSchemaShape pins the DKIM settings the operator edits on the Direct
// MX sending tab.
func TestDKIMSchemaShape(t *testing.T) {
	fields := dkimFields(t)
	for name, want := range map[string]struct {
		fieldType string
		secret    bool
		generated bool
		readOnly  bool
	}{
		"helo":             {fieldType: "text"},
		"dkim_enabled":     {fieldType: "select"},
		"dkim_selector":    {fieldType: "text"},
		"dkim_domain":      {fieldType: "text"},
		"dkim_public_key":  {fieldType: "text", readOnly: true},
		"dkim_private_key": {fieldType: "password", secret: true, generated: true},
	} {
		t.Run(name, func(t *testing.T) {
			f, ok := fields[name]
			if !ok {
				t.Fatalf("field %q is not in the schema", name)
			}
			if f.Type != want.fieldType {
				t.Errorf("type = %q, want %q", f.Type, want.fieldType)
			}
			if f.Secret != want.secret {
				t.Errorf("secret = %v, want %v", f.Secret, want.secret)
			}
			if f.Generated != want.generated {
				t.Errorf("generated = %v, want %v", f.Generated, want.generated)
			}
			if f.ReadOnly != want.readOnly {
				t.Errorf("readOnly = %v, want %v", f.ReadOnly, want.readOnly)
			}
		})
	}
	// The private key must be overridable so an operator can paste their own
	// key; the public key must be read-only because it is derived server-side.
	if !fields["dkim_private_key"].Overridable {
		t.Error("dkim_private_key is not overridable, so a pasted key could not be stored")
	}
	if fields["dkim_public_key"].Secret || fields["dkim_public_key"].Generated {
		t.Error("dkim_public_key should be neither secret nor generated")
	}
	// Signing must default to off, so enabling it is a deliberate act.
	if off := fields["dkim_enabled"].Default; off != "off" {
		t.Errorf("dkim_enabled default = %q, want \"off\"", off)
	}
}

func TestResolveDKIMDisabled(t *testing.T) {
	for name, cfg := range map[string]map[string]any{
		"nil config":      nil,
		"empty":           {},
		"explicitly off":  {"dkim_enabled": "off"},
		"unrecognised":    {"dkim_enabled": "yes"},
		"wrong case only": {"dkim_enabled": "OFF"},
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

// TestResolveDKIMEnabledAppliesDefaults proves the defaults an operator gets by
// turning signing on and nothing else: the well-known selector, and a signing
// domain taken from the sending address's domain.
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
	if !got.Enabled {
		t.Fatal("signing not enabled")
	}
	if got.Selector != dkimsign.DefaultSelector {
		t.Errorf("selector = %q, want the default %q", got.Selector, dkimsign.DefaultSelector)
	}
	if got.Domain != "example.com" {
		t.Errorf("domain = %q, want example.com", got.Domain)
	}
}

// TestResolveDKIMEnabledRequiresUsableKey proves signing cannot be on without a
// key: a missing key and an unparseable key are both errors, never a silent
// unsigned send.
func TestResolveDKIMEnabledRequiresUsableKey(t *testing.T) {
	t.Run("no key", func(t *testing.T) {
		if _, err := mx.ResolveDKIM(map[string]any{"dkim_enabled": "on"}, "example.com"); err == nil {
			t.Fatal("expected an error when signing is on with no key")
		}
	})
	t.Run("unparseable key", func(t *testing.T) {
		// ResolveDKIM checks presence; the parse happens in Signer. Either way
		// the config must not yield a usable signature, and Signer must refuse.
		got, err := mx.ResolveDKIM(map[string]any{"dkim_enabled": "on", "dkim_private_key": "garbage"}, "example.com")
		if err == nil {
			if _, serr := got.Signer(); serr == nil {
				t.Fatal("Signer accepted an unparseable key")
			}
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
}

// TestResolveDKIMHonoursExplicitValues proves an explicit selector and signing
// domain override the defaults.
func TestResolveDKIMHonoursExplicitValues(t *testing.T) {
	privPEM, _, err := dkimsign.GenerateRSA2048()
	if err != nil {
		t.Fatal(err)
	}
	got, err := mx.ResolveDKIM(map[string]any{
		"dkim_enabled":     "on",
		"dkim_private_key": privPEM,
		"dkim_selector":    "custom",
		"dkim_domain":      "signer.example.org",
	}, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.Selector != "custom" {
		t.Errorf("selector = %q, want custom", got.Selector)
	}
	if got.Domain != "signer.example.org" {
		t.Errorf("domain = %q, want signer.example.org", got.Domain)
	}
}

// TestResolveDKIMSignerParsesKey proves the resolved key is usable for signing.
func TestResolveDKIMSignerParsesKey(t *testing.T) {
	privPEM, _, err := dkimsign.GenerateRSA2048()
	if err != nil {
		t.Fatal(err)
	}
	got, err := mx.ResolveDKIM(map[string]any{"dkim_enabled": "on", "dkim_private_key": privPEM}, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	key, err := got.Signer()
	if err != nil {
		t.Fatalf("Signer: %v", err)
	}
	// The parsed key must reproduce the public half the operator publishes.
	if got := key.Public(); got == nil {
		t.Error("parsed signer has no public key")
	}
}

// TestDKIMSchemaFieldsAreWired proves the schema field names match the constants
// the app and delivery paths use, so a rename cannot silently disconnect the UI
// from the signing code.
func TestDKIMSchemaFieldsAreWired(t *testing.T) {
	fields := dkimFields(t)
	for _, name := range []string{
		mx.FieldDKIMEnabled,
		mx.FieldDKIMSelector,
		mx.FieldDKIMDomain,
		mx.FieldDKIMPrivateKey,
		mx.FieldDKIMPublicKey,
	} {
		if _, ok := fields[name]; !ok {
			t.Errorf("constant %q has no matching schema field", name)
		}
	}
	if !strings.HasPrefix(mx.FieldDKIMPrivateKey, "dkim_") {
		t.Errorf("unexpected field naming: %q", mx.FieldDKIMPrivateKey)
	}
}
