package mxdial_test

import (
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/mxwire"
)

// TestNotMXReasonMapsToRejected proves the core surfaces a receiver's routing-gate
// verdict as its own status vocabulary: a rejected AuthResult with reason not_mx
// becomes state "rejected" with reason "not_mx", distinct from the generic
// authentication_failed that any other unknown reason collapses to.
func TestNotMXReasonMapsToRejected(t *testing.T) {
	rc := newFakeReceiver(t)
	rc.onAuth = func(fc *fakeConn, ch uint64, a mxwire.DomainAuth) {
		fc.send(mxwire.FrameAuthResult, 0, ch, mxwire.AuthResult{Domain: a.Domain, KeyID: a.KeyID, Accepted: false, Reason: "not_mx"})
	}
	be := &scriptedBackend{}
	be.setDomains(wireDomain(rc, "routed.test", "", ""))
	m, cancel := managerFor(t, be, rc, t.TempDir())
	defer cancel()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range m.Status("routed.test") {
			if s.State == "rejected" {
				if s.Reason != "not_mx" {
					t.Fatalf("rejected reason = %q, want not_mx", s.Reason)
				}
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("domain never reported rejected: %#v", m.Status("routed.test"))
}
