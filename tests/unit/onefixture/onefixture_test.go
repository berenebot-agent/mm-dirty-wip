package onefixture_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/dellarb/mammothmail/internal/transport/one"
	"github.com/dellarb/mammothmail/tests/support/onefixture"
)

// TestRetiredKeyServesInFlightRequest pins the behaviour the One import screen
// test relies on: a connection a test retires from discovery still answers a
// request that was already in flight with its key, and stays hidden from
// discovery.
func TestRetiredKeyServesInFlightRequest(t *testing.T) {
	t.Parallel()
	f := onefixture.New(t)
	f.Change(func(f *onefixture.Fixture) {
		f.RetiredKeys = []string{onefixture.Selector}
		f.Connections = []map[string]any{
			{"id": "fixture-other-connection", "key": "fixture-other-selector", "platform": "gmail", "active": true, "state": "operational", "environment": "live"},
		}
	})

	req, err := http.NewRequest("GET", "https://api.withone.ai/v1/passthrough/gmail/v1/users/me/profile", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-One-Secret", onefixture.Key)
	req.Header.Set("X-One-Connection-Key", onefixture.Selector)
	req.Header.Set("X-One-Action-Id", "GET-/v1/passthrough/gmail/v1/users/me/profile")
	resp, err := f.Client(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("in-flight request with the retired key answered %d", resp.StatusCode)
	}

	connections, err := one.NewClient(onefixture.Key, f.Client(t), nil).Connections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(connections) != 1 || connections[0].Key == onefixture.Selector {
		t.Fatalf("discovery returned the retired connection: %+v", connections)
	}
}
