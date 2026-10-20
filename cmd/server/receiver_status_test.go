package main

import (
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// TestReceiverDetailNamesSettledFailures pins the operator-facing phrase for each
// dialer failure token, so the installation Direct MX panel and the account
// Remote MX panel describe the same receiver the same way.
func TestReceiverDetailNamesSettledFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		reason string
		want   string
	}{
		{"refused session", mxdial.ReasonRejected, "receiver refused the session"},
		{"http/1.1", mxdial.ReasonHTTP2Required, "receiver answered HTTP/1.1 where HTTP/2 is required"},
		{"mode mismatch", mxdial.ReasonModeMismatch, "receiver answered the wrong session mode"},
		{"protocol mismatch", mxdial.ReasonProtocolMismatch, "receiver protocol mismatch"},
		{"dns", mxdial.ReasonDNSFailure, "receiver hostname does not resolve"},
		{"tls", mxdial.ReasonTLSCertificate, "receiver TLS certificate could not be verified"},
		{"blocked destination", mxdial.ReasonDestinationNotAllowed, "receiver destination is not allowed"},
		{"unknown", "something_else", "receiver unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := receiverDetail(tc.reason); got != tc.want {
				t.Fatalf("detail = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestApplyConnectionStatusReportsSettledFailures proves a receiver that answered
// and refused — or could not be reached, or was blocked before the dial — is
// unavailable rather than a connection still in progress, while a session that
// was reached and is being retried stays connecting.
func TestApplyConnectionStatusReportsSettledFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		live       mxdial.Status
		wantState  string
		wantDetail string
	}{
		{"ready", mxdial.Status{State: mxdial.StatusActive, SMTPHostname: "mx.example.test"}, app.MXStateActive, ""},
		{"authorizing", mxdial.Status{State: mxdial.StatusConnecting}, app.MXStateConnecting, "connecting to receiver"},
		{"retrying", mxdial.Status{State: mxdial.StatusDisconnected, Reason: mxdial.ReasonReceiverError}, app.MXStateConnecting, mxdial.ReasonReceiverError},
		{"refused", mxdial.Status{State: mxdial.StatusRejected, Reason: mxdial.ReasonRejected}, app.MXStateUnavailable, "receiver refused the session"},
		{"unreachable", mxdial.Status{State: mxdial.StatusUnreachable, Reason: mxdial.ReasonDNSFailure}, app.MXStateUnavailable, "receiver hostname does not resolve"},
		{"blocked", mxdial.Status{State: mxdial.StatusUnavailable, Reason: mxdial.ReasonDestinationNotAllowed}, app.MXStateUnavailable, "receiver destination is not allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := &mxRuntime{}
			rt.applyConnectionStatus(tc.live)
			if rt.state != tc.wantState {
				t.Fatalf("state = %q, want %q", rt.state, tc.wantState)
			}
			if rt.detail != tc.wantDetail {
				t.Fatalf("detail = %q, want %q", rt.detail, tc.wantDetail)
			}
		})
	}
}

// TestRemoteMXStateReportsSettledFailuresAsUnavailable proves the account Remote
// MX status names a settled failure instead of reporting every non-ready
// receiver as still connecting.
func TestRemoteMXStateReportsSettledFailuresAsUnavailable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		live       mxdial.Status
		wantState  string
		wantDetail string
	}{
		{"ready", mxdial.Status{State: mxdial.StatusActive}, app.MXStateActive, ""},
		{"authorizing", mxdial.Status{State: mxdial.StatusConnecting}, app.MXStateConnecting, ""},
		{"idle timeout", mxdial.Status{State: mxdial.StatusDisconnected, Reason: mxdial.ReasonIdleTimeout}, app.MXStateConnecting, mxdial.ReasonIdleTimeout},
		{"refused", mxdial.Status{State: mxdial.StatusRejected, Reason: mxdial.ReasonRejected}, app.MXStateUnavailable, "receiver refused the session"},
		{"protocol mismatch", mxdial.Status{State: mxdial.StatusRejected, Reason: mxdial.ReasonProtocolMismatch}, app.MXStateUnavailable, "receiver protocol mismatch"},
		{"unreachable", mxdial.Status{State: mxdial.StatusUnreachable, Reason: mxdial.ReasonTLSCertificate}, app.MXStateUnavailable, "receiver TLS certificate could not be verified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, detail := remoteMXState(tc.live)
			if state != tc.wantState {
				t.Fatalf("state = %q, want %q", state, tc.wantState)
			}
			if detail != tc.wantDetail {
				t.Fatalf("detail = %q, want %q", detail, tc.wantDetail)
			}
		})
	}
}
