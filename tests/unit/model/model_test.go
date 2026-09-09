package model

import "testing"

func TestNormalizeAllowedSender(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"Alice@Example.com", "alice@example.com", false},
		{"  alice@example.com  ", "alice@example.com", false},
		{"*@Example.com", "*@example.com", false},
		{"*@*.Example.com", "*@*.example.com", false},
		{"", "", false},
		{"   ", "", false},
		{"not an email", "", true},
		{"alice@*", "", true},
		{"*@*", "", true},
		{"*@domain.*", "", true},
		{"*@*.", "", true},
		{"*@*.*", "", true},
		{"a*b@example.com", "", true},
		{"alice@*.com", "", true},
		{"**@example.com", "", true},
	}
	for _, tc := range cases {
		got, err := NormalizeAllowedSender(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("NormalizeAllowedSender(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeAllowedSender(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeAllowedSender(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMatchAllowedSender(t *testing.T) {
	cases := []struct {
		pattern string
		address string
		want    bool
	}{
		{"alice@example.com", "Alice@Example.com", true},
		{"alice@example.com", "bob@example.com", false},
		{"*@example.com", "anything@example.com", true},
		{"*@example.com", "anything+tag@example.com", true},
		{"*@example.com", "a@sub.example.com", false},
		{"*@example.com", "a@example.com.evil.com", false},
		{"*@*.example.com", "a@sub.example.com", true},
		{"*@*.example.com", "a@deep.sub.example.com", true},
		{"*@*.example.com", "a@example.com", false},
		{"*@*.example.com", "a@evil-example.com", false},
		{"*@*.example.com", "a@example.com.evil.com", false},
		{"", "a@example.com", false},
		{"*@example.com", "", false},
	}
	for _, tc := range cases {
		if got := MatchAllowedSender(tc.pattern, tc.address); got != tc.want {
			t.Errorf("MatchAllowedSender(%q, %q) = %v, want %v", tc.pattern, tc.address, got, tc.want)
		}
	}
}

func TestInboxAllowsSender(t *testing.T) {
	if !(Inbox{}).AllowsSender("anyone@example.com") {
		t.Fatal("empty allowlist must allow all senders")
	}
	box := Inbox{AllowedSenders: []string{"alice@example.com", "*@allowed.test", "*@*.corp.test"}}
	cases := []struct {
		address string
		want    bool
	}{
		{"alice@example.com", true},
		{"bob@example.com", false},
		{"anyone@allowed.test", true},
		{"anyone@sub.corp.test", true},
		{"anyone@corp.test", false},
		{"anyone@other.test", false},
	}
	for _, tc := range cases {
		if got := box.AllowsSender(tc.address); got != tc.want {
			t.Errorf("AllowsSender(%q) = %v, want %v", tc.address, got, tc.want)
		}
	}
}
