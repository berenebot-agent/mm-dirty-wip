package mxdial_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// TestUnreachableReceiverIsReportedNotConnecting proves a receiver the core
// cannot reach at all is reported as unreachable, and never as a pending
// authorization. A receiver that is down and one that is still authorizing this
// domain are different facts, and the operator has nothing to publish in DNS in
// the first case: presenting the second for the first sends them looking for a
// DNS problem that does not exist.
func TestUnreachableReceiverIsReportedNotConnecting(t *testing.T) {
	// A port with nothing bound to it: the connection is refused immediately,
	// so the failure is attributable to reachability and not to a timeout.
	addr := reserveLoopbackAddr(t)

	rc := newFakeReceiver(t)
	b := &scriptedBackend{}
	b.setDomains(mxdial.Domain{Name: "example.test", KeyID: rc.keyID, PrivateKey: rc.priv, ReceiverURLs: []string{"https://" + addr}})
	m, _ := managerFor(t, b, rc, t.TempDir())

	waitState(t, m, "example.test", "unreachable", 5*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	var last mxdial.Status
	for time.Now().Before(deadline) {
		rows := m.Status("example.test")
		if len(rows) == 0 {
			t.Fatalf("no status row for a configured domain")
		}
		last = rows[0]
		if last.State != "unreachable" {
			t.Fatalf("state changed to %q after the receiver was reported unreachable", last.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if last.Reason != mxdial.ReasonUnreachable {
		t.Fatalf("reason = %q, want %q", last.Reason, mxdial.ReasonUnreachable)
	}
}

// TestSingleModeStatusIncludesConnectionRow proves a single-mode manager reports
// its configured receiver for a domain that has no per-domain row, so the UI can
// render the connection the operator actually configured. Without this the only
// row a caller ever sees is one a completed handshake produced, and an
// unreachable receiver is indistinguishable from one that was never looked at.
func TestSingleModeStatusIncludesConnectionRow(t *testing.T) {
	srv := startSingleReceiver(t, "private-key")
	m := startManager(t, mxdial.Config{
		DataDir:                  t.TempDir(),
		ReceiverURL:              srv.URL,
		CoreKey:                  "private-key",
		ReconcileInterval:        20 * time.Millisecond,
		AllowPrivateDestinations: true,
	})
	waitConnectionState(t, m, "ready", 5*time.Second)

	rows := m.Status("example.test")
	if len(rows) != 1 {
		t.Fatalf("rows = %#v, want exactly the configured receiver", rows)
	}
	if rows[0].ReceiverURL != srv.URL || rows[0].State != "ready" {
		t.Fatalf("row = %#v, want %s ready", rows[0], srv.URL)
	}
}

// TestConnectionFailureReasonClassification pins the operator-facing reason each
// failure shape maps to. Several of these are reachable-but-unusable, which is a
// different problem from an unreachable receiver and must not be reported as one.
func TestConnectionFailureReasonClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"dns", &net.DNSError{Err: "no such host", IsNotFound: true}, mxdial.ReasonDNSFailure},
		{"timeout", context.DeadlineExceeded, mxdial.ReasonUnreachable},
		{"refused destination", errors.New("destination is not public-routable"), mxdial.ReasonDestinationNotAllowed},
		{"refused bearer", mxdial.ErrSessionRejected, mxdial.ReasonRejected},
		{"tls", errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority"), mxdial.ReasonTLSCertificate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mxdial.ConnectionFailureReason(tc.err); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// reserveLoopbackAddr returns a loopback host:port that was bindable a moment
// ago, and is therefore very likely to refuse a connection now.
func reserveLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind a loopback port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}
