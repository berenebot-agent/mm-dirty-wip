package mxdial_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

const validManifest = `{
  "schema_version": 1,
  "future_field": {"ignored": true},
  "receivers": [
    {"id":"antler-1","session_url":"https://antler1.example.test","smtp_hostname":"antler1.example.test","mx_priority":10},
    {"id":"antler-2","session_url":"https://antler2.example.test","smtp_hostname":"antler2.example.test","mx_priority":20}
  ]
}`

func TestEmbeddedAntlerManifestIsValid(t *testing.T) {
	receivers := mxdial.EmbeddedAntlerReceivers()
	if len(receivers) != 2 {
		t.Fatalf("embedded manifest receivers = %d, want 2", len(receivers))
	}
	if receivers[0].SessionURL != "https://antler1.hgolabs.com" || receivers[0].MXPriority != 10 {
		t.Fatalf("unexpected first receiver: %+v", receivers[0])
	}
}

func TestParseAntlerManifestValidates(t *testing.T) {
	// Unknown fields are tolerated so the hosted document can grow without
	// breaking older cores; known fields are still validated strictly.
	if _, err := mxdial.ParseAntlerManifest([]byte(validManifest)); err != nil {
		t.Fatalf("valid manifest with unknown field rejected: %v", err)
	}
	for name, raw := range map[string]string{
		"wrong schema":       `{"schema_version":2,"receivers":[{"id":"a","session_url":"https://a.test","smtp_hostname":"a.test","mx_priority":10}]}`,
		"empty receivers":    `{"schema_version":1,"receivers":[]}`,
		"non-canonical url":  `{"schema_version":1,"receivers":[{"id":"a","session_url":"https://a.test/","smtp_hostname":"a.test","mx_priority":10}]}`,
		"http url":           `{"schema_version":1,"receivers":[{"id":"a","session_url":"http://a.test","smtp_hostname":"a.test","mx_priority":10}]}`,
		"private literal":    `{"schema_version":1,"receivers":[{"id":"a","session_url":"https://127.0.0.1","smtp_hostname":"a.test","mx_priority":10}]}`,
		"localhost hostname": `{"schema_version":1,"receivers":[{"id":"a","session_url":"https://localhost","smtp_hostname":"a.test","mx_priority":10}]}`,
		"bad smtp hostname":  `{"schema_version":1,"receivers":[{"id":"a","session_url":"https://a.test","smtp_hostname":"not a host","mx_priority":10}]}`,
		"bad priority":       `{"schema_version":1,"receivers":[{"id":"a","session_url":"https://a.test","smtp_hostname":"a.test","mx_priority":70000}]}`,
		"duplicate id":       `{"schema_version":1,"receivers":[{"id":"a","session_url":"https://a.test","smtp_hostname":"a.test","mx_priority":10},{"id":"a","session_url":"https://b.test","smtp_hostname":"b.test","mx_priority":20}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := mxdial.ParseAntlerManifest([]byte(raw)); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

// TestAntlerResolverFetchesAndFallsBack proves the live fetch is used, cached,
// and that a fetch failure falls back to the last known good copy rather than
// failing setup.
func TestAntlerResolverFetchesAndFallsBack(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(validManifest))
	}))
	defer srv.Close()

	resolver := &mxdial.AntlerResolver{URL: srv.URL, Client: srv.Client(), TTL: time.Minute}
	got, err := resolver.Receivers(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("fetch: %v %+v", err, got)
	}
	if got[0].SessionURL != "https://antler1.example.test" {
		t.Fatalf("live manifest not used: %+v", got[0])
	}

	// A stale cache plus a failing fetch returns the cached copy.
	resolver.TTL = 0
	fail.Store(true)
	got, err = resolver.Receivers(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("cached fallback: %v %+v", err, got)
	}
}

// TestAntlerResolverUsesEmbeddedWhenNeverFetched proves setup still works with
// no network at all: the embedded manifest is used.
func TestAntlerResolverUsesEmbeddedWhenNeverFetched(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	resolver := &mxdial.AntlerResolver{URL: srv.URL, Client: srv.Client()}
	got, err := resolver.Receivers(context.Background())
	if err != nil {
		t.Fatalf("embedded fallback: %v", err)
	}
	if len(got) != 2 || got[0].SessionURL != "https://antler1.hgolabs.com" {
		t.Fatalf("embedded manifest not used: %+v", got)
	}
}
