package mxagent_test

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"gatehouse-mail/internal/mxagent"
)

func TestFromHeaderDomain(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"From: Sender <sender@outside.test>\r\nTo: x@y\r\n\r\nbody", "outside.test"},
		{"From: sender@example.com\r\n\r\nbody", "example.com"},
		{"From: \"Weird, Name\" <a@sub.example.com>\r\n\r\nbody", "sub.example.com"},
		{"To: x@y\r\n\r\nno from", ""},
	}
	for _, tc := range cases {
		if got := mxagent.FromHeaderDomain([]byte(tc.raw)); got != tc.want {
			t.Fatalf("FromHeaderDomain(%q)=%q want %q", tc.raw, got, tc.want)
		}
	}
}

func TestDomainsAlign(t *testing.T) {
	cases := []struct {
		from, checked string
		strict        bool
		want          bool
	}{
		{"example.com", "example.com", true, true},
		{"example.com", "mail.example.com", true, false},
		{"example.com", "mail.example.com", false, true},
		{"example.com", "other.com", false, false},
		{"a.b.co.uk", "x.b.co.uk", false, true},
		{"a.b.co.uk", "x.c.co.uk", false, false},
	}
	for _, tc := range cases {
		if got := mxagent.DomainsAlign(tc.from, tc.checked, tc.strict); got != tc.want {
			t.Fatalf("DomainsAlign(%q,%q,%v)=%v want %v", tc.from, tc.checked, tc.strict, got, tc.want)
		}
	}
}

func TestOrganizationalDomain(t *testing.T) {
	cases := map[string]string{
		"example.com":       "example.com",
		"mail.example.com":  "example.com",
		"a.b.co.uk":         "b.co.uk",
		"sub.example.co.uk": "example.co.uk",
	}
	for in, want := range cases {
		if got := mxagent.OrganizationalDomain(in); got != want {
			t.Fatalf("OrganizationalDomain(%q)=%q want %q", in, got, want)
		}
	}
}

func TestStageMessageBounds(t *testing.T) {
	raw := strings.Repeat("A", 1000)
	b, err := mxagent.StageMessage(strings.NewReader(raw), t.TempDir(), 2000, 5_000_000_000)
	if err != nil || string(b) != raw {
		t.Fatalf("stage ok: err=%v len=%d", err, len(b))
	}
	if _, err := mxagent.StageMessage(strings.NewReader(raw), t.TempDir(), 100, 5_000_000_000); err != mxagent.ErrTooLarge {
		t.Fatalf("expected too large, got %v", err)
	}
	if _, err := mxagent.StageMessage(bytes.NewReader(nil), t.TempDir(), 100, 5_000_000_000); err == nil {
		t.Fatal("expected empty error")
	}
}

func TestPeerIP(t *testing.T) {
	if got := mxagent.PeerIP(&net.TCPAddr{IP: net.ParseIP("203.0.113.5"), Port: 1234}); got == nil || got.String() != "203.0.113.5" {
		t.Fatalf("peerIP=%v", got)
	}
	if got := mxagent.PeerIP(nil); got != nil {
		t.Fatalf("nil addr should give nil, got %v", got)
	}
}
