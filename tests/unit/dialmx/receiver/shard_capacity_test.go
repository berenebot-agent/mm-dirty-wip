package receiver_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/dialmx/receiver"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// actualMaxDomains is the receiver's advertised per-session domain cap the test
// is pinned against.
const actualMaxDomains = 128

// TestReceiverMaxDomainsCapacityPacking drives a real receiver advertising its
// default 128-domain session cap with 127/128/129/256/257 domains and proves
// every configured domain becomes ready while no session is opened beyond the
// capacity-packed shard count. It then adds one more domain and proves existing
// domains are not re-authenticated (their grants are unchanged).
func TestReceiverMaxDomainsCapacityPacking(t *testing.T) {
	for _, n := range []int{127, 128, 129, 256, 257} {
		t.Run(fmt.Sprintf("domains=%d", n), func(t *testing.T) {
			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			dns := func(context.Context, string) ([]string, error) { return []string{mxwire.DomainTXT("key1", pub)}, nil }
			recv, srv, client := newReceiverServer(t, receiver.Config{
				LookupTXT: dns,
				// Injected generous source limits so sharding, not the per-IP
				// auth window, is what is under test.
				MaxAuthConcurrent: 512,
				AuthWindowMax:     100000,
			})
			names := make([]string, 0, n)
			domains := make([]mxdial.Domain, 0, n)
			for i := 0; i < n; i++ {
				name := fmt.Sprintf("d%04d.test", i)
				names = append(names, name)
				domains = append(domains, mxdial.Domain{Name: name, KeyID: "key1", PrivateKey: priv, ReceiverURLs: []string{srv.URL}})
			}
			be := &backend{domains: domains}
			manager := mxdial.New(be, mxdial.Config{DataDir: t.TempDir(), TLSConfig: client, ReconcileInterval: 20 * time.Millisecond, AllowPrivateDestinations: true})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); manager.Run(ctx) }()
			defer func() { cancel(); <-done }()

			deadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(deadline) && readyCount(manager, names) != n {
				time.Sleep(20 * time.Millisecond)
			}
			if got := readyCount(manager, names); got != n {
				t.Fatalf("%d/%d domains ready", got, n)
			}
			// Capacity packing: ceil(n/128) shards, never more.
			expectShards := (n + actualMaxDomains - 1) / actualMaxDomains
			waitLive := time.Now().Add(5 * time.Second)
			for time.Now().Before(waitLive) {
				if int(recv.Stats()["active_connections"]) <= expectShards {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if got := recv.Stats()["active_connections"]; got > int64(expectShards) {
				t.Fatalf("%d domains opened %d sessions, expected <= %d", n, got, expectShards)
			}

			before := grantSnapshot(manager, names)

			// Add one more domain; existing grants must not change.
			extra := fmt.Sprintf("x%04d.test", n)
			domains = append(domains, mxdial.Domain{Name: extra, KeyID: "key1", PrivateKey: priv, ReceiverURLs: []string{srv.URL}})
			be.mu.Lock()
			be.domains = domains
			be.mu.Unlock()
			if !waitReadyDomain(manager, extra, 15*time.Second) {
				t.Fatalf("added domain %s did not become ready", extra)
			}
			after := grantSnapshot(manager, names)
			for _, name := range names {
				if !before[name].Equal(after[name]) {
					t.Fatalf("existing domain %s was re-authenticated on add: %v -> %v", name, before[name], after[name])
				}
			}
		})
	}
}

func readyCount(m *mxdial.Manager, names []string) int {
	n := 0
	for _, name := range names {
		if s := m.Status(name); len(s) > 0 && s[0].State == "ready" {
			n++
		}
	}
	return n
}

func waitReadyDomain(m *mxdial.Manager, name string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if s := m.Status(name); len(s) > 0 && s[0].State == "ready" {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func grantSnapshot(m *mxdial.Manager, names []string) map[string]time.Time {
	out := make(map[string]time.Time, len(names))
	for _, name := range names {
		if s := m.Status(name); len(s) > 0 {
			out[name] = s[0].ExpiresAt
		} else {
			out[name] = time.Time{}
		}
	}
	return out
}
