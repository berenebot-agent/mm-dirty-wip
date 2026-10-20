// These tests live in package app rather than under tests/unit because
// mxAuthenticated is unexported and is a pure decision function over edge
// evidence; the black-box tests/unit convention cannot reach it, and wiring a
// full staged ingest for each alignment permutation would obscure the cases
// under test. This mirrors the documented in-package exception for
// cmd/server/mxruntime_test.go.
package app

import (
	"testing"

	"github.com/dellarb/mailmoose/internal/mxwire"
)

func TestMXAuthenticated(t *testing.T) {
	cases := []struct {
		name string
		in   mxwire.AuthResults
		want bool
	}{
		{
			name: "dmarc pass authenticates",
			in:   mxwire.AuthResults{DMARC: &mxwire.DMARCEvidence{Result: "pass"}},
			want: true,
		},
		{
			name: "dmarc fail rejects even with relaxed dkim aligned",
			in: mxwire.AuthResults{
				DMARC: &mxwire.DMARCEvidence{Result: "fail"},
				DKIM:  []mxwire.DKIMEvidence{{Result: "pass", Domain: "example.com", Aligned: true}},
			},
			want: false,
		},
		{
			name: "no policy: aligned spf pass authenticates",
			in: mxwire.AuthResults{
				DMARC: &mxwire.DMARCEvidence{Result: "none"},
				SPF:   &mxwire.SPFEvidence{Result: "pass", Domain: "example.com", Aligned: true},
			},
			want: true,
		},
		{
			name: "no policy: unaligned spf pass does not authenticate",
			in: mxwire.AuthResults{
				DMARC: &mxwire.DMARCEvidence{Result: "none"},
				SPF:   &mxwire.SPFEvidence{Result: "pass", Domain: "lookalike.test", Aligned: false},
			},
			want: false,
		},
		{
			name: "no policy: aligned dkim pass authenticates",
			in: mxwire.AuthResults{
				DMARC: &mxwire.DMARCEvidence{Result: "none"},
				DKIM:  []mxwire.DKIMEvidence{{Result: "pass", Domain: "example.com", Aligned: true}},
			},
			want: true,
		},
		{
			name: "no evidence does not authenticate",
			in:   mxwire.AuthResults{},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mxAuthenticated(tc.in); got != tc.want {
				t.Fatalf("mxAuthenticated = %v, want %v", got, tc.want)
			}
		})
	}
}
