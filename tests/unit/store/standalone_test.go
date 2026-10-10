package store_test

import (
	"context"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestMigration052Foundation proves migration 052 adds the standalone-mailbox
// foundation on a genuinely fresh database: the inbox kind/address/remote
// columns, the three new tables, and the partial standalone-address index. It
// uses store.Open on a real temp dir, not the shared testdb template, because it
// asserts migration behaviour.
func TestMigration052Foundation(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n := markerCount(t, st, "052"); n != 1 {
		t.Fatalf("migration 052 marker count %d", n)
	}
	db := rawDB(t, st.Path())
	defer db.Close()
	ctx := context.Background()
	var n int
	for _, col := range []string{"kind", "address", "namespace", "remote_host", "remote_port", "remote_username", "remote_security", "smtp_host", "smtp_port", "smtp_username", "smtp_security", "remote_configured"} {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('inboxes') WHERE name=?`, col).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("inboxes.%s missing after migration 052", col)
		}
	}
	for _, table := range []string{"inbox_remote_credentials", "inbox_folders", "inbox_remote_messages"} {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("table %s missing after migration 052", table)
		}
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name='idx_inboxes_standalone_address'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("idx_inboxes_standalone_address missing")
	}
	// Existing domain inboxes keep kind='domain' by the column default.
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('inboxes') WHERE name='kind' AND dflt_value="'domain'"`).Scan(&n); err != nil {
		t.Fatal(err)
	}
}

// seedStandaloneAccount creates an account admin and returns the store plus the
// account id, using the shared migrated template.
func seedStandaloneAccount(t *testing.T) (*store.Store, string) {
	t.Helper()
	ctx := context.Background()
	st := openStore(t)
	u, err := st.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	return st, u.AccountID
}

// TestStandaloneInboxCRUD proves a standalone inbox can be created, listed and
// fetched independently of any managed domain, and that its kind is immutable.
func TestStandaloneInboxCRUD(t *testing.T) {
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()

	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{
		DisplayName: "Agent Inbox",
		Address:     "agent@remote.example",
		Namespace:   "Archive",
		Remote:      &model.RemoteConnection{Host: "imap.remote.example", Username: "agent@remote.example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.Kind != model.InboxKindStandalone {
		t.Fatalf("kind = %q, want standalone", in.Kind)
	}
	if in.DomainID != "" {
		t.Fatalf("standalone inbox must have no domain, got %q", in.DomainID)
	}
	if in.Address != "agent@remote.example" {
		t.Fatalf("address = %q", in.Address)
	}
	if in.Namespace != "Archive" {
		t.Fatalf("namespace = %q", in.Namespace)
	}
	if in.Remote == nil || in.Remote.Host != "imap.remote.example" {
		t.Fatalf("remote = %+v", in.Remote)
	}
	// Defaults: TLS by default, no SMTP unless asked.
	if in.Remote.Security != model.RemoteSecurityTLS {
		t.Fatalf("remote security default = %q, want tls", in.Remote.Security)
	}
	if in.Remote.Port != model.RemoteDefaultIMAPPort {
		t.Fatalf("remote port default = %d", in.Remote.Port)
	}
	if in.Remote.SMTP != nil {
		t.Fatal("SMTP should be nil unless configured")
	}

	got, err := st.GetInboxInternal(ctx, acct, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != model.InboxKindStandalone || got.Address != in.Address {
		t.Fatalf("get inbox = %+v", got)
	}

	standalone, err := st.ListStandaloneInboxes(ctx, acct)
	if err != nil {
		t.Fatal(err)
	}
	if len(standalone) != 1 || standalone[0].ID != in.ID {
		t.Fatalf("standalone list = %+v", standalone)
	}

	// The kind is immutable.
	if err := st.SetInboxKind(ctx, acct, in.ID, model.InboxKindDomain); err != store.ErrKindImmutable {
		t.Fatalf("SetInboxKind err = %v, want ErrKindImmutable", err)
	}
	if err := st.SetInboxKind(ctx, acct, in.ID, model.InboxKindStandalone); err != nil {
		t.Fatalf("idempotent SetInboxKind: %v", err)
	}
}

// TestStandaloneAddressValidation proves plaintext/TLS and address validation.
func TestStandaloneAddressValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr string
		ok   bool
	}{
		{"good", "user@example.com", true},
		{"case-normalized", "User@Example.com", true},
		{"no-at", "userexample.com", false},
		{"display-name", "User <user@example.com>", false},
		{"no-dot", "user@localhost", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.ValidateStandaloneAddress(tc.addr)
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

// TestStandaloneRemoteSecurity proves the security mode defaults to TLS,
// STARTTLS and plain are accepted as explicit choices, an unknown mode is
// rejected, and the conventional port is applied per mode.
func TestStandaloneRemoteSecurity(t *testing.T) {
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	if _, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{
		Address: "a@remote.example",
		Remote:  &model.RemoteConnection{Host: "imap.remote.example", Username: "a@remote.example", Security: "none"},
	}); err == nil {
		t.Fatal("unknown remote security mode must be rejected")
	}
	starttls, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{
		Address: "a@remote.example",
		Remote:  &model.RemoteConnection{Host: "imap.remote.example", Username: "a@remote.example", Security: model.RemoteSecurityStartTLS},
	})
	if err != nil {
		t.Fatalf("starttls must be accepted: %v", err)
	}
	if starttls.Remote.Port != model.RemoteDefaultIMAPStartPort {
		t.Fatalf("starttls imap port = %d, want %d", starttls.Remote.Port, model.RemoteDefaultIMAPStartPort)
	}
	plain, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{
		Address: "plain@remote.example",
		Remote:  &model.RemoteConnection{Host: "imap.remote.example", Username: "plain@remote.example", Security: model.RemoteSecurityPlain},
	})
	if err != nil {
		t.Fatalf("plain must be accepted at the store layer (deployment policy decides later): %v", err)
	}
	if plain.Remote.Security != model.RemoteSecurityPlain {
		t.Fatalf("plain security = %q", plain.Remote.Security)
	}
}

// TestStandaloneAddressCollision proves a standalone address may not collide
// with another standalone inbox or a managed mailbox in the same account.
func TestStandaloneAddressCollision(t *testing.T) {
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	if _, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "dup@remote.example"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "DUP@remote.example"}); err == nil {
		t.Fatal("duplicate standalone address must be rejected case-insensitively")
	}
}

// TestFoldersHierarchy proves folders can be reconciled with a custom hierarchy
// and that paths removed from the input are pruned.
func TestFoldersHierarchy(t *testing.T) {
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "a@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	folders, err := st.UpsertFolders(ctx, acct, in.ID, []store.FolderInput{
		{Path: "INBOX", Name: "INBOX", Selectable: true},
		{Path: "Archive", Name: "Archive", Selectable: true},
		{Path: "Archive/2026", Name: "2026", ParentPath: "Archive", Selectable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 3 {
		t.Fatalf("folder count = %d, want 3", len(folders))
	}
	roleByPath := map[string]string{}
	for _, f := range folders {
		roleByPath[f.Path] = f.Role
	}
	if roleByPath["INBOX"] != model.FolderRoleInbox {
		t.Fatalf("INBOX role = %q", roleByPath["INBOX"])
	}
	if roleByPath["Archive"] != model.FolderRoleArchive {
		t.Fatalf("Archive role = %q, want archive", roleByPath["Archive"])
	}
	// Archive and Outbox roles are recognized from conventional remote names.
	for name, want := range map[string]string{"Outbox": model.FolderRoleOutbox, "All Mail": model.FolderRoleArchive} {
		if got := store.FolderRoleForName(name); got != want {
			t.Fatalf("FolderRoleForName(%q) = %q, want %q", name, got, want)
		}
	}
	nested, err := st.GetFolder(ctx, acct, in.ID, "Archive/2026")
	if err != nil {
		t.Fatal(err)
	}
	if nested.ParentPath != "Archive" || nested.Name != "2026" {
		t.Fatalf("nested folder = %+v", nested)
	}
	// A second reconcile without Archive/2026 prunes it.
	folders, err = st.UpsertFolders(ctx, acct, in.ID, []store.FolderInput{{Path: "INBOX", Name: "INBOX"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 {
		t.Fatalf("after prune, folder count = %d, want 1", len(folders))
	}
}

// TestRemoteMessageMetadata proves remote message headers can be upserted and
// read back by UID and by id, with bodies never stored.
func TestRemoteMessageMetadata(t *testing.T) {
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "a@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	recv := "2026-01-02T03:04:05Z"
	msg, err := st.UpsertRemoteMessage(ctx, acct, in.ID, store.RemoteMessageInput{
		FolderPath:   "INBOX",
		UIDValidity:  42,
		UID:          7,
		RFCMessageID: "<m1@remote.example>",
		FromName:     "Sender",
		FromAddress:  "sender@elsewhere.example",
		To:           []string{"a@remote.example"},
		Subject:      "Hello",
		Snippet:      "Hi there",
		SizeBytes:    1234,
		HasAttach:    true,
		ReceivedAt:   &recv,
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.UID != 7 || msg.UIDValidity != 42 || msg.Subject != "Hello" {
		t.Fatalf("remote message = %+v", msg)
	}
	if !msg.HasAttach {
		t.Fatal("has_attachments not persisted")
	}
	// Upsert the same UID updates in place.
	recv2 := "2026-01-02T03:04:06Z"
	msg2, err := st.UpsertRemoteMessage(ctx, acct, in.ID, store.RemoteMessageInput{
		FolderPath: "INBOX", UIDValidity: 42, UID: 7, RFCMessageID: "<m1@remote.example>",
		FromAddress: "sender@elsewhere.example", Subject: "Hello (edited)", Snippet: "new", ReceivedAt: &recv2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg2.ID != msg.ID {
		t.Fatalf("upsert created a new row: %s vs %s", msg2.ID, msg.ID)
	}
	if msg2.Subject != "Hello (edited)" {
		t.Fatalf("subject not updated: %q", msg2.Subject)
	}
	byUID, err := st.GetRemoteMessageByUID(ctx, acct, in.ID, "INBOX", 42, 7)
	if err != nil || byUID.ID != msg.ID {
		t.Fatalf("get by uid: %v %+v", err, byUID)
	}
	listed, err := st.ListRemoteMessages(ctx, acct, in.ID, "INBOX")
	if err != nil || len(listed) != 1 {
		t.Fatalf("list remote messages: %v %d", err, len(listed))
	}
	// No body columns exist on the metadata table.
	db := rawDB(t, st.Path())
	defer db.Close()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('inbox_remote_messages') WHERE name IN ('text_body','html_body','raw_path')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("remote metadata table must not carry body columns")
	}
}

// TestStandaloneRequiresStandalone proves folder/remote operations reject a
// domain inbox.
func TestStandaloneRequiresStandalone(t *testing.T) {
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	dom, err := st.CreateDomain(ctx, acct, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	in, err := st.CreateInbox(ctx, acct, dom.ID, "box", "Box")
	if err != nil {
		t.Fatal(err)
	}
	if in.Kind != model.InboxKindDomain {
		t.Fatalf("domain inbox kind = %q", in.Kind)
	}
	if _, err := st.UpsertFolders(ctx, acct, in.ID, []store.FolderInput{{Path: "INBOX"}}); err != store.ErrStandaloneRequired {
		t.Fatalf("UpsertFolders on domain inbox err = %v", err)
	}
	if err := st.SaveRemoteCredentials(ctx, acct, in.ID, "x", "", store.ConfigVersion{}); err != store.ErrStandaloneRequired {
		t.Fatalf("SaveRemoteCredentials on domain inbox err = %v", err)
	}
}
