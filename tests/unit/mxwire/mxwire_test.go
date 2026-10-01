package mxwire_test

import (
	"testing"

	"github.com/dellarb/mailmoose/internal/mxwire"
)

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
