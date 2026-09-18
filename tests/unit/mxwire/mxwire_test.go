package mxwire_test

import (
	"bytes"
	"io"
	"testing"
	"time"

	"gatehouse-mail/internal/mxwire"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	key := []byte("s3cret")
	meta := []byte(`{"version":"mx-v1","key_id":"edge"}`)
	body := []byte("From: a@b\r\n\r\nhi")
	md, bd := mxwire.MetaDigest(meta), mxwire.BodyDigest(body)
	sig := mxwire.Sign(key, mxwire.ProtocolVersion, "edge", 1000, "req1", "POST", mxwire.PathIngest, md, bd)
	if err := mxwire.Verify(key, mxwire.ProtocolVersion, "edge", 1000, "req1", "POST", mxwire.PathIngest, md, bd, sig, time.Unix(1000, 0), time.Minute); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	// Tampered body.
	if err := mxwire.Verify(key, mxwire.ProtocolVersion, "edge", 1000, "req1", "POST", mxwire.PathIngest, md, mxwire.BodyDigest([]byte("tampered")), sig, time.Unix(1000, 0), time.Minute); err == nil {
		t.Fatal("tampered body accepted")
	}
	// Wrong key id.
	if err := mxwire.Verify(key, mxwire.ProtocolVersion, "other", 1000, "req1", "POST", mxwire.PathIngest, md, bd, sig, time.Unix(1000, 0), time.Minute); err == nil {
		t.Fatal("wrong key id accepted")
	}
	// Skew.
	if err := mxwire.Verify(key, mxwire.ProtocolVersion, "edge", 1000, "req1", "POST", mxwire.PathIngest, md, bd, sig, time.Unix(100000, 0), time.Minute); err == nil {
		t.Fatal("stale timestamp accepted")
	}
	// Wrong version.
	if err := mxwire.Verify(key, "mx-v2", "edge", 1000, "req1", "POST", mxwire.PathIngest, md, bd, sig, time.Unix(1000, 0), time.Minute); err == nil {
		t.Fatal("unknown version accepted")
	}
}

func TestPreludeRoundTrip(t *testing.T) {
	meta := []byte(`{"version":"mx-v1","recipients":["a@b"]}`)
	body := []byte("From: a@b\r\n\r\nhi")
	var buf bytes.Buffer
	if err := mxwire.WritePrelude(&buf, meta); err != nil {
		t.Fatal(err)
	}
	buf.Write(body)
	gotMeta, err := mxwire.ReadPrelude(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotMeta, meta) {
		t.Fatalf("meta=%q", gotMeta)
	}
	rest, _ := io.ReadAll(&buf)
	if !bytes.Equal(rest, body) {
		t.Fatalf("body=%q", rest)
	}
	// SplitPreludeBytes agrees on the framed bytes.
	var framed bytes.Buffer
	if err := mxwire.WritePrelude(&framed, meta); err != nil {
		t.Fatal(err)
	}
	framed.Write(body)
	sm, sb, err := mxwire.SplitPreludeBytes(framed.Bytes())
	if err != nil || !bytes.Equal(sm, meta) || !bytes.Equal(sb, body) {
		t.Fatalf("split err=%v sm=%q sb=%q", err, sm, sb)
	}
	if _, _, err := mxwire.SplitPreludeBytes([]byte{0}); err == nil {
		t.Fatal("short prelude accepted")
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

func TestCheckEdgeSecret(t *testing.T) {
	strong := []string{
		// 32 random bytes as hex (openssl rand -hex 32).
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		// 32 random bytes as base64.
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		// 32 raw bytes.
		"01234567890123456789012345678901",
	}
	for _, s := range strong {
		if err := mxwire.CheckEdgeSecret(s); err != nil {
			t.Fatalf("strong secret rejected: %v", err)
		}
	}
	weak := []string{
		"",
		"secret",
		"correct horse battery staple",
		"0123456789abcdef",
		"AAAAAAAAAAAAAAAAAAAAAA==",
	}
	for _, s := range weak {
		if err := mxwire.CheckEdgeSecret(s); err == nil {
			t.Fatalf("weak secret %q accepted", s)
		}
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
