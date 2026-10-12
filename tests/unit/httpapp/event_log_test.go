package httpapp_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/httpapp"
)

// eventLogRecords returns the parsed JSON log records whose msg is one of the
// per-action event phrases the publisher emits.
func eventLogRecords(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		switch r["msg"] {
		case "message marked read", "message marked unread", "message moved to trash", "message restored", "message purged":
			records = append(records, r)
		}
	}
	return records
}

// TestEventLogDescribesAction drives a bulk read and a bulk trash through the
// synchronous UI bulk path and asserts the log stream names the action in prose
// while keeping the wire type and cursor for correlation. The handler is built
// at debug level so message.state_changed records are visible, and the trash is
// asserted at info so operational events stay in the default stream.
func TestEventLogDescribesAction(t *testing.T) {
	svc, _, u, _, box := httpFixture(t)
	m := seedInbound(t, svc, box, "evtlog-1", "<evtlog-1@test>", "Logged", "body")

	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	svc.Store.SetInboxLogger(log)
	h := httpapp.New(svc, log).Handler()
	cookie, csrf := uiSession(t, svc, u.ID)

	bulk := func(action string) {
		t.Helper()
		form := url.Values{"_csrf": {csrf}, "action": {action}, "ids": {m.ID}}
		rr := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/bulk", form.Encode())
		if rr.Code != 303 {
			t.Fatalf("bulk %s = %d: %s", action, rr.Code, rr.Body.String())
		}
	}
	bulk("read")
	bulk("delete")

	records := eventLogRecords(t, &logs)
	if len(records) != 2 {
		t.Fatalf("event records (%d): %s", len(records), logs.String())
	}
	read, trashed := records[0], records[1]
	if read["msg"] != "message marked read" {
		t.Fatalf("read record msg = %v, want %q", read["msg"], "message marked read")
	}
	if read["type"] != "message.state_changed" || read["level"] != "DEBUG" {
		t.Fatalf("read record type/level = %v/%v, want message.state_changed/DEBUG", read["type"], read["level"])
	}
	if read["entity_id"] != m.ID || read["inbox_id"] != m.InboxID || read["cursor"] == "" {
		t.Fatalf("read record lost correlation fields: %v", read)
	}
	if trashed["msg"] != "message moved to trash" || trashed["type"] != "message.trashed" || trashed["level"] != "INFO" {
		t.Fatalf("trash record = %v", trashed)
	}
	if trashed["entity_id"] != m.ID || trashed["inbox_id"] != m.InboxID {
		t.Fatalf("trash record lost correlation fields: %v", trashed)
	}
}

// TestEventLogHidesStateChangeAtInfo proves the default (info) stream carries no
// message.state_changed record, so routine read churn does not flood the log,
// and that the old generic "event published" phrase is gone.
func TestEventLogHidesStateChangeAtInfo(t *testing.T) {
	svc, _, u, _, box := httpFixture(t)
	m := seedInbound(t, svc, box, "evtlog-2", "<evtlog-2@test>", "Quiet", "body")

	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil)) // default threshold: info
	svc.Store.SetInboxLogger(log)
	h := httpapp.New(svc, log).Handler()
	cookie, csrf := uiSession(t, svc, u.ID)

	form := url.Values{"_csrf": {csrf}, "action": {"read"}, "ids": {m.ID}}
	if rr := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/bulk", form.Encode()); rr.Code != 303 {
		t.Fatalf("bulk read = %d", rr.Code)
	}

	if strings.Contains(logs.String(), "message.state_changed") {
		t.Fatalf("state change leaked into the info stream: %s", logs.String())
	}
	if strings.Contains(logs.String(), "event published") {
		t.Fatalf("the generic phrase should no longer be emitted: %s", logs.String())
	}
}
