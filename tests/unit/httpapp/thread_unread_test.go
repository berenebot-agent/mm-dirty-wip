package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/imap"
)

// Assert the wire field itself, including zero, rather than accepting a missing
// field as Go's integer zero value.
type threadUnreadResponse struct {
	ID           string `json:"id"`
	MessageCount int    `json:"message_count"`
	UnreadCount  *int   `json:"unread_count"`
}

func assertThreadUnread(t *testing.T, row threadUnreadResponse, count, unread int) {
	t.Helper()
	if row.UnreadCount == nil {
		t.Fatalf("thread %s: unread_count missing", row.ID)
	}
	if row.MessageCount != count || *row.UnreadCount != unread {
		t.Fatalf("thread %s: message_count=%d unread_count=%d, want %d/%d", row.ID, row.MessageCount, *row.UnreadCount, count, unread)
	}
}

func assertThreadUnreadRoutes(t *testing.T, h http.Handler, key, inbox, thread string, count, unread int) {
	t.Helper()
	for _, path := range []string{
		"/v1/inboxes/" + inbox + "/threads",
		"/v1/threads?inbox=" + inbox,
		"/v1/threads",
	} {
		res := apiGet(t, h, path, key)
		var page struct {
			Items []threadUnreadResponse `json:"items"`
		}
		if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &page) != nil {
			t.Fatalf("%s: %d %s", path, res.Code, res.Body.String())
		}
		found := false
		for _, row := range page.Items {
			if row.ID == thread {
				assertThreadUnread(t, row, count, unread)
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: missing thread %s: %s", path, thread, res.Body.String())
		}
	}
	for _, path := range []string{
		"/v1/inboxes/" + inbox + "/threads/" + url.PathEscape(thread),
		"/v1/threads/" + url.PathEscape(thread) + "?inbox=" + inbox,
		"/v1/threads/" + url.PathEscape(thread),
	} {
		res := apiGet(t, h, path, key)
		var row threadUnreadResponse
		if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &row) != nil {
			t.Fatalf("%s: %d %s", path, res.Code, res.Body.String())
		}
		assertThreadUnread(t, row, count, unread)
	}
}

func TestThreadUnreadCountsAcrossDomainAndStandaloneRoutes(t *testing.T) {
	t.Parallel()
	svc, h, u, local, remote, fake := standaloneFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	key := adminKey(t, svc, u)
	root := seedInbound(t, svc, local, "unread-root", "<unread-root@test>", "Local thread", "body")
	var reply model.Message
	for _, name := range []string{"read", "spam", "trash"} {
		m, _, _, err := svc.Store.CommitInbound(ctx, store.InboundRecord{
			Inbox: local, Provider: "mailgun", ProviderDeliveryID: "unread-" + name,
			RFCMessageID: "<unread-" + name + "@test>", InReplyTo: root.RFCMessageID,
			From: root.From, To: []string{local.Address}, Subject: "Re: Local thread",
			Text: "reply", RawPath: "messages/test.eml", SizeBytes: 5, ReceivedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if m.ThreadID != root.ThreadID {
			t.Fatalf("reply was not grouped with root: %s", m.ThreadID)
		}
		switch name {
		case "read":
			reply = m
			read := true
			if _, err := svc.Store.UpdateMessageState(ctx, p, m.ID, &read); err != nil {
				t.Fatal(err)
			}
		case "spam":
			if _, _, err := svc.Store.SetMessageSpam(ctx, p, m.ID, true); err != nil {
				t.Fatal(err)
			}
		case "trash":
			if _, _, err := svc.Store.TrashMessage(ctx, p, m.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := svc.Store.ReplaceMessageLabels(ctx, p, root.ID, []string{"Selected"}); err != nil {
		t.Fatal(err)
	}
	folder, err := svc.Store.CreateFolder(ctx, p, local.ID, store.FolderCreate{Path: "Read mail"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Store.MoveMessageToFolder(ctx, p, reply.ID, folder.ID); err != nil {
		t.Fatal(err)
	}
	fake.add("INBOX", "From: a@b.test\r\nMessage-ID: <unread-remote-root@test>\r\nSubject: Remote thread\r\n\r\nbody", "<unread-remote-root@test>", "Remote thread")
	fake.add("INBOX", "From: a@b.test\r\nMessage-ID: <unread-remote-reply@test>\r\nIn-Reply-To: <unread-remote-root@test>\r\nReferences: <unread-remote-root@test>\r\nSubject: Re: Remote thread\r\n\r\nreply", "<unread-remote-reply@test>", "Re: Remote thread")
	fake.mu.Lock()
	fake.messages[1].flags = []string{imap.FlagSeen}
	fake.mu.Unlock()
	refreshRemote(t, h, remote.ID, key)
	res := apiGet(t, h, "/v1/inboxes/"+remote.ID+"/threads", key)
	var page struct {
		Items []threadUnreadResponse `json:"items"`
	}
	if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &page) != nil || len(page.Items) != 1 {
		t.Fatalf("remote thread: %d %s", res.Code, res.Body.String())
	}
	remoteThread := page.Items[0].ID
	assertThreadUnreadRoutes(t, h, key, local.ID, root.ThreadID, 2, 1)
	assertThreadUnreadRoutes(t, h, key, remote.ID, remoteThread, 2, 1)
	folders, err := svc.Store.ListFolders(ctx, p.AccountID, remote.ID)
	if err != nil {
		t.Fatal(err)
	}
	var inboxLabel string
	for _, f := range folders {
		if f.Role == model.FolderRoleInbox && f.Origin == "remote" {
			inboxLabel = "folder:" + f.ID
		}
	}
	if inboxLabel == "" {
		t.Fatal("remote Inbox label missing")
	}
	// Filtered counts must use the same subset as message_count.
	for _, tc := range []struct {
		inbox, filter string
		count, unread int
	}{
		{local.ID, "label_id=tag:Selected", 1, 1},
		{local.ID, "folder=" + url.QueryEscape(folder.ID), 1, 0},
		{remote.ID, "label_id=" + url.QueryEscape(inboxLabel), 2, 1},
	} {
		for _, path := range []string{"/v1/inboxes/" + tc.inbox + "/threads?", "/v1/threads?inbox=" + tc.inbox + "&"} {
			res := apiGet(t, h, path+tc.filter, key)
			var page struct {
				Items []threadUnreadResponse `json:"items"`
			}
			if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &page) != nil || len(page.Items) != 1 {
				t.Fatalf("filtered threads: %d %s", res.Code, res.Body.String())
			}
			assertThreadUnread(t, page.Items[0], tc.count, tc.unread)
		}
	}
	filtered := apiGet(t, h, "/v1/threads?label_id=tag:Selected", key)
	var filteredPage struct {
		Items []threadUnreadResponse `json:"items"`
	}
	if filtered.Code != 200 || json.Unmarshal(filtered.Body.Bytes(), &filteredPage) != nil || len(filteredPage.Items) != 1 {
		t.Fatalf("merged filtered threads: %d %s", filtered.Code, filtered.Body.String())
	}
	assertThreadUnread(t, filteredPage.Items[0], 1, 1)
	// A one-thread page must carry each source's unread count through the merge.
	seen := map[string]bool{}
	before := ""
	for i := 0; i < 3; i++ {
		res := apiGet(t, h, "/v1/threads?limit=1"+before, key)
		var page struct {
			Items []threadUnreadResponse `json:"items"`
			Next  string                 `json:"next_cursor"`
		}
		if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &page) != nil {
			t.Fatalf("thread page: %d %s", res.Code, res.Body.String())
		}
		for _, row := range page.Items {
			assertThreadUnread(t, row, 2, 1)
			if seen[row.ID] {
				t.Fatalf("duplicate thread: %s", row.ID)
			}
			seen[row.ID] = true
		}
		if page.Next == "" {
			break
		}
		before = "&before=" + url.QueryEscape(page.Next)
	}
	if len(seen) != 2 || !seen[root.ThreadID] || !seen[remoteThread] {
		t.Fatalf("missing paginated threads: %v", seen)
	}
	// Reading every visible message must preserve the explicit zero on the wire.
	read := true
	if _, err := svc.Store.UpdateMessageState(ctx, p, root.ID, &read); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	for _, m := range fake.messages {
		m.flags = []string{imap.FlagSeen}
	}
	fake.mu.Unlock()
	refreshRemote(t, h, remote.ID, key)
	assertThreadUnreadRoutes(t, h, key, local.ID, root.ThreadID, 2, 0)
	assertThreadUnreadRoutes(t, h, key, remote.ID, remoteThread, 2, 0)
}
