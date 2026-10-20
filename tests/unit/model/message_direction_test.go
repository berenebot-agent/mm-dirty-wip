package model_test

import (
	"reflect"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
)

func TestMessageDirectionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, recorded     string
		sent, inbox, draft bool
		want               string
	}{
		{"recorded inbound wins", "inbound", true, false, false, "inbound"},
		{"recorded outbound wins", "outbound", false, true, true, "outbound"},
		{"sent evidence", "", true, false, false, "outbound"},
		{"sent to self", "", true, true, false, "outbound"},
		{"inbox", "", false, true, false, "inbound"},
		{"draft", "", true, true, true, "unknown"},
		{"no evidence", "", false, false, false, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := model.MessageDirection(tc.recorded, tc.sent, tc.inbox, tc.draft); got != tc.want {
				t.Fatalf("direction=%s want=%s", got, tc.want)
			}
		})
	}
	for direction, want := range map[string][]string{"inbound": {"sender@example.test"}, "outbound": {"recipient@example.test"}, "unknown": nil, "": nil} {
		m := model.Message{Direction: direction, From: model.Address{Address: "sender@example.test"}, To: []string{"recipient@example.test"}}
		if got := model.DefaultReplyRecipients(m); !reflect.DeepEqual(got, want) {
			t.Errorf("%s recipients=%v want=%v", direction, got, want)
		}
	}
}
