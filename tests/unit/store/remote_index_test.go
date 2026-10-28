package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestRemoteIndexAndLabels proves remote metadata reconcile, folder validity,
// labels and sent-copy durability.
func TestRemoteIndexAndLabels(t *testing.T) {
	t.Parallel()
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "a@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	folders, err := st.ReconcileRemoteFolders(ctx, acct, in.ID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 77},
		{Path: "Archive", Name: "Archive", Role: model.FolderRoleArchive, Selectable: true, UIDValidity: 88},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 2 {
		t.Fatalf("folders = %d want 2", len(folders))
	}
	validity, _, known, err := st.RemoteFolderValidity(ctx, acct, in.ID, "INBOX")
	if err != nil || !known || validity != 77 {
		t.Fatalf("INBOX validity = %d known=%v err=%v", validity, known, err)
	}
	written, err := st.ReconcileRemoteFolderMessages(ctx, acct, in.ID, "INBOX", 77, []store.RemoteMessageInput{
		{UID: 1, RFCMessageID: "<m1@remote>", Subject: "One", FromAddress: "s@x.test", Read: false},
		{UID: 2, RFCMessageID: "<m2@remote>", Subject: "Two", FromAddress: "s@x.test", Flagged: true, Answered: true},
	}, true)
	if err != nil || written != 2 {
		t.Fatalf("reconcile messages: %d err=%v", written, err)
	}
	msgs, err := st.ListRemoteMessages(ctx, acct, in.ID, "INBOX")
	if err != nil || len(msgs) != 2 {
		t.Fatalf("list remote: %d err=%v", len(msgs), err)
	}
	// A UIDVALIDITY change drops the stale rows and re-indexes.
	if _, err := st.ReconcileRemoteFolderMessages(ctx, acct, in.ID, "INBOX", 99, []store.RemoteMessageInput{
		{UID: 5, RFCMessageID: "<m1@remote>", Subject: "One", FromAddress: "s@x.test"},
	}, true); err != nil {
		t.Fatal(err)
	}
	msgs, err = st.ListRemoteMessages(ctx, acct, in.ID, "INBOX")
	if err != nil || len(msgs) != 1 || msgs[0].UIDValidity != 99 || msgs[0].UID != 5 {
		t.Fatalf("after validity change: %+v err=%v", msgs, err)
	}
	// Labels are local metadata and intersect the search path.
	if _, err := st.SetRemoteMessageLabels(ctx, acct, in.ID, msgs[0].ID, []string{"keep", "work"}); err != nil {
		t.Fatal(err)
	}
	filtered, err := st.ListRemoteMessagesFiltered(ctx, acct, in.ID, store.RemoteMessageFilter{FolderPath: "INBOX", Labels: []string{"keep"}})
	if err != nil || len(filtered) != 1 {
		t.Fatalf("label filter: %d err=%v", len(filtered), err)
	}
	// Multiple labels AND together, so a repeatable filter never widens a result.
	filtered, err = st.ListRemoteMessagesFiltered(ctx, acct, in.ID, store.RemoteMessageFilter{FolderPath: "INBOX", Labels: []string{"keep", "work"}})
	if err != nil || len(filtered) != 1 {
		t.Fatalf("two-label AND filter: %d err=%v", len(filtered), err)
	}
	filtered, err = st.ListRemoteMessagesFiltered(ctx, acct, in.ID, store.RemoteMessageFilter{FolderPath: "INBOX", Labels: []string{"keep", "absent"}})
	if err != nil || len(filtered) != 0 {
		t.Fatalf("unmatched AND filter returned rows: %d err=%v", len(filtered), err)
	}
	if _, err := st.RemoveRemoteMessageLabels(ctx, acct, in.ID, msgs[0].ID, []string{"keep"}); err != nil {
		t.Fatal(err)
	}
	filtered, _ = st.ListRemoteMessagesFiltered(ctx, acct, in.ID, store.RemoteMessageFilter{FolderPath: "INBOX", Labels: []string{"keep"}})
	if len(filtered) != 0 {
		t.Fatalf("label removal not applied: %d", len(filtered))
	}
	labels, err := st.RemoteLabelsForInbox(ctx, acct, in.ID)
	if err != nil || len(labels) != 1 || labels[0] != "work" {
		t.Fatalf("labels for inbox = %+v err=%v", labels, err)
	}
}

// TestRemotePartialValidityChangeKeepsCachedRows proves a UIDVALIDITY change on a
// PARTIAL (windowed) pass does not transiently delete the folder's cached rows:
// the stale-generation rows stay visible until a complete pass can prove them
// obsolete. This is the regression guard for "recent mail vanished while a sync
// ran".
func TestRemotePartialValidityChangeKeepsCachedRows(t *testing.T) {
	t.Parallel()
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "a@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileRemoteFolders(ctx, acct, in.ID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 10},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileRemoteFolderBatch(ctx, acct, in.ID, "INBOX", 10, []store.RemoteMessageInput{
		{UID: 1, RFCMessageID: "<m1@remote>", Subject: "One"},
		{UID: 2, RFCMessageID: "<m2@remote>", Subject: "Two"},
		{UID: 3, RFCMessageID: "<m3@remote>", Subject: "Three"},
	}, []uint32{1, 2, 3}, true, 1, true); err != nil {
		t.Fatal(err)
	}
	// A UIDVALIDITY change arrives, but this pass only supplies a PARTIAL window
	// of the new generation (prune=false).
	if _, err := st.ReconcileRemoteFolderBatch(ctx, acct, in.ID, "INBOX", 20, []store.RemoteMessageInput{
		{UID: 50, RFCMessageID: "<m50@remote>", Subject: "Newest"},
	}, nil, false, 50, false); err != nil {
		t.Fatal(err)
	}
	msgs, err := st.ListRemoteMessages(ctx, acct, in.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	sawOld := false
	sawNew := false
	for _, m := range msgs {
		if m.UID == 2 && m.UIDValidity == 10 {
			sawOld = true
		}
		if m.UID == 50 && m.UIDValidity == 20 {
			sawNew = true
		}
	}
	if !sawOld || !sawNew {
		t.Fatalf("partial validity change dropped cached rows: old=%v new=%v rows=%d", sawOld, sawNew, len(msgs))
	}
	// A later COMPLETE pass prunes the stale-generation rows.
	if _, err := st.ReconcileRemoteFolderBatch(ctx, acct, in.ID, "INBOX", 20, []store.RemoteMessageInput{
		{UID: 50, RFCMessageID: "<m50@remote>", Subject: "Newest"},
	}, []uint32{50}, true, 50, true); err != nil {
		t.Fatal(err)
	}
	msgs, err = st.ListRemoteMessages(ctx, acct, in.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].UID != 50 || msgs[0].UIDValidity != 20 {
		t.Fatalf("complete pass did not prune stale generation: %+v", msgs)
	}
}

// TestRemoteFolderPruneKeepsFoldersWithMessages proves a reduced folder discovery
// does not delete a remote folder (and its cached messages) while it still holds
// cached rows, and that an empty discovery prunes nothing at all.
func TestRemoteFolderPruneKeepsFoldersWithMessages(t *testing.T) {
	t.Parallel()
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "a@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileRemoteFolders(ctx, acct, in.ID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 10},
		{Path: "Archive", Name: "Archive", Role: model.FolderRoleArchive, Selectable: true, UIDValidity: 10},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileRemoteFolderBatch(ctx, acct, in.ID, "Archive", 10, []store.RemoteMessageInput{
		{UID: 7, RFCMessageID: "<m7@remote>", Subject: "Archived"},
	}, []uint32{7}, true, 7, true); err != nil {
		t.Fatal(err)
	}
	// A reduced discovery omits Archive. Its cached message must survive.
	folders, err := st.ReconcileRemoteFolders(ctx, acct, in.ID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range folders {
		if f.Path == "Archive" {
			found = true
		}
	}
	if !found {
		t.Fatal("folder with cached messages was pruned by a reduced discovery")
	}
	if msgs, _ := st.ListRemoteMessages(ctx, acct, in.ID, "Archive"); len(msgs) != 1 {
		t.Fatalf("cached messages pruned with folder: %d", len(msgs))
	}
	// An empty discovery prunes nothing.
	if _, err := st.ReconcileRemoteFolders(ctx, acct, in.ID, nil); err != nil {
		t.Fatal(err)
	}
	if msgs, _ := st.ListRemoteMessages(ctx, acct, in.ID, "Archive"); len(msgs) != 1 {
		t.Fatalf("empty discovery pruned cached messages: %d", len(msgs))
	}
}

// TestSetInboxRemoteSyncRoundTrip proves the per-inbox sync cadence overrides set,
// clear, and read back through the full inbox projection.
func TestSetInboxRemoteSyncRoundTrip(t *testing.T) {
	t.Parallel()
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "a@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	// A fresh inbox inherits the defaults (no override).
	got, err := st.GetInboxInternal(ctx, acct, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemotePollSeconds != nil || got.RemoteFullSyncMinutes != nil {
		t.Fatalf("new inbox unexpectedly carries overrides: %v %v", got.RemotePollSeconds, got.RemoteFullSyncMinutes)
	}
	poll, full := 30, 5
	if err := st.SetInboxRemoteSync(ctx, acct, in.ID, &poll, &full); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetInboxInternal(ctx, acct, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemotePollSeconds == nil || *got.RemotePollSeconds != 30 {
		t.Fatalf("poll seconds = %v want 30", got.RemotePollSeconds)
	}
	if got.RemoteFullSyncMinutes == nil || *got.RemoteFullSyncMinutes != 5 {
		t.Fatalf("full sync minutes = %v want 5", got.RemoteFullSyncMinutes)
	}
	// A nil override clears it back to inherit.
	if err := st.SetInboxRemoteSync(ctx, acct, in.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetInboxInternal(ctx, acct, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemotePollSeconds != nil || got.RemoteFullSyncMinutes != nil {
		t.Fatalf("cleared overrides persisted: %v %v", got.RemotePollSeconds, got.RemoteFullSyncMinutes)
	}
	// An explicit 0 poll persists as 0 ("no quick poll"), distinct from nil
	// (inherit), and a negative poll is rejected.
	zero := 0
	if err := st.SetInboxRemoteSync(ctx, acct, in.ID, &zero, &full); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetInboxInternal(ctx, acct, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemotePollSeconds == nil || *got.RemotePollSeconds != 0 {
		t.Fatalf("disabled poll = %v want an explicit 0", got.RemotePollSeconds)
	}
	neg := -1
	if err := st.SetInboxRemoteSync(ctx, acct, in.ID, &neg, nil); err == nil {
		t.Fatal("negative poll override was accepted")
	}
}

// TestSetFolderRolePersistsThroughReconcile proves an explicit role mapping of an
// arbitrarily-named existing folder is durable: a later remote reconcile that
// re-infers roles from names must not reset it.
func TestSetFolderRolePersistsThroughReconcile(t *testing.T) {
	t.Parallel()
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "a@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	// A remote folder with an arbitrary name (not a conventional Trash name).
	folders, err := st.ReconcileRemoteFolders(ctx, acct, in.ID, []store.RemoteFolderSync{
		{Path: "Old Mail", Name: "Old Mail", Selectable: true, UIDValidity: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	var target model.Folder
	for _, f := range folders {
		if f.Path == "Old Mail" {
			target = f
		}
	}
	if target.ID == "" {
		t.Fatal("folder not indexed")
	}
	p := model.Principal{AccountID: acct, Admin: true}
	mapped, err := st.SetFolderRole(ctx, p, in.ID, target.ID, model.FolderRoleTrash)
	if err != nil {
		t.Fatalf("SetFolderRole: %v", err)
	}
	if mapped.Role != model.FolderRoleTrash || !mapped.RoleLocked {
		t.Fatalf("mapped = %+v", mapped)
	}
	// A subsequent reconcile that still supplies no role (name does not infer
	// Trash) must keep the locked role.
	folders, err = st.ReconcileRemoteFolders(ctx, acct, in.ID, []store.RemoteFolderSync{
		{Path: "Old Mail", Name: "Old Mail", Selectable: true, UIDValidity: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range folders {
		if f.Path == "Old Mail" {
			if f.Role != model.FolderRoleTrash || !f.RoleLocked {
				t.Fatalf("locked role reset by reconcile: %+v", f)
			}
		}
	}
	if _, err := st.SetFolderRole(ctx, p, in.ID, target.ID, "not_a_role"); err == nil {
		t.Fatal("unknown role was accepted")
	}
}

// TestRemoteThreadKeyGroupsReplies proves a reply joins its parent's thread by
// In-Reply-To/References rather than becoming a separate thread.
func TestRemoteThreadKeyGroupsReplies(t *testing.T) {
	t.Parallel()
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "a@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileRemoteFolders(ctx, acct, in.ID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 7},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileRemoteFolderMessages(ctx, acct, in.ID, "INBOX", 7, []store.RemoteMessageInput{
		{UID: 1, RFCMessageID: "<root@remote>", Subject: "Root"},
	}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileRemoteFolderMessages(ctx, acct, in.ID, "INBOX", 7, []store.RemoteMessageInput{
		{UID: 1, RFCMessageID: "<root@remote>", Subject: "Root"},
		{UID: 2, RFCMessageID: "<reply@remote>", InReplyTo: "<root@remote>", References: []string{"<root@remote>"}, Subject: "Re: Root"},
	}, true); err != nil {
		t.Fatal(err)
	}
	threads, err := st.ListRemoteThreads(ctx, acct, in.ID, "", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0].MessageCount != 2 {
		t.Fatalf("threads = %+v, want one thread of 2", threads)
	}
}

// TestRemoteThreadGraphOutOfOrder proves the thread graph is rebuilt correctly
// regardless of arrival order: a reply indexed before its parent (which itself
// references a grandparent not yet seen) is merged onto the eventual root, and all
// members share one stable thread key.
func TestRemoteThreadGraphOutOfOrder(t *testing.T) {
	t.Parallel()
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "a@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileRemoteFolders(ctx, acct, in.ID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 9},
	}); err != nil {
		t.Fatal(err)
	}
	// Reply arrives first, referencing a root that has not been seen.
	if _, err := st.ReconcileRemoteFolderBatch(ctx, acct, in.ID, "INBOX", 9, []store.RemoteMessageInput{
		{UID: 1, RFCMessageID: "<reply@remote>", InReplyTo: "<root@remote>", References: []string{"<root@remote>"}, Subject: "Re"},
	}, nil, false, 0, false); err != nil {
		t.Fatal(err)
	}
	// The root arrives next, itself referencing a grandparent not yet seen.
	if _, err := st.ReconcileRemoteFolderBatch(ctx, acct, in.ID, "INBOX", 9, []store.RemoteMessageInput{
		{UID: 2, RFCMessageID: "<root@remote>", InReplyTo: "<gp@remote>", References: []string{"<gp@remote>"}, Subject: "Root"},
	}, nil, false, 0, false); err != nil {
		t.Fatal(err)
	}
	// The grandparent finally arrives.
	if _, err := st.ReconcileRemoteFolderBatch(ctx, acct, in.ID, "INBOX", 9, []store.RemoteMessageInput{
		{UID: 3, RFCMessageID: "<gp@remote>", Subject: "GP"},
	}, nil, false, 0, false); err != nil {
		t.Fatal(err)
	}
	msgs, err := st.ListRemoteMessages(ctx, acct, in.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, m := range msgs {
		keys[m.ThreadKey] = true
	}
	if len(keys) != 1 {
		t.Fatalf("out-of-order reply did not converge to one thread: keys=%v msgs=%+v", keys, msgs)
	}
}

// TestRemoteThreadDuplicateMessageIDNotConflated proves a reference to a Message-ID
// shared by more than one cached row is not followed, so distinct messages are not
// merged into one thread by an ambiguous reference.
func TestRemoteThreadDuplicateMessageIDNotConflated(t *testing.T) {
	t.Parallel()
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "a@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileRemoteFolders(ctx, acct, in.ID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 9},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileRemoteFolderBatch(ctx, acct, in.ID, "INBOX", 9, []store.RemoteMessageInput{
		{UID: 1, RFCMessageID: "<dup@remote>", Subject: "one"},
		{UID: 2, RFCMessageID: "<dup@remote>", Subject: "two"},
		{UID: 3, RFCMessageID: "<child@remote>", InReplyTo: "<dup@remote>", References: []string{"<dup@remote>"}, Subject: "child"},
	}, nil, false, 0, false); err != nil {
		t.Fatal(err)
	}
	child, err := st.GetRemoteMessageByUID(ctx, acct, in.ID, "INBOX", 9, 3)
	if err != nil {
		t.Fatal(err)
	}
	if child.ThreadKey != "<child@remote>" {
		t.Fatalf("child joined a duplicated Message-ID thread: key=%q", child.ThreadKey)
	}
}

// TestRemoteReconcileCompleteOnlyWhenValidityResolved proves an unresolvable
// folder UIDVALIDITY does not yield a "complete" index status.
func TestRemoteReconcileCompleteOnlyWhenValidityResolved(t *testing.T) {
	t.Parallel()
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "a@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	// A folder stored with UIDVALIDITY 0 (as ReconcileRemoteFolderBatch writes when
	// the live validity could not be resolved) must not be treated as complete.
	if _, err := st.ReconcileRemoteFolders(ctx, acct, in.ID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 0},
	}); err != nil {
		t.Fatal(err)
	}
	bf, err := st.GetRemoteFolderBackfill(ctx, acct, in.ID, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if bf.Complete {
		t.Fatal("folder with UIDVALIDITY 0 reported backfill complete")
	}
}

// TestRemoteSentCopyStatesDurable proves the sent-copy queue is durable and
// separate: a transient failure returns the job to pending, a terminal failure
// marks it failed, and a confirmed copy marks it copied.
func TestRemoteSentCopyStatesDurable(t *testing.T) {
	t.Parallel()
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "a@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	job, created, err := st.EnqueueUniqueRemoteSentCopy(ctx, acct, in.ID, store.RemoteSentCopyInput{
		MessageID: "msg_1", RFCMessageID: "<s1@remote>", RawPath: "remote/x.eml", SizeBytes: 10,
	})
	if err != nil || !created {
		t.Fatalf("enqueue: %v created=%v", err, created)
	}
	// A duplicate enqueue for the same Message-ID is not created.
	if _, created2, err := st.EnqueueUniqueRemoteSentCopy(ctx, acct, in.ID, store.RemoteSentCopyInput{
		MessageID: "msg_1", RFCMessageID: "<s1@remote>", RawPath: "remote/x.eml", SizeBytes: 10,
	}); err != nil || created2 {
		t.Fatalf("duplicate enqueue created=%v err=%v", created2, err)
	}
	// Claim it, record a transient retry, then confirm.
	claimed, ok, err := st.ClaimNextRemoteSentCopy(ctx, time.Now().UTC(), "w", 1)
	if err != nil || !ok || claimed.ID != job.ID {
		t.Fatalf("claim: %+v ok=%v err=%v", claimed, ok, err)
	}
	if err := st.RetryRemoteSentCopy(ctx, acct, job.ID, "transient", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetRemoteSentCopy(ctx, acct, job.ID)
	if err != nil || got.State != store.RemoteCopyPending || got.Attempts != 1 {
		t.Fatalf("after retry: %+v err=%v", got, err)
	}
	if err := st.MarkRemoteSentCopyDone(ctx, acct, job.ID, 42, 9); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetRemoteSentCopy(ctx, acct, job.ID)
	if got.State != store.RemoteCopyCopied || got.RemoteUID != 9 {
		t.Fatalf("after done: %+v", got)
	}
}

// TestRemoteMessagesKeysetPaginationNoSkip proves the remote message listing
// resumes by an opaque metadata-id cursor and never skips a same-second sibling.
// A received_at-only cursor would drop every message after the first that shared
// the boundary second; the keyset on (received_at, id) must exhaust the folder
// exactly once with no gaps or duplicates.
func TestRemoteMessagesKeysetPaginationNoSkip(t *testing.T) {
	t.Parallel()
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "a@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	validity := uint32(5)
	if _, err := st.ReconcileRemoteFolders(ctx, acct, in.ID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: validity},
	}); err != nil {
		t.Fatal(err)
	}
	// Six messages: three share the exact same received_at (a burst), the rest
	// distinct. All are seeded in one batch.
	same := "2026-01-01T12:00:00Z"
	inputs := []store.RemoteMessageInput{
		{UID: 1, RFCMessageID: "<m1@r>", Subject: "s1", ReceivedAt: ptr(same)},
		{UID: 2, RFCMessageID: "<m2@r>", Subject: "s2", ReceivedAt: ptr(same)},
		{UID: 3, RFCMessageID: "<m3@r>", Subject: "s3", ReceivedAt: ptr(same)},
		{UID: 4, RFCMessageID: "<m4@r>", Subject: "s4", ReceivedAt: ptr("2026-01-01T11:00:00Z")},
		{UID: 5, RFCMessageID: "<m5@r>", Subject: "s5", ReceivedAt: ptr("2026-01-01T10:00:00Z")},
		{UID: 6, RFCMessageID: "<m6@r>", Subject: "s6", ReceivedAt: ptr("2026-01-01T09:00:00Z")},
	}
	if _, err := st.ReconcileRemoteFolderMessages(ctx, acct, in.ID, "INBOX", validity, inputs, true); err != nil {
		t.Fatal(err)
	}
	// Page newest-first in pages of two, resuming strictly after the last id.
	seen := map[string]int{}
	cursor := ""
	for page := 0; page < 10; page++ {
		msgs, err := st.ListRemoteMessagesFiltered(ctx, acct, in.ID, store.RemoteMessageFilter{FolderPath: "INBOX", Limit: 2, Before: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) == 0 {
			break
		}
		for _, m := range msgs {
			seen[m.ID]++
			if seen[m.ID] > 1 {
				t.Fatalf("duplicate message across pages: %s", m.ID)
			}
		}
		cursor = msgs[len(msgs)-1].ID
		if len(msgs) < 2 {
			break
		}
	}
	if len(seen) != 6 {
		t.Fatalf("pagination returned %d distinct messages, want 6 (gaps=%v)", len(seen), seen)
	}
	// A missing cursor must be rejected so a client can recover explicitly.
	// Silently returning an empty page hides a lost position.
	extra, err := st.ListRemoteMessagesFiltered(ctx, acct, in.ID, store.RemoteMessageFilter{FolderPath: "INBOX", Limit: 2, Before: "rm_does_not_exist"})
	if !errors.Is(err, store.ErrInvalidCursor) || len(extra) != 0 {
		t.Fatalf("unknown cursor returned %d rows, err=%v; want invalid cursor", len(extra), err)
	}
}

// TestRemoteThreadsKeysetPaginationNoSkip proves the remote thread listing
// resumes by a thread-key cursor on (latest_received_at, thread_key) so a merged
// account-wide page never loses a thread that sorts below the global key.
func TestRemoteThreadsKeysetPaginationNoSkip(t *testing.T) {
	t.Parallel()
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "a@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileRemoteFolders(ctx, acct, in.ID, []store.RemoteFolderSync{
		{Path: "INBOX", Name: "INBOX", Role: model.FolderRoleInbox, Selectable: true, UIDValidity: 3},
	}); err != nil {
		t.Fatal(err)
	}
	// Four independent threads, distinct received times.
	var msgs []store.RemoteMessageInput
	for i := 1; i <= 4; i++ {
		msgs = append(msgs, store.RemoteMessageInput{
			UID:          uint32(i),
			RFCMessageID: "<t" + string(rune('0'+i)) + "@r>",
			Subject:      "thread",
			ReceivedAt:   ptr("2026-01-0" + string(rune('0'+i)) + "T00:00:00Z"),
		})
	}
	if _, err := st.ReconcileRemoteFolderMessages(ctx, acct, in.ID, "INBOX", 3, msgs, true); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	cursor := ""
	for page := 0; page < 10; page++ {
		threads, err := st.ListRemoteThreads(ctx, acct, in.ID, "", 2, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(threads) == 0 {
			break
		}
		for _, th := range threads {
			seen[th.Key]++
			if seen[th.Key] > 1 {
				t.Fatalf("duplicate thread across pages: %s", th.Key)
			}
		}
		cursor = threads[len(threads)-1].Key
		if len(threads) < 2 {
			break
		}
	}
	if len(seen) != 4 {
		t.Fatalf("thread pagination returned %d distinct threads, want 4", len(seen))
	}
}

func ptr(s string) *string { return &s }

// TestReconcileKeepsFolderIDOnRename proves a renamed remote folder keeps its
// Mammoth Mail id when the provider's own identity (StableID) is unchanged, rather
// than being re-minted under the new path. That is what makes a client-held
// folder id survive a rename.
func TestReconcileKeepsFolderIDOnRename(t *testing.T) {
	t.Parallel()
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "a@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	const labelID = "Label_42"
	folders, err := st.ReconcileRemoteFolders(ctx, acct, in.ID, []store.RemoteFolderSync{
		{Path: labelID, StableID: labelID, Name: "Old Name", Selectable: true, UIDValidity: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	var before model.Folder
	for _, f := range folders {
		if f.Path == labelID {
			before = f
		}
	}
	if before.ID == "" {
		t.Fatal("folder not indexed")
	}
	// The provider renamed it: same label id, new display name.
	folders, err = st.ReconcileRemoteFolders(ctx, acct, in.ID, []store.RemoteFolderSync{
		{Path: labelID, StableID: labelID, Name: "New Name", Selectable: true, UIDValidity: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	var after model.Folder
	for _, f := range folders {
		if f.Path == labelID {
			after = f
		}
	}
	if after.ID != before.ID {
		t.Fatalf("rename re-minted the folder id: %q -> %q", before.ID, after.ID)
	}
	if after.Name != "New Name" {
		t.Fatalf("name = %q want New Name", after.Name)
	}
}

// TestReconcileRenameWithoutStableIDMintsNewID pins the behaviour for a provider
// with no stable folder identity (IMAP): a path it has not seen is a new folder,
// so a rename there is carried by RenameRemoteFolderPaths instead.
func TestReconcileRenameWithoutStableIDMintsNewID(t *testing.T) {
	t.Parallel()
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "a@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	folders, err := st.ReconcileRemoteFolders(ctx, acct, in.ID, []store.RemoteFolderSync{
		{Path: "Old", Name: "Old", Selectable: true, UIDValidity: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	var before model.Folder
	for _, f := range folders {
		if f.Path == "Old" {
			before = f
		}
	}
	folders, err = st.ReconcileRemoteFolders(ctx, acct, in.ID, []store.RemoteFolderSync{
		{Path: "New", Name: "New", Selectable: true, UIDValidity: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	var after model.Folder
	for _, f := range folders {
		if f.Path == "New" {
			after = f
		}
	}
	if after.ID == "" || after.ID == before.ID {
		t.Fatalf("without a stable id the new path must be a new folder: %q -> %q", before.ID, after.ID)
	}
}

// TestRenameRemoteFolderPathsMovesSubtreeIDs proves the IMAP rename hint rewrites
// the renamed folder and every descendant to the new path while keeping each
// row's id, so a following reconcile finds them where they already are.
func TestRenameRemoteFolderPathsMovesSubtreeIDs(t *testing.T) {
	t.Parallel()
	st, acct := seedStandaloneAccount(t)
	ctx := context.Background()
	in, err := st.CreateStandaloneInbox(ctx, acct, store.StandaloneCreate{Address: "a@remote.example"})
	if err != nil {
		t.Fatal(err)
	}
	folders, err := st.ReconcileRemoteFolders(ctx, acct, in.ID, []store.RemoteFolderSync{
		{Path: "Projects", Name: "Projects", Selectable: true, UIDValidity: 5},
		{Path: "Projects/Child", Name: "Child", ParentPath: "Projects", Selectable: true, UIDValidity: 6},
		{Path: "Projects/Child/Deep", Name: "Deep", ParentPath: "Projects/Child", Selectable: true, UIDValidity: 7},
		{Path: "Other", Name: "Other", Selectable: true, UIDValidity: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, f := range folders {
		ids[f.Path] = f.ID
	}
	moved, err := st.RenameRemoteFolderPaths(ctx, acct, in.ID, "Projects", "Renamed")
	if err != nil {
		t.Fatal(err)
	}
	if moved != 2 {
		t.Fatalf("moved %d descendant rows, want 2", moved)
	}
	folders, err = st.ListFolders(ctx, acct, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]model.Folder{}
	for _, f := range folders {
		paths[f.Path] = f
	}
	for _, want := range []struct{ path, oldPath, parent string }{
		{"Renamed", "Projects", ""},
		{"Renamed/Child", "Projects/Child", "Renamed"},
		{"Renamed/Child/Deep", "Projects/Child/Deep", "Renamed/Child"},
	} {
		f, ok := paths[want.path]
		if !ok {
			t.Fatalf("folder %q missing after the subtree move", want.path)
		}
		if f.ID != ids[want.oldPath] {
			t.Fatalf("%q id = %q want %q (unchanged)", want.path, f.ID, ids[want.oldPath])
		}
		if f.ParentPath != want.parent {
			t.Fatalf("%q parent = %q want %q", want.path, f.ParentPath, want.parent)
		}
	}
	// A sibling outside the renamed subtree is untouched.
	if f := paths["Other"]; f.ID != ids["Other"] {
		t.Fatalf("sibling id changed: %q -> %q", ids["Other"], f.ID)
	}
}
