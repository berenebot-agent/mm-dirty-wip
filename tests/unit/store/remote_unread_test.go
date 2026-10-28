package store_test

import (
	"context"
	"testing"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

// seedStandaloneWithFolders creates a standalone inbox whose folder set mirrors a
// remote server: an INBOX (inbox-role) folder plus a Sent folder, both indexed
// with a mix of read and unread messages. It returns the store, account, inbox and
// the number of unread messages placed in the INBOX folder.
func seedStandaloneWithFolders(t *testing.T) (*store.Store, string, model.Inbox, int) {
	t.Helper()
	ctx := context.Background()
	st, acct := seedStandaloneAccount(t)
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "a@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileRemoteFolders(ctx, acct, in.ID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 10},
		{Path: "Sent", Name: "Sent", Role: model.FolderRoleSent, Selectable: true, UIDValidity: 20},
	}); err != nil {
		t.Fatal(err)
	}
	inboxMsgs := []store.RemoteMessageInput{
		{UID: 1, RFCMessageID: "<u1@remote>", Subject: "Unread one", Read: false},
		{UID: 2, RFCMessageID: "<r1@remote>", Subject: "Read one", Read: true},
		{UID: 3, RFCMessageID: "<u2@remote>", Subject: "Unread two", Read: false},
	}
	if _, err := st.ReconcileRemoteFolderMessages(ctx, acct, in.ID, "INBOX", 10, inboxMsgs, true); err != nil {
		t.Fatal(err)
	}
	// Sent mail is unread too, but it must never inflate the Inbox badge.
	sentMsgs := []store.RemoteMessageInput{
		{UID: 1, RFCMessageID: "<s1@remote>", Subject: "Sent unread", Read: false},
		{UID: 2, RFCMessageID: "<s2@remote>", Subject: "Sent read", Read: true},
	}
	if _, err := st.ReconcileRemoteFolderMessages(ctx, acct, in.ID, "Sent", 20, sentMsgs, true); err != nil {
		t.Fatal(err)
	}
	return st, acct, in, 2
}

// TestRemoteInboxUnreadCountsCountsOnlyInboxRoleFolder proves the standalone
// Inbox badge counts unread messages in the inbox-role folder only: a Sent
// folder's unread is excluded, matching the local inbox definition (inbound, not
// spam, not deleted).
func TestRemoteInboxUnreadCountsCountsOnlyInboxRoleFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, acct, in, wantUnread := seedStandaloneWithFolders(t)
	p := model.Principal{AccountID: acct, Admin: true, MailboxRoles: map[string]string{}}

	counts, err := st.RemoteInboxUnreadCounts(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if counts[in.ID] != wantUnread {
		t.Fatalf("inbox unread = %d, want %d (Sent unread must not be counted)", counts[in.ID], wantUnread)
	}
}

// TestRemoteInboxUnreadCountsNamespaceFallback proves an inbox with no explicit
// inbox-role mapping falls back to its namespace root, matching the folder the
// Inbox view lists.
func TestRemoteInboxUnreadCountsNamespaceFallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, acct := seedStandaloneAccount(t)
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "a@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	// No folder reconcile: no inbox-role mapping exists, so the namespace root
	// (INBOX) stands in for the Inbox.
	if _, err := st.ReconcileRemoteFolderMessages(ctx, acct, in.ID, "INBOX", 10, []store.RemoteMessageInput{
		{UID: 1, RFCMessageID: "<u1@remote>", Subject: "Unread", Read: false},
		{UID: 2, RFCMessageID: "<r1@remote>", Subject: "Read", Read: true},
		{UID: 3, RFCMessageID: "<u2@remote>", Subject: "Unread two", Read: false},
	}, true); err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: acct, Admin: true, MailboxRoles: map[string]string{}}
	counts, err := st.RemoteInboxUnreadCounts(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if counts[in.ID] != 2 {
		t.Fatalf("namespace fallback unread = %d, want 2", counts[in.ID])
	}
}

// TestRemoteInboxUnreadCountsPrincipalScoping proves a non-Admin sees a
// standalone inbox's unread only when they hold a role on it, and that another
// account's inbox is never counted.
func TestRemoteInboxUnreadCountsPrincipalScoping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, acct, in, wantUnread := seedStandaloneWithFolders(t)

	noAccess := model.Principal{AccountID: acct, MailboxRoles: map[string]string{}}
	counts, err := st.RemoteInboxUnreadCounts(ctx, noAccess)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := counts[in.ID]; ok {
		t.Fatalf("inbox-less principal saw unread %d for %s, want none", n, in.ID)
	}

	withAccess := model.Principal{AccountID: acct, MailboxRoles: map[string]string{in.ID: "read"}}
	counts, err = st.RemoteInboxUnreadCounts(ctx, withAccess)
	if err != nil {
		t.Fatal(err)
	}
	if counts[in.ID] != wantUnread {
		t.Fatalf("scoped unread = %d, want %d", counts[in.ID], wantUnread)
	}

	// A principal from another account must not see this inbox even by id.
	other := model.Principal{AccountID: "acct_other", MailboxRoles: map[string]string{in.ID: "read"}}
	counts, err = st.RemoteInboxUnreadCounts(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := counts[in.ID]; ok {
		t.Fatalf("foreign account saw unread %d for %s, want none", n, in.ID)
	}
}
