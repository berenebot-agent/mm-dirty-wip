package timezone_test

import (
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/timezone"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"", false},
		{"UTC", false},
		{"America/New_York", false},
		{"Europe/London", false},
		{"Asia/Kolkata", false},
		{"Not/AZone", true},
		{"Mars/Olympus", true},
		{"  ", true},
	}
	for _, tc := range cases {
		err := timezone.Validate(tc.in)
		if tc.wantErr && err == nil {
			t.Errorf("Validate(%q) = nil, want error", tc.in)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("Validate(%q) unexpected error: %v", tc.in, err)
		}
	}
}

func TestResolve(t *testing.T) {
	ny := timezone.Resolve("America/New_York", "")
	if ny.String() != "America/New_York" {
		t.Fatalf("user override not preferred: %s", ny)
	}
	// User unset falls back to account.
	london := timezone.Resolve("", "Europe/London")
	if london.String() != "Europe/London" {
		t.Fatalf("account default not used: %s", london)
	}
	// User override wins over account.
	tokyo := timezone.Resolve("Asia/Tokyo", "Europe/London")
	if tokyo.String() != "Asia/Tokyo" {
		t.Fatalf("user did not win: %s", tokyo)
	}
	// Both unset -> UTC.
	if got := timezone.Resolve("", ""); got != time.UTC {
		t.Fatalf("empty resolve = %s, want UTC", got)
	}
	// An unknown name is ignored, falling through to the next valid value.
	got := timezone.Resolve("Not/AZone", "Europe/London")
	if got.String() != "Europe/London" {
		t.Fatalf("stale name not skipped: %s", got)
	}
	// Both unknown -> UTC.
	if got := timezone.Resolve("Not/AZone", "Also/Bad"); got != time.UTC {
		t.Fatalf("unknown names = %s, want UTC", got)
	}
}

// TestOptionsAllResolve guards the generated name list: every selectable zone
// must actually load, and UTC must be present.
func TestOptionsAllResolve(t *testing.T) {
	opts := timezone.Options()
	if len(opts) < 2 {
		t.Fatalf("options too small: %d", len(opts))
	}
	if opts[0] != timezone.Default {
		t.Fatalf("first option = %q, want %q", opts[0], timezone.Default)
	}
	seenUTC := false
	for _, name := range opts {
		if name == timezone.Default {
			seenUTC = true
		}
		if _, err := time.LoadLocation(name); err != nil {
			t.Errorf("option %q does not load: %v", name, err)
		}
	}
	if !seenUTC {
		t.Fatal("UTC missing from options")
	}
}
