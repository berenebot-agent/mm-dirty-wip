package store_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/tests/support/testdb"
)

// TestGoogleFolderScopeFollowsSingleFolderPrecedence proves a Gmail message
// carrying several system labels belongs to exactly one folder view — Trash,
// Spam, Drafts, Inbox, Sent — and that a message with no system label belongs
// to no folder. A user label stays additive and does not select a folder.
func TestGoogleFolderScopeFollowsSingleFolderPrecedence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := testdb.Open(t)
	u, err := s.CreateAccountAndAdmin(ctx, "A", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	box, err := s.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{Address: "gmail@example.net"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveGoogleConnection(ctx, store.GoogleConnection{AccountID: u.AccountID, InboxID: box.ID, ClientID: "client", EncryptedSecret: "s", EncryptedToken: "t", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	received := time.Now().UTC().Format(time.RFC3339Nano)
	for _, m := range []struct {
		provider string
		labels   []string
	}{
		{"m-trash", []string{"INBOX", "TRASH"}},
		{"m-spam", []string{"INBOX", "SPAM"}},
		{"m-draft", []string{"INBOX", "DRAFT"}},
		{"m-inbox", []string{"INBOX"}},
		{"m-sent-inbox", []string{"SENT", "INBOX"}},
		{"m-sent", []string{"SENT"}},
		{"m-archive", []string{"Label_1"}},
	} {
		rec, err := s.UpsertGoogleMessage(ctx, u.AccountID, box.ID, m.provider, "th-"+m.provider, m.labels, store.RemoteMessageInput{
			RFCMessageID: "<" + m.provider + "@test>", Subject: m.provider, FromAddress: "sender@example.net",
			ReceivedAt: &received, InternalDate: &received,
		})
		if err != nil {
			t.Fatal(err)
		}
		if m.provider == "m-inbox" || m.provider == "m-sent-inbox" {
			if err := s.UpdateRemoteMessageFlags(ctx, u.AccountID, box.ID, rec.ID, true, false, false, false, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	view := func(path string) []string {
		t.Helper()
		rows, err := s.ListRemoteMessagesFiltered(ctx, u.AccountID, box.ID, store.RemoteMessageFilter{FolderPath: path})
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, r := range rows {
			out = append(out, r.Subject)
		}
		sort.Strings(out)
		return out
	}
	for _, c := range []struct {
		path string
		want []string
	}{
		{"TRASH", []string{"m-trash"}},
		{"SPAM", []string{"m-spam"}},
		{"DRAFT", []string{"m-draft"}},
		// Inbox outranks Sent: a message carrying both is Inbox mail.
		{"INBOX", []string{"m-inbox", "m-sent-inbox"}},
		{"SENT", []string{"m-sent"}},
		// A user label is an additive label, not a folder view.
		{"Label_1", []string{}},
		// There is no Archive folder: a message with no system label is in no
		// folder view.
		{"ARCHIVE", []string{}},
	} {
		got := view(c.path)
		if len(got) != len(c.want) {
			t.Fatalf("%s view = %v, want %v", c.path, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s view = %v, want %v", c.path, got, c.want)
			}
		}
	}
	// An unfiltered listing is the whole mailbox, including the mail that
	// carries no system label.
	all := view("")
	if len(all) != 7 {
		t.Fatalf("unfiltered listing = %v, want every cached message", all)
	}
	// The folder listing (and its counts) must work for a Gmail index.
	folders, err := s.ListFolders(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("folders: %+v", folders)
	counts, err := s.CountRemoteFolderMessages(ctx, u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("counts: %+v", counts)
	if counts["ARCHIVE"][0] != 0 {
		t.Fatalf("archive count = %v, want none", counts["ARCHIVE"])
	}
	if counts["INBOX"][0] != 2 || counts["INBOX"][1] != 0 {
		t.Fatalf("inbox counts = %v, want 2 total and 0 unread", counts["INBOX"])
	}
	if counts["SPAM"][0] != 1 || counts["SPAM"][1] != 1 {
		t.Fatalf("spam counts = %v, want 1 total and 1 unread", counts["SPAM"])
	}
	// The thread scope resolves the same single folder view.
	threads, err := s.ListRemoteThreads(ctx, u.AccountID, box.ID, "INBOX", 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 2 {
		t.Fatalf("Inbox threads = %d, want 2", len(threads))
	}
	archived, err := s.ListRemoteThreads(ctx, u.AccountID, box.ID, "ARCHIVE", 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 0 {
		t.Fatalf("Archive threads = %+v, want none", archived)
	}
}
