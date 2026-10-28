package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dellarb/mammothmail/internal/logging"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/tests/support/testdb"
)

func inboxLogRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func TestInboxLifecycleLogging(t *testing.T) {
	t.Parallel()
	s, u, d, _ := testStore(t)
	var logs bytes.Buffer
	s.SetInboxLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	ctx := store.WithInboxLogActor(context.Background(), "api", model.Principal{AccountID: u.AccountID, APIKeyID: "key_actor", UserID: u.ID})
	box, err := s.CreateInbox(ctx, u.AccountID, d.ID, "logged", "do-not-log-display")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateInbox(ctx, u.AccountID, d.ID, "logged", "duplicate"); err == nil {
		t.Fatal("duplicate creation succeeded")
	}
	if _, err = s.CreateInbox(ctx, u.AccountID, d.ID, "invalid@local", ""); err == nil {
		t.Fatal("invalid creation succeeded")
	}
	if _, err = s.PurgeInbox(ctx, "wrong-account", box.ID); err == nil {
		t.Fatal("cross-account deletion succeeded")
	}
	if _, err = s.PurgeInbox(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PurgeInbox(ctx, u.AccountID, box.ID); err == nil {
		t.Fatal("repeated deletion succeeded")
	}
	records := inboxLogRecords(t, &logs)
	if len(records) != 2 {
		t.Fatalf("want two events, got %s", logs.String())
	}
	for i, event := range []string{"inbox.created", "inbox.deleted"} {
		for field, want := range map[string]any{"msg": event, "level": "INFO", "account_id": u.AccountID, "inbox_id": box.ID, "address": box.Address, "inbox_kind": "domain", "domain_id": d.ID, "source": "api", "actor_type": "api_key", "api_key_id": "key_actor", "user_id": u.ID} {
			if records[i][field] != want {
				t.Errorf("event %d %s = %v, want %v", i, field, records[i][field], want)
			}
		}
	}
	if records[1]["reason"] != "direct_delete" || records[0]["operation"] != "inbox_create" || records[1]["operation"] != "inbox_delete" {
		t.Fatalf("operations/reason: %s", logs.String())
	}
	if strings.Contains(logs.String(), "do-not-log-display") {
		t.Fatal("display name leaked")
	}
}

func TestInboxCoreTerminalLogFormat(t *testing.T) {
	t.Parallel()
	s, u, d, _ := testStore(t)
	var logs bytes.Buffer
	s.SetInboxLogger(logging.New(&logs, slog.LevelInfo, logging.PrefixCore))
	box, err := s.CreateInbox(context.Background(), u.AccountID, d.ID, "terminal", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"[Core] INFO inbox.created", "account_id=" + u.AccountID, "inbox_id=" + box.ID, "address=terminal@example.com", "source=internal", "actor_type=system"} {
		if !strings.Contains(logs.String(), field) {
			t.Fatalf("missing %q in terminal log: %s", field, logs.String())
		}
	}
	if strings.Count(logs.String(), "\n") != 1 {
		t.Fatal("lifecycle event should occupy one terminal line")
	}
}

func TestStandaloneLifecycleLoggingAndSetupCleanup(t *testing.T) {
	t.Parallel()
	s, u, _, _ := testStore(t)
	var logs bytes.Buffer
	s.SetInboxLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	ctx := store.WithInboxLogActor(context.Background(), "ui", model.Principal{UserID: u.ID})
	ctx = store.WithInboxLogOperation(ctx, "google_connect")
	box, err := s.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "external@example.net", DisplayName: "secret-label", Remote: &model.RemoteConnection{Host: "secret-host.example.net", Port: 993, Username: "secret-username", Security: "tls"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: box.Address}); err == nil {
		t.Fatal("duplicate succeeded")
	}
	if _, err = s.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "invalid"}); err == nil {
		t.Fatal("invalid succeeded")
	}
	if _, err = s.PurgeInbox(store.WithInboxLogReason(ctx, "setup_rollback"), u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	records := inboxLogRecords(t, &logs)
	if len(records) != 2 {
		t.Fatalf("events: %s", logs.String())
	}
	for _, r := range records {
		if r["inbox_kind"] != "standalone" || r["address"] != box.Address || r["actor_type"] != "user" || r["source"] != "ui" || r["operation"] != "google_connect" || r["user_id"] != u.ID {
			t.Fatalf("fields: %v", r)
		}
		if _, ok := r["domain_id"]; ok {
			t.Fatal("standalone domain_id should be omitted")
		}
	}
	if records[1]["reason"] != "setup_rollback" {
		t.Fatal("cleanup reason missing")
	}
	if strings.Contains(logs.String(), "secret-") {
		t.Fatal("connector configuration leaked")
	}
}

func TestDomainPurgeLogsEveryDeletedInbox(t *testing.T) {
	t.Parallel()
	s, u, d, boxes := testStore(t)
	var logs bytes.Buffer
	s.SetInboxLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	if _, err := s.PurgeDomain(context.Background(), "wrong-account", d.ID); err == nil {
		t.Fatal("cross-account purge succeeded")
	}
	if _, err := s.PurgeDomain(context.Background(), u.AccountID, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurgeDomain(context.Background(), u.AccountID, d.ID); err == nil {
		t.Fatal("repeated purge succeeded")
	}
	records := inboxLogRecords(t, &logs)
	if len(records) != len(boxes) {
		t.Fatalf("want %d records: %s", len(boxes), logs.String())
	}
	want := make(map[string]string)
	for _, b := range boxes {
		want[b.ID] = b.Address
	}
	for _, r := range records {
		id, _ := r["inbox_id"].(string)
		if want[id] == "" || r["address"] != want[id] || r["msg"] != "inbox.deleted" || r["reason"] != "domain_delete" || r["operation"] != "domain_delete" || r["source"] != "internal" || r["actor_type"] != "system" {
			t.Fatalf("unexpected record %v", r)
		}
		delete(want, id)
	}
}

func TestOneImportLogsOnlyCommittedNewInbox(t *testing.T) {
	t.Parallel()
	s, u, _, _ := testStore(t)
	var logs bytes.Buffer
	s.SetInboxLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	b := store.OneBinding{InboxID: "in_one_logged", AccountID: u.AccountID, ConnectionID: "connection", GmailEmail: "one@example.net", EncryptedKey: "secret-key", EncryptedConnectionKey: "secret-connection-key", KeyAAD: "aad", Fingerprint: "fingerprint", RateGroup: "group"}
	bad := b
	bad.ConnectionID = ""
	// A valid first import also exercises all connector-owned inserts.
	id, err := s.ImportOneInbox(context.Background(), b, "10")
	if err != nil {
		t.Fatal(err)
	}
	if id != b.InboxID {
		t.Fatal("wrong inbox ID")
	}
	b.InboxID = "in_unused"
	if repeated, err := s.ImportOneInbox(context.Background(), b, "10"); err != nil || repeated != id {
		t.Fatalf("repeat: %q %v", repeated, err)
	}
	bad.InboxID, bad.GmailEmail, bad.EncryptedKey = "in_invalid", "invalid@example.net", ""
	if _, err = s.ImportOneInbox(context.Background(), bad, "10"); err == nil {
		t.Fatal("invalid import succeeded")
	}
	records := inboxLogRecords(t, &logs)
	if len(records) != 1 || records[0]["msg"] != "inbox.created" || records[0]["operation"] != "one_import" || records[0]["inbox_id"] != id {
		t.Fatalf("events: %s", logs.String())
	}
	if strings.Contains(logs.String(), "secret-") || strings.Contains(logs.String(), "fingerprint") {
		t.Fatal("One credentials leaked")
	}
}

func TestInboxLoggingOmitsRolledBackMutations(t *testing.T) {
	t.Parallel()
	s, dir := testdb.OpenDir(t)
	ctx := context.Background()
	u, err := s.CreateAccountAndAdmin(ctx, "Logging", "logging@example.com", "correct horse battery staple", 100<<20)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDomain(ctx, u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	box, err := s.CreateInbox(ctx, u.AccountID, d.ID, "rollback", "")
	if err != nil {
		t.Fatal(err)
	}
	// Fault injection via SQL keeps the test external to the store package.
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		`CREATE TRIGGER fail_inbox_delete BEFORE DELETE ON inboxes BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`,
		`CREATE TRIGGER fail_one_setup BEFORE INSERT ON google_detection BEGIN SELECT RAISE(ABORT, 'injected setup failure'); END`,
	} {
		if _, err := db.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	var logs bytes.Buffer
	s.SetInboxLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	if _, err = s.PurgeInbox(ctx, u.AccountID, box.ID); err == nil {
		t.Fatal("purge did not fail")
	}
	if _, err = s.PurgeDomain(ctx, u.AccountID, d.ID); err == nil {
		t.Fatal("domain purge did not fail")
	}
	if _, err = s.GetInboxInternal(ctx, u.AccountID, box.ID); err != nil {
		t.Fatal("rolled-back deletion lost inbox", err)
	}
	b := store.OneBinding{InboxID: "in_rolled_back", AccountID: u.AccountID, ConnectionID: "connection", GmailEmail: "one@example.net", EncryptedKey: "secret", EncryptedConnectionKey: "secret", KeyAAD: "aad", Fingerprint: "fingerprint", RateGroup: "group"}
	if _, err = s.ImportOneInbox(ctx, b, "10"); err == nil {
		t.Fatal("One setup did not fail")
	}
	if _, err = s.GetInboxInternal(ctx, u.AccountID, b.InboxID); err == nil {
		t.Fatal("rolled-back import left inbox")
	}
	if logs.Len() != 0 {
		t.Fatalf("rolled-back mutations logged: %s", logs.String())
	}
}
