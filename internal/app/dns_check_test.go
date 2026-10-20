package app

// This suite lives in package app rather than under tests/unit because CheckMX
// and CheckTXT are the published-record checks whose rules decide what the
// dashboard light and the status dialog assert. It uses no database or server.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net"
	"reflect"
	"testing"

	"github.com/dellarb/mailmoose/internal/mxwire"
)

// staticResolver serves fixed MX/TXT answers for the checker.
type staticResolver struct {
	mx   []*net.MX
	mxE  error
	txt  []string
	txtE error
}

func (s staticResolver) LookupMX(context.Context, string) ([]*net.MX, error) { return s.mx, s.mxE }
func (s staticResolver) LookupTXT(context.Context, string) ([]string, error) {
	return s.txt, s.txtE
}

// recordingResolver captures the name a lookup was asked for, so the checks'
// absolute-name rule is observable. The lookup answers are empty, which is all
// the name tests need.
type recordingResolver struct {
	mx  func(name string)
	txt func(name string)
}

func (r recordingResolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	if r.mx != nil {
		r.mx(name)
	}
	return nil, &net.DNSError{IsNotFound: true, Name: name}
}

func (r recordingResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if r.txt != nil {
		r.txt(name)
	}
	return nil, &net.DNSError{IsNotFound: true, Name: name}
}

func TestCheckMXReportsUnexpectedHosts(t *testing.T) {
	expected := []string{"antler1.example.test", "antler2.example.test"}
	for _, tc := range []struct {
		name      string
		published []*net.MX
		wantState string
		wantMatch []string
		wantExtra []string
	}{
		{
			name:      "only our receivers",
			published: []*net.MX{{Host: "antler1.example.test.", Pref: 10}, {Host: "antler2.example.test.", Pref: 20}},
			wantState: "ok",
			wantMatch: []string{"antler1.example.test", "antler2.example.test"},
		},
		{
			name:      "one receiver plus a rogue",
			published: []*net.MX{{Host: "antler1.example.test.", Pref: 10}, {Host: "old.example.test.", Pref: 20}},
			wantState: "ok",
			wantMatch: []string{"antler1.example.test"},
			wantExtra: []string{"old.example.test"},
		},
		{
			name:      "no receiver published is a mismatch",
			published: []*net.MX{{Host: "old.example.test.", Pref: 10}},
			wantState: "mismatch",
			wantExtra: []string{"old.example.test"},
		},
		{
			name:      "case and trailing dot are normalized",
			published: []*net.MX{{Host: "ANTLER1.Example.Test.", Pref: 10}, {Host: "Old.Example.Test.", Pref: 20}},
			wantState: "ok",
			wantMatch: []string{"antler1.example.test"},
			wantExtra: []string{"old.example.test"},
		},
		{
			name:      "nothing published is not found",
			published: nil,
			wantState: StateNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := CheckMX(context.Background(), staticResolver{mx: tc.published}, "example.test", expected)
			if view.State != tc.wantState {
				t.Fatalf("state = %q, want %q", view.State, tc.wantState)
			}
			if len(tc.wantMatch) > 0 && !reflect.DeepEqual(view.Matched, tc.wantMatch) {
				t.Fatalf("matched = %v, want %v", view.Matched, tc.wantMatch)
			}
			if len(tc.wantExtra) > 0 && !reflect.DeepEqual(view.Unexpected, tc.wantExtra) {
				t.Fatalf("unexpected = %v, want %v", view.Unexpected, tc.wantExtra)
			}
			if len(tc.wantExtra) == 0 && len(view.Unexpected) != 0 {
				t.Fatalf("unexpected = %v, want none", view.Unexpected)
			}
		})
	}
}

// TestCheckMXAbsentIsNotFoundNotDNSError proves a definitive "no such records"
// answer reads as not_found (publish one), while a lookup that could not be
// completed reads as dns_error. Presenting the second as the first would tell an
// operator to create a record they may already have published.
func TestCheckMXAbsentIsNotFoundNotDNSError(t *testing.T) {
	expected := []string{"antler1.example.test"}

	notFound := CheckMX(context.Background(), staticResolver{mxE: &net.DNSError{IsNotFound: true, Name: "example.test"}}, "example.test", expected)
	if notFound.State != StateNotFound {
		t.Fatalf("NXDOMAIN state = %q, want not_found", notFound.State)
	}

	servfail := CheckMX(context.Background(), staticResolver{mxE: errors.New("SERVFAIL")}, "example.test", expected)
	if servfail.State != StateDNSError {
		t.Fatalf("SERVFAIL state = %q, want dns_error", servfail.State)
	}
	if len(servfail.Found) != 0 || len(servfail.Matched) != 0 {
		t.Fatalf("a dns_error check must claim nothing: %+v", servfail)
	}
}

// TestCheckMXQueriesAbsoluteName proves the core's MX check asks for a fully
// qualified name. A relative name is expanded through the resolver's search
// list, and a search-suffix failure then masks a clean NODATA as IsTemporary —
// which turns "nothing is published" into dns_error instead of not_found, the
// exact "a brand new domain's check never settles" symptom. The receiver's own
// proofs query absolute names for the same reason.
func TestCheckMXQueriesAbsoluteName(t *testing.T) {
	var got string
	res := recordingResolver{mx: func(name string) { got = name }}
	CheckMX(context.Background(), res, "example.test", []string{"antler1.example.test"})
	if want := "example.test."; got != want {
		t.Fatalf("MX lookup queried %q, want %q", got, want)
	}
}

// TestCheckTXTQueriesAbsoluteName proves the same for the TXT check: the
// _mailmoose-mx name must be qualified or a search-suffix failure can mask its
// absent answer as dns_error.
func TestCheckTXTQueriesAbsoluteName(t *testing.T) {
	var got string
	res := recordingResolver{txt: func(name string) { got = name }}
	CheckTXT(context.Background(), res, "example.test", "id", []byte("x"), "v=MM1")
	if want := "_mailmoose-mx.example.test."; got != want {
		t.Fatalf("TXT lookup queried %q, want %q", got, want)
	}
}

// TestCheckTXTAgreesWithReceiverParser proves the local check uses the receiver's
// own MM1 rule. The old hand-written parser accepted records the receiver
// rejects, so the dialog could show a green "matching" record next to a receiver
// refusing with "key unavailable".
func TestCheckTXTAgreesWithReceiverParser(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = priv
	keyID := "test-key-id"
	expected := mxwire.DomainTXT(keyID, pub)

	// A malformed MM1 record that the old parser skipped: it has an unknown
	// field, so mxwire.ParseDomainTXT rejects the whole set.
	withExtraField := expected + "; extra=1"
	// A duplicate MM1 record for the same key: ambiguous, so it fails closed.
	duplicate := []string{expected, expected}

	for _, tc := range []struct {
		name    string
		records []string
		want    string
	}{
		{"the exact record matches", []string{expected}, StateOK},
		{"an extra field is rejected like the receiver", []string{withExtraField}, StateMismatch},
		{"a duplicate MM1 record is ambiguous", duplicate, StateMismatch},
		{"a different key id is rejected", []string{"v=MM1; k=ed25519; id=other; p=" + base64.StdEncoding.EncodeToString(pub)}, StateMismatch},
		{"a non-MM1 record is rejected", []string{"v=spf1 -all"}, StateMismatch},
		{"a broken public key is rejected", []string{"v=MM1; k=ed25519; id=" + keyID + "; p=not-base64"}, StateMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := CheckTXT(context.Background(), staticResolver{txt: tc.records}, "example.test", keyID, pub, expected)
			if view.State != tc.want {
				t.Fatalf("state = %q, want %q (%+v)", view.State, tc.want, view)
			}
		})
	}

	// An absent record and an unanswered lookup are different facts.
	absent := CheckTXT(context.Background(), staticResolver{txtE: &net.DNSError{IsNotFound: true}}, "example.test", keyID, pub, expected)
	if absent.State != StateNotFound {
		t.Fatalf("NXDOMAIN state = %q, want not_found", absent.State)
	}
	unavailable := CheckTXT(context.Background(), staticResolver{txtE: errors.New("timeout")}, "example.test", keyID, pub, expected)
	if unavailable.State != StateDNSError {
		t.Fatalf("timeout state = %q, want dns_error", unavailable.State)
	}
}

// TestCheckDKIM proves the Direct MX sending-side record check: green when the
// published p= value equals the signing key, not_found when nothing usable is
// published, red (mismatch) when a different key is published, and dns_error
// when the lookup cannot be completed.
func TestCheckDKIM(t *testing.T) {
	const name = "mailmoose._domainkey.example.test"
	const want = "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8A"
	for _, tc := range []struct {
		name    string
		records []string
		want    string
	}{
		{"the exact record matches", []string{"v=DKIM1; k=rsa; p=" + want}, StateOK},
		{"tags may be reordered", []string{"k=rsa; p=" + want + "; v=DKIM1"}, StateOK},
		{"whitespace inside the key is ignored", []string{"v=DKIM1; k=rsa; p=" + want[:10] + " " + want[10:]}, StateOK},
		{"a different key is a mismatch", []string{"v=DKIM1; k=rsa; p=DIFFERENTKEY"}, StateMismatch},
		{"a record with no p= is not found", []string{"v=DKIM1; k=rsa"}, StateNotFound},
		{"an unrelated TXT record is not found", []string{"v=spf1 -all"}, StateNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := CheckDKIM(context.Background(), staticResolver{txt: tc.records}, name, "v=DKIM1; k=rsa; p="+want, want)
			if view.State != tc.want {
				t.Fatalf("state = %q, want %q (%+v)", view.State, tc.want, view)
			}
			if view.Kind != "dkim" {
				t.Fatalf("kind = %q, want dkim", view.Kind)
			}
		})
	}

	// An absent record and an unanswered lookup are different facts.
	absent := CheckDKIM(context.Background(), staticResolver{txtE: &net.DNSError{IsNotFound: true}}, name, "", want)
	if absent.State != StateNotFound {
		t.Fatalf("NXDOMAIN state = %q, want not_found", absent.State)
	}
	unavailable := CheckDKIM(context.Background(), staticResolver{txtE: errors.New("timeout")}, name, "", want)
	if unavailable.State != StateDNSError {
		t.Fatalf("timeout state = %q, want dns_error", unavailable.State)
	}
}
