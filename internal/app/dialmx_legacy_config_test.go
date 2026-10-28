package app

import (
	"testing"

	"github.com/dellarb/mammothmail/internal/transport/mxdial"
)

// An existing domain config is encrypted, so a SQL migration cannot rewrite
// the old service and metadata keys. Decoding must preserve receiver identity
// and normalise those fields before any API read or subsequent save.
func TestNormalizeLegacyDialMXReceivingConfig(t *testing.T) {
	t.Parallel()
	snapshot := `[{"id":"antler-1","session_url":"https://old.example.test","smtp_hostname":"old.example.test","mx_priority":10}]`
	values := map[string]any{
		"service":                "antler",
		"antler_receivers":       snapshot,
		"antler_manifest_source": "embedded",
		"antler_manifest_at":     "2026-10-18T05:12:03Z",
		"receiver_urls":          "https://old.example.test",
		"setup_id":               "setup_existing",
		"contact_email":          "ops@example.test",
	}
	normalizeDialMXReceivingConfig(values)
	normalizeDialMXReceivingConfig(values)
	if values["service"] != mxdial.ServiceHosted {
		t.Fatalf("legacy service was not normalised: %v", values["service"])
	}
	if values["receiver_snapshot"] != snapshot ||
		values["receiver_manifest_source"] != "embedded" ||
		values["receiver_manifest_at"] != "2026-10-18T05:12:03Z" {
		t.Fatalf("legacy metadata was not preserved: %#v", values)
	}
	if values["receiver_urls"] != "https://old.example.test" ||
		values["setup_id"] != "setup_existing" ||
		values["contact_email"] != "ops@example.test" {
		t.Fatalf("receiver identity or contact changed: %#v", values)
	}
	for _, key := range []string{"antler_receivers", "antler_manifest_source", "antler_manifest_at"} {
		if _, ok := values[key]; ok {
			t.Fatalf("legacy key remains in normalised config: %s", key)
		}
	}
}

func TestNormalizeDialMXPrefersCanonicalKeys(t *testing.T) {
	t.Parallel()
	values := map[string]any{
		"service":                  mxdial.ServiceHosted,
		"receiver_snapshot":        "canonical",
		"antler_receivers":         "legacy",
		"receiver_manifest_source": "github",
		"antler_manifest_source":   "embedded",
	}
	normalizeDialMXReceivingConfig(values)
	if values["receiver_snapshot"] != "canonical" || values["receiver_manifest_source"] != "github" {
		t.Fatalf("canonical metadata was overwritten: %#v", values)
	}
	if _, ok := values["antler_receivers"]; ok {
		t.Fatal("old receiver key was not removed")
	}
	if _, ok := values["antler_manifest_source"]; ok {
		t.Fatal("old manifest key was not removed")
	}
}
