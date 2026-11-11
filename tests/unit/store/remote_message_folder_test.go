package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/tests/support/testdb"
)

// TestRemoteMessageFolderResolvesProviderFolders proves a cached row's real
// folder is resolved per provider, so a Gmail or Outlook draft is recognised as
// sitting in the Drafts folder even though its row is filed under a
// provider-prefixed id rather than the folder path.
func TestRemoteMessageFolderResolvesProviderFolders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := testdb.Open(t)
	u, err := s.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	box, err := s.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "mixed@example.net"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveGoogleConnection(ctx, store.GoogleConnection{AccountID: u.AccountID, InboxID: box.ID, ClientID: "client", EncryptedSecret: "s", EncryptedToken: "t", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	received := time.Now().UTC().Format(time.RFC3339Nano)
	in := func(id string) store.RemoteMessageInput {
		return store.RemoteMessageInput{RFCMessageID: "<" + id + "@test>", Subject: id, FromAddress: "a@example.net", ReceivedAt: &received, InternalDate: &received}
	}
	for _, c := range []struct {
		provider string
		labels   []string
		want     string
	}{
		{"g-draft", []string{"INBOX", "DRAFT"}, "DRAFT"},
		{"g-inbox", []string{"INBOX"}, "INBOX"},
		{"g-none", []string{"Label_1"}, ""},
	} {
		rec, err := s.UpsertGoogleMessage(ctx, u.AccountID, box.ID, c.provider, "th-"+c.provider, c.labels, in(c.provider))
		if err != nil {
			t.Fatal(err)
		}
		if got := s.RemoteMessageFolder(ctx, u.AccountID, box.ID, rec); got != c.want {
			t.Errorf("gmail %s: folder %q, want %q", c.provider, got, c.want)
		}
	}
	msBox, err := s.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "outlook@example.net"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveMicrosoftConnection(ctx, store.MicrosoftConnection{AccountID: u.AccountID, InboxID: msBox.ID, ClientID: "client", Tenant: "common", IdentityID: "id", Mailbox: "outlook@example.net", EncryptedToken: "t", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ms, err := s.UpsertMicrosoftMessage(ctx, u.AccountID, msBox.ID, "ms-1", "th-ms", "graph-drafts-id", in("ms-1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.RemoteMessageFolder(ctx, u.AccountID, msBox.ID, ms); got != "graph-drafts-id" {
		t.Errorf("outlook: folder %q, want the Graph folder id", got)
	}
	imap := store.RemoteMessage{ID: "x", FolderPath: "Drafts"}
	if got := s.RemoteMessageFolder(ctx, u.AccountID, box.ID, imap); got != "Drafts" {
		t.Errorf("imap: folder %q, want Drafts", got)
	}
}
