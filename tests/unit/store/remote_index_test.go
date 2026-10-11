package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestMigration056RemoteIndex proves migration 056 adds the remote-index columns,
// the local-label table and the sent-copy queue on a genuinely fresh database.
func TestMigration056RemoteIndex(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n := markerCount(t, st, "056"); n != 1 {
		t.Fatalf("migration 056 marker count %d", n)
	}
	db := rawDB(t, st.Path())
	defer db.Close()
	ctx := context.Background()
	var n int
	for _, col := range []string{"is_answered", "is_draft", "flags_json", "internal_date", "indexed_at"} {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('inbox_remote_messages') WHERE name=?`, col).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("inbox_remote_messages.%s missing after 056", col)
		}
	}
	for _, col := range []string{"remote_index_status", "remote_indexed_at", "remote_index_error"} {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('inboxes') WHERE name=?`, col).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("inboxes.%s missing after 056", col)
		}
	}
	for _, table := range []string{"inbox_remote_labels", "remote_sent_copies"} {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("table %s missing after 056", table)
		}
	}
}

// TestRemoteIndexAndLabels proves remote metadata reconcile, folder validity,
// labels and sent-copy durability.
func TestRemoteIndexAndLabels(t *testing.T) {
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
	filtered, err := st.ListRemoteMessagesFiltered(ctx, acct, in.ID, store.RemoteMessageFilter{FolderPath: "INBOX", Label: "keep"})
	if err != nil || len(filtered) != 1 {
		t.Fatalf("label filter: %d err=%v", len(filtered), err)
	}
	if _, err := st.RemoveRemoteMessageLabels(ctx, acct, in.ID, msgs[0].ID, []string{"keep"}); err != nil {
		t.Fatal(err)
	}
	filtered, _ = st.ListRemoteMessagesFiltered(ctx, acct, in.ID, store.RemoteMessageFilter{FolderPath: "INBOX", Label: "keep"})
	if len(filtered) != 0 {
		t.Fatalf("label removal not applied: %d", len(filtered))
	}
	labels, err := st.RemoteLabelsForInbox(ctx, acct, in.ID)
	if err != nil || len(labels) != 1 || labels[0] != "work" {
		t.Fatalf("labels for inbox = %+v err=%v", labels, err)
	}
}

// TestMigration058RoleLockAndSentCopy proves migration 058 adds the role-lock and
// sent-copy columns and the notification-message-id column on a fresh database.
func TestMigration058RoleLockAndSentCopy(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n := markerCount(t, st, "058"); n != 1 {
		t.Fatalf("migration 058 marker count %d", n)
	}
	db := rawDB(t, st.Path())
	defer db.Close()
	ctx := context.Background()
	var n int
	for _, col := range []string{"role_locked", "backfill_before_uid", "backfill_complete"} {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('inbox_folders') WHERE name=?`, col).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("inbox_folders.%s missing after 058", col)
		}
	}
	for _, col := range []string{"remote_sent_copy_enabled", "remote_sent_copy_folder"} {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('inboxes') WHERE name=?`, col).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("inboxes.%s missing after 058", col)
		}
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('assistant_handling_requests') WHERE name='notification_message_id'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("assistant_handling_requests.notification_message_id missing after 058")
	}
}

// TestMigration060RemoteSyncColumns proves the per-inbox sync-cadence columns are
// added and round-trip through SetInboxRemoteSync and a full-inbox read.
func TestMigration060RemoteSyncColumns(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if n := markerCount(t, st, "060"); n != 1 {
		t.Fatalf("migration 060 marker count %d", n)
	}
	db := rawDB(t, st.Path())
	defer db.Close()
	ctx := context.Background()
	var n int
	for _, col := range []string{"remote_poll_seconds", "remote_full_sync_minutes"} {
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('inboxes') WHERE name=?`, col).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("inboxes.%s missing after 060", col)
		}
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('inbox_folders') WHERE name='remote_highest_modseq'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("inbox_folders.remote_highest_modseq missing after 060")
	}
}

// TestSetInboxRemoteSyncRoundTrip proves the per-inbox sync cadence overrides set,
// clear, and read back through the full inbox projection.
func TestSetInboxRemoteSyncRoundTrip(t *testing.T) {
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
}

// TestSetFolderRolePersistsThroughReconcile proves an explicit role mapping of an
// arbitrarily-named existing folder is durable: a later remote reconcile that
// re-infers roles from names must not reset it.
func TestSetFolderRolePersistsThroughReconcile(t *testing.T) {
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
	// An unknown cursor is an empty result, never a silent restart from newest.
	extra, err := st.ListRemoteMessagesFiltered(ctx, acct, in.ID, store.RemoteMessageFilter{FolderPath: "INBOX", Limit: 2, Before: "rm_does_not_exist"})
	if err != nil {
		t.Fatal(err)
	}
	if len(extra) != 0 {
		t.Fatalf("unknown cursor returned %d rows, want 0", len(extra))
	}
}

// TestRemoteThreadsKeysetPaginationNoSkip proves the remote thread listing
// resumes by a thread-key cursor on (latest_received_at, thread_key) so a merged
// account-wide page never loses a thread that sorts below the global key.
func TestRemoteThreadsKeysetPaginationNoSkip(t *testing.T) {
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
