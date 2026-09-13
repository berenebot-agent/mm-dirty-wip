package launcher_test

import (
	"strings"
	"testing"

	"gatehouse-mail/internal/launcher"
)

func TestResolveEdgeCredentialPicksDeterministic(t *testing.T) {
	keys := map[string]string{"edge-2": "b", "edge-1": "a", "edge-10": "c"}
	id, secret, err := launcher.ResolveEdgeCredential(keys)
	if err != nil {
		t.Fatal(err)
	}
	if id != "edge-1" || secret != "a" {
		t.Fatalf("got %q/%q, want edge-1/a", id, secret)
	}
}

func TestResolveEdgeCredentialGenerates(t *testing.T) {
	id, secret, err := launcher.ResolveEdgeCredential(nil)
	if err != nil {
		t.Fatal(err)
	}
	if id != "edge-1" {
		t.Fatalf("generated key id %q", id)
	}
	if len(secret) != 64 {
		t.Fatalf("generated secret length %d", len(secret))
	}
	_, secret2, err := launcher.ResolveEdgeCredential(nil)
	if err != nil {
		t.Fatal(err)
	}
	if secret == secret2 {
		t.Fatal("generated secrets should differ")
	}
}

func TestEdgeEnvScrubsAppSecrets(t *testing.T) {
	t.Setenv("APP_ENCRYPTION_KEY", "super-secret")
	t.Setenv("DATA_DIR", "/data")
	t.Setenv("MX_EDGE_KEYS", "edge-1:sekret")
	t.Setenv("MX_MAX_MESSAGE_BYTES", "1048576")

	env := launcher.EdgeEnv("mail.example.com", "edge-1", "sekret", 3)
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{"APP_ENCRYPTION_KEY", "DATA_DIR", "MX_EDGE_KEYS"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("edge env leaked %s: %v", forbidden, env)
		}
	}
	for _, want := range []string{
		"MX_EDGE_KEY_ID=edge-1",
		"MX_EDGE_SECRET=sekret",
		"GATEHOUSE_INGEST_URL=http://127.0.0.1:8082",
		"MX_LISTEN_ADDR=:2525",
		"MX_HOSTNAME=mail.example.com",
		"MX_SHUTDOWN_FD=3",
		"MX_MAX_MESSAGE_BYTES=1048576",
	} {
		found := false
		for _, e := range env {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("edge env missing %q in %v", want, env)
		}
	}
}

func TestEdgeEnvDefaultHostname(t *testing.T) {
	env := launcher.EdgeEnv("", "edge-1", "s", 3)
	found := false
	for _, e := range env {
		if e == "MX_HOSTNAME=gatehouse-mx" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected gatehouse-mx fallback hostname")
	}
}
