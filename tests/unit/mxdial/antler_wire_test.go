package mxdial_test

import (
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// wireDomain builds one dialer domain for a fake receiver, optionally with the
// Antler MX registration metadata. The signing key must be the receiver's own
// public key or the fake rejects the proof.
func wireDomain(rc *fakeReceiver, name, contact, setupID string) mxdial.Domain {
	return mxdial.Domain{Name: name, KeyID: rc.keyID, PrivateKey: rc.priv, ReceiverURLs: []string{rc.url()}, ContactEmail: contact, SetupID: setupID}
}

// TestDomainAuthCarriesRegistrationMetadata proves the dialer attaches the
// optional Antler MX contact email and setup id to every DomainAuth. The
// standard fake challenge exchange runs so the proof completes; a passive
// onFrame hook observes the frame the dialer sent.
func TestDomainAuthCarriesRegistrationMetadata(t *testing.T) {
	rc := newFakeReceiver(t)
	got := make(chan mxwire.DomainAuth, 2)
	rc.onFrame = func(_ *fakeConn, f mxwire.Frame) bool {
		if f.Type == mxwire.FrameDomainAuth {
			var a mxwire.DomainAuth
			if mxwire.DecodeFrame(f, &a) == nil {
				select {
				case got <- a:
				default:
				}
			}
		}
		return false
	}
	be := &scriptedBackend{}
	be.setDomains(wireDomain(rc, "antler.test", "ops@example.test", "setup_ab12"))
	m, cancel := managerFor(t, be, rc, t.TempDir())
	defer cancel()
	waitReady(t, m, "antler.test", 4*time.Second)
	for _, status := range m.Status("antler.test") {
		if status.State == "ready" && status.KeyID != rc.keyID {
			t.Fatalf("readiness must identify the authenticated key: %+v", status)
		}
	}

	select {
	case a := <-got:
		if a.Domain != "antler.test" || a.ContactEmail != "ops@example.test" || a.SetupID != "setup_ab12" {
			t.Fatalf("metadata not carried: %+v", a)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("no DomainAuth observed")
	}
}

// TestCustomDomainAuthOmitsMetadata proves a custom (non-Antler) domain sends a
// DomainAuth with no registration metadata, so a legacy receiver sees the same
// frame it always did.
func TestCustomDomainAuthOmitsMetadata(t *testing.T) {
	rc := newFakeReceiver(t)
	got := make(chan mxwire.DomainAuth, 2)
	rc.onFrame = func(_ *fakeConn, f mxwire.Frame) bool {
		if f.Type == mxwire.FrameDomainAuth {
			var a mxwire.DomainAuth
			if mxwire.DecodeFrame(f, &a) == nil {
				select {
				case got <- a:
				default:
				}
			}
		}
		return false
	}
	be := &scriptedBackend{}
	be.setDomains(wireDomain(rc, "custom.test", "", ""))
	m, cancel := managerFor(t, be, rc, t.TempDir())
	defer cancel()
	waitReady(t, m, "custom.test", 4*time.Second)

	select {
	case a := <-got:
		if a.ContactEmail != "" || a.SetupID != "" {
			t.Fatalf("custom domain carried metadata: %+v", a)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("no DomainAuth observed")
	}
}
