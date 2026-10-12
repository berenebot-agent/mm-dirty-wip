package app

// This suite lives in package app rather than under tests/unit because CheckMX
// is the published-record check whose "unexpected host" rule (a published MX
// hostname that is not one of our receivers) is exactly the extra-MX signal the
// dashboard light turns amber on. It uses no database or server.

import (
	"context"
	"net"
	"reflect"
	"testing"
)

// staticResolver serves fixed MX answers for the checker.
type staticResolver struct {
	mx  []*net.MX
	err error
}

func (s staticResolver) LookupMX(context.Context, string) ([]*net.MX, error) { return s.mx, s.err }
func (s staticResolver) LookupTXT(context.Context, string) ([]string, error) {
	return nil, nil
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
			name:      "nothing published is pending",
			published: nil,
			wantState: "pending",
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
