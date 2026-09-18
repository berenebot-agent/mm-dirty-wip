package mxagent_test

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/mxagent"
	"github.com/dellarb/mailmoose/internal/mxwire"
)

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
		// Private/hosted suffixes from the full PSL must not collapse two
		// tenants of the same platform into one organization.
		"alice.github.io": "alice.github.io",
		"bob.github.io":   "bob.github.io",
		"a.herokuapp.com": "a.herokuapp.com",
		// A bare public suffix (or a lone label) has no registrable domain and
		// is returned unchanged.
		"co.uk":     "co.uk",
		"localhost": "localhost",
	}
	for in, want := range cases {
		if got := mxagent.OrganizationalDomain(in); got != want {
			t.Fatalf("OrganizationalDomain(%q)=%q want %q", in, got, want)
		}
	}
	if mxagent.DomainsAlign("alice.github.io", "bob.github.io", false) {
		t.Fatal("relaxed alignment must not treat two github.io tenants as one organization")
	}
}

func TestFromHeaderDomain(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"From: Sender <sender@outside.test>\r\nTo: x@y\r\n\r\nbody", "outside.test"},
		{"From: sender@example.com\r\n\r\nbody", "example.com"},
		{"From: \"Weird, Name\" <a@sub.example.com>\r\n\r\nbody", "sub.example.com"},
		{"To: x@y\r\n\r\nno from", ""},
		// RFC 5322 comment must not be mistaken for the address.
		{"From: attacker@evil.com (ceo@bank.com)\r\n\r\nbody", "evil.com"},
		// Folded From header: address on the continuation line.
		{"From: Sender <\r\n\tsender@outside.test>\r\n\r\nbody", "outside.test"},
		// Angle address with a comment inside the display name.
		{"From: \"a (b)\" <real@domain.test>\r\n\r\nbody", "domain.test"},
	}
	for _, tc := range cases {
		if got := mxagent.FromHeaderDomain([]byte(tc.raw)); got != tc.want {
			t.Fatalf("FromHeaderDomain(%q)=%q want %q", tc.raw, got, tc.want)
		}
	}
}

func TestStageMessageBounds(t *testing.T) {
	raw := strings.Repeat("A", 1000)
	buf, size, digest, err := mxagent.StageMessage(strings.NewReader(raw), 2000, 5_000_000_000)
	if err != nil {
		t.Fatalf("stage err=%v", err)
	}
	if string(buf) != raw {
		t.Fatalf("staged bytes differ")
	}
	if size != int64(len(raw)) {
		t.Fatalf("stage size=%d want %d", size, len(raw))
	}
	if digest != mxwire.BodyDigest([]byte(raw)) {
		t.Fatalf("stage digest %q", digest)
	}
	if _, _, _, err := mxagent.StageMessage(strings.NewReader(raw), 100, 5_000_000_000); err != mxagent.ErrTooLarge {
		t.Fatalf("expected too large, got %v", err)
	}
	if _, _, _, err := mxagent.StageMessage(bytes.NewReader(nil), 100, 5_000_000_000); err == nil {
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
