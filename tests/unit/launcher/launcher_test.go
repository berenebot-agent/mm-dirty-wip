package launcher_test

import (
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/launcher"
)

func TestResolveCoreKeyPreservesConfigured(t *testing.T) {
	secret, err := launcher.ResolveCoreKey("configured-key")
	if err != nil {
		t.Fatal(err)
	}
	if secret != "configured-key" {
		t.Fatalf("got %q", secret)
	}
}

func TestResolveEdgeCredentialGenerates(t *testing.T) {
	secret, err := launcher.ResolveCoreKey("")
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) != 64 {
		t.Fatalf("generated secret length %d", len(secret))
	}
	secret2, err := launcher.ResolveCoreKey("")
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

	env := launcher.EdgeEnv("mail.example.com", "sekret", 3)
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{"APP_ENCRYPTION_KEY", "DATA_DIR", "MX_EDGE_KEYS"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("edge env leaked %s: %v", forbidden, env)
		}
	}
	for _, want := range []string{
		"DIALMX_MODE=single",
		"DIALMX_CORE_KEY=sekret",
		"DIALMX_LISTEN_ADDR=127.0.0.1:8443",
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
	env := launcher.EdgeEnv("", "s", 3)
	found := false
	for _, e := range env {
		if e == "MX_HOSTNAME=mailmoose-mx" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected mailmoose-mx fallback hostname")
	}
}
