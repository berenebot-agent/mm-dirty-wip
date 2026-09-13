package mxwire_test

import (
	"testing"
	"time"

	"gatehouse-mail/internal/mxwire"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	key := []byte("s3cret")
	meta := []byte(`{"version":"mx-v1","key_id":"edge"}`)
	body := []byte("From: a@b\r\n\r\nhi")
	sig := mxwire.Sign(key, mxwire.ProtocolVersion, "edge", 1000, "req1", "POST", mxwire.PathIngest, meta, body)
	if err := mxwire.Verify(key, mxwire.ProtocolVersion, "edge", "edge", 1000, "req1", "POST", mxwire.PathIngest, meta, body, sig, time.Unix(1000, 0), time.Minute); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	// Tampered body.
	if err := mxwire.Verify(key, mxwire.ProtocolVersion, "edge", "edge", 1000, "req1", "POST", mxwire.PathIngest, meta, []byte("tampered"), sig, time.Unix(1000, 0), time.Minute); err == nil {
		t.Fatal("tampered body accepted")
	}
	// Wrong key id.
	if err := mxwire.Verify(key, mxwire.ProtocolVersion, "other", "edge", 1000, "req1", "POST", mxwire.PathIngest, meta, body, sig, time.Unix(1000, 0), time.Minute); err == nil {
		t.Fatal("wrong key id accepted")
	}
	// Skew.
	if err := mxwire.Verify(key, mxwire.ProtocolVersion, "edge", "edge", 1000, "req1", "POST", mxwire.PathIngest, meta, body, sig, time.Unix(100000, 0), time.Minute); err == nil {
		t.Fatal("stale timestamp accepted")
	}
	// Wrong version.
	if err := mxwire.Verify(key, "mx-v2", "edge", "edge", 1000, "req1", "POST", mxwire.PathIngest, meta, body, sig, time.Unix(1000, 0), time.Minute); err == nil {
		t.Fatal("unknown version accepted")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		mode mxwire.Enforcement
		auth mxwire.AuthResults
		spam bool
	}{
		{"none", mxwire.EnforcementModerate, mxwire.AuthResults{}, false},
		{"moderate spf only", mxwire.EnforcementModerate, mxwire.AuthResults{SPF: &mxwire.SPFEvidence{Result: "fail"}}, false},
		{"moderate dkim only", mxwire.EnforcementModerate, mxwire.AuthResults{DKIM: []mxwire.DKIMEvidence{{Result: "fail"}}}, false},
		{"moderate both", mxwire.EnforcementModerate, mxwire.AuthResults{SPF: &mxwire.SPFEvidence{Result: "fail"}, DKIM: []mxwire.DKIMEvidence{{Result: "fail"}}}, true},
		{"moderate dmarc", mxwire.EnforcementModerate, mxwire.AuthResults{DMARC: &mxwire.DMARCEvidence{Result: "fail"}}, true},
		{"hard spf", mxwire.EnforcementHard, mxwire.AuthResults{SPF: &mxwire.SPFEvidence{Result: "fail"}}, true},
		{"hard dkim", mxwire.EnforcementHard, mxwire.AuthResults{DKIM: []mxwire.DKIMEvidence{{Result: "fail"}}}, true},
		{"softfail never", mxwire.EnforcementHard, mxwire.AuthResults{SPF: &mxwire.SPFEvidence{Result: "softfail"}}, false},
		{"temperror never", mxwire.EnforcementHard, mxwire.AuthResults{SPF: &mxwire.SPFEvidence{Result: "temperror"}}, false},
		{"mixed dkim indeterminate", mxwire.EnforcementModerate, mxwire.AuthResults{DKIM: []mxwire.DKIMEvidence{{Result: "fail"}, {Result: "temperror"}}}, false},
		{"one pass clears dkim", mxwire.EnforcementHard, mxwire.AuthResults{DKIM: []mxwire.DKIMEvidence{{Result: "fail"}, {Result: "pass"}}}, false},
		{"dmarc pass not none", mxwire.EnforcementModerate, mxwire.AuthResults{DMARC: &mxwire.DMARCEvidence{Result: "none", Policy: "none"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mxwire.Classify(&tc.auth, tc.mode)
			if got.Spam != tc.spam {
				t.Fatalf("spam=%v want %v (%s)", got.Spam, tc.spam, got.Reason)
			}
		})
	}
}

func TestDeliveryFingerprintStable(t *testing.T) {
	a := mxwire.DeliveryFingerprint("a@x", "b@y", "digest")
	b := mxwire.DeliveryFingerprint("A@X", "B@Y", "digest")
	if a != b {
		t.Fatal("fingerprint should be case-insensitive on addresses")
	}
	if a == mxwire.DeliveryFingerprint("a@x", "b@y", "other") {
		t.Fatal("fingerprint should change with content")
	}
}
