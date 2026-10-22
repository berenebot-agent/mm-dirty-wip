package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func TestSystemLabelsLocalClassification(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	key := adminKey(t, svc, u)
	catalog := apiGet(t, h, "/v1/inboxes/"+box.ID+"/label-catalog", key)
	var labels struct {
		Items []struct {
			ID, Role, Membership string
			CanAdd               bool `json:"can_add"`
			CanRemove            bool `json:"can_remove"`
			CanRename            bool `json:"can_rename"`
			CanDelete            bool `json:"can_delete"`
		} `json:"items"`
	}
	if catalog.Code != 200 || json.Unmarshal(catalog.Body.Bytes(), &labels) != nil {
		t.Fatalf("catalogue: %d %s", catalog.Code, catalog.Body.String())
	}
	ids := map[string]string{}
	for _, label := range labels.Items {
		ids[label.Role] = label.ID
		if label.CanRename || label.CanDelete || label.CanRemove {
			t.Fatalf("local system label must be protected: %+v", label)
		}
		if label.Role == "sent" && label.CanAdd {
			t.Fatal("Sent must be read-only")
		}
	}
	for _, role := range []string{"inbox", "sent", "spam", "trash", "archive"} {
		if ids[role] == "" {
			t.Fatalf("missing system label %s", role)
		}
	}
	m := seedInbound(t, svc, box, "system-labels", "<system-labels@test>", "Classification", "body")
	if _, err := svc.Store.ReplaceMessageLabels(ctx, p, m.ID, []string{"Alpha", "Beta"}); err != nil {
		t.Fatal(err)
	}
	apply := func(id, op string, want int) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/v1/inboxes/"+box.ID+"/messages/"+m.ID+"/labels?wait=true", strings.NewReader(`{"label_id":"`+id+`","operation":"`+op+`"}`))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("apply %s %s: %d %s", op, id, w.Code, w.Body.String())
		}
	}
	for _, role := range []string{"inbox", "spam", "trash", "inbox", "archive", "inbox"} {
		apply(ids[role], "add", 200)
		for _, path := range []string{
			"/v1/inboxes/" + box.ID + "/messages?",
			"/v1/messages?inbox=" + box.ID + "&",
			"/v1/inboxes/" + box.ID + "/search?",
			"/v1/search?inbox=" + box.ID + "&",
		} {
			w := apiGet(t, h, path+"label_id="+url.QueryEscape(ids[role])+"&label_id=tag:Alpha&label_id=tag:Beta", key)
			var page struct {
				Items []struct {
					ID       string
					LabelIDs []string `json:"label_ids"`
				} `json:"items"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].ID != m.ID {
				t.Fatalf("%s %s: %d %s", role, path, w.Code, w.Body.String())
			}
			if !stringSliceContains(page.Items[0].LabelIDs, ids[role]) || !stringSliceContains(page.Items[0].LabelIDs, "tag:Beta") {
				t.Fatalf("membership differs from filter: %+v", page.Items[0])
			}
		}
		if role != "inbox" {
			w := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages?label_id="+url.QueryEscape(ids["inbox"]), key)
			var page struct {
				Items []json.RawMessage `json:"items"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 0 {
				t.Fatalf("%s must leave Inbox: %d %s", role, w.Code, w.Body.String())
			}
		}
	}
	apply(ids["sent"], "add", 400)
	apply(ids["inbox"], "remove", 409)
	// An explicitly addressed additive tag must not resolve to a same-name system label.
	apply("tag:Inbox", "add", 200)
	detail := apiGet(t, h, "/v1/messages/"+m.ID, key)
	if !strings.Contains(detail.Body.String(), `"tag:Inbox"`) || !strings.Contains(detail.Body.String(), ids["inbox"]) {
		t.Fatalf("tag/system name collision: %s", detail.Body.String())
	}
	apply("tag:Inbox", "remove", 200)
	// A label-driven state transition must publish the same durable events as
	// the legacy spam/trash/restore actions, not just folder_changed.
	events, err := svc.Store.ListEvents(ctx, p, 0, box.ID, 200)
	if err != nil {
		t.Fatal(err)
	}
	eventCounts := map[string]int{}
	for _, event := range events {
		if event.EntityID == m.ID {
			eventCounts[event.Type]++
		}
	}
	for kind, count := range map[string]int{
		model.EventMessageSpamChanged: 2,
		model.EventMessageTrashed:     1,
		model.EventMessageRestored:    1,
	} {
		if eventCounts[kind] != count {
			t.Fatalf("classification event %s emitted %d times, want %d (all: %v)", kind, eventCounts[kind], count, eventCounts)
		}
	}
	// The UI derives selected membership from the same projection as the API.
	cookie, _ := uiSession(t, svc, u.ID)
	r := httptest.NewRequest(http.MethodGet, "/ui/messages/"+m.ID, nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `<span class="labelpill">Inbox</span>`) {
		t.Fatalf("UI Inbox membership: %d %s", w.Code, w.Body.String())
	}
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestSystemLabelsIMAPExclusiveViewsAndMove(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, fake := standaloneFixture(t)
	fake.folders["Spam"] = 100
	fake.folders["Trash"] = 100
	fake.add("INBOX", "From: a@b.test\r\nSubject: Labels\r\nMessage-ID: <labels-imap@test>\r\n\r\nbody", "<labels-imap@test>", "Labels")
	key := adminKey(t, svc, u)
	seed := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages", key)
	if seed.Code != 200 {
		t.Fatalf("seed: %d %s", seed.Code, seed.Body.String())
	}
	catalog := apiGet(t, h, "/v1/inboxes/"+box.ID+"/label-catalog", key)
	var labels struct {
		Items []struct{ ID, Role, Membership string } `json:"items"`
	}
	if catalog.Code != 200 || json.Unmarshal(catalog.Body.Bytes(), &labels) != nil {
		t.Fatalf("catalogue: %d %s", catalog.Code, catalog.Body.String())
	}
	ids := map[string]string{}
	for _, label := range labels.Items {
		ids[label.Role] = label.ID
		if label.Membership != "exclusive" {
			t.Fatalf("IMAP labels must be exclusive: %+v", label)
		}
	}
	var messageID string
	for _, path := range []string{
		"/v1/inboxes/" + box.ID + "/messages?", "/v1/messages?inbox=" + box.ID + "&",
		"/v1/inboxes/" + box.ID + "/search?", "/v1/search?inbox=" + box.ID + "&",
	} {
		w := apiGet(t, h, path+"label_id="+url.QueryEscape(ids["inbox"]), key)
		var page struct {
			Items []struct {
				ID       string
				LabelIDs []string `json:"label_ids"`
			} `json:"items"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || !stringSliceContains(page.Items[0].LabelIDs, ids["inbox"]) {
			t.Fatalf("IMAP label filter %s: %d %s", path, w.Code, w.Body.String())
		}
		messageID = page.Items[0].ID
	}
	for _, step := range []struct {
		op, role string
		status   int
	}{{"remove", "inbox", 409}, {"add", "spam", 200}} {
		req := httptest.NewRequest(http.MethodPost, "/v1/inboxes/"+box.ID+"/messages/"+messageID+"/labels?wait=true", strings.NewReader(`{"label_id":"`+ids[step.role]+`","operation":"`+step.op+`"}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != step.status {
			t.Fatalf("IMAP %s: %d %s", step.op, w.Code, w.Body.String())
		}
	}
	if got := fake.folderOf("<labels-imap@test>"); got != "Spam" {
		t.Fatalf("applying exclusive label must move: %s", got)
	}
	for _, label := range []string{ids["inbox"], "folder:other-inbox"} {
		w := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages?label_id="+url.QueryEscape(label)+"&label_id="+url.QueryEscape(ids["spam"]), key)
		if w.Code != 400 {
			t.Fatalf("invalid exclusive intersection: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestSentLabelExcludesPendingAndFailedOutbound(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	key := adminKey(t, svc, u)
	newOutbound := func(subject string) model.Message {
		t.Helper()
		m, _, err := svc.Store.CommitOutbound(ctx, store.OutboundRecord{
			Inbox: box, Provider: "smtp",
			RFCMessageID: "<" + subject + "@example.test>",
			From: model.Address{Address: box.Address},
			To: []string{"recipient@example.test"}, Subject: subject, Text: subject,
			RawPath: "messages/" + subject + ".eml", SizeBytes: 5,
		})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	pending := newOutbound("pending-sent-label")
	failed := newOutbound("failed-sent-label")
	sent := newOutbound("completed-sent-label")
	if _, _, err := svc.Store.MarkFailed(ctx, u.AccountID, failed.ID, "rejected", time.Now().UTC(), 1, "smtp", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Store.MarkSent(ctx, u.AccountID, sent.ID, "<wire-sent@test>", "smtp", ""); err != nil {
		t.Fatal(err)
	}
	catalog := apiGet(t, h, "/v1/inboxes/"+box.ID+"/label-catalog", key)
	var labels struct {
		Items []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"items"`
	}
	if catalog.Code != 200 || json.Unmarshal(catalog.Body.Bytes(), &labels) != nil {
		t.Fatalf("catalogue: %d %s", catalog.Code, catalog.Body.String())
	}
	var sentLabel string
	for _, label := range labels.Items {
		if label.Role == model.FolderRoleSent {
			sentLabel = label.ID
		}
	}
	if sentLabel == "" {
		t.Fatal("Sent label not advertised")
	}
	for _, tc := range []struct {
		id      string
		hasSent bool
	}{
		{pending.ID, false}, {failed.ID, false}, {sent.ID, true},
	} {
		res := apiGet(t, h, "/v1/messages/"+tc.id, key)
		var msg struct {
			LabelIDs []string `json:"label_ids"`
		}
		if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &msg) != nil {
			t.Fatalf("message detail: %d %s", res.Code, res.Body.String())
		}
		if got := stringSliceContains(msg.LabelIDs, sentLabel); got != tc.hasSent {
			t.Fatalf("%s Sent membership = %v, want %v (%v)", tc.id, got, tc.hasSent, msg.LabelIDs)
		}
	}
	for _, path := range []string{
		"/v1/inboxes/" + box.ID + "/messages?label_id=",
		"/v1/messages?inbox=" + box.ID + "&label_id=",
		"/v1/inboxes/" + box.ID + "/search?label_id=",
		"/v1/inboxes/" + box.ID + "/threads?label_id=",
	} {
		res := apiGet(t, h, path+url.QueryEscape(sentLabel), key)
		var page struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &page) != nil || len(page.Items) != 1 {
			t.Fatalf("%s: %d %s", path, res.Code, res.Body.String())
		}
		if strings.Contains(path, "/threads") {
			if page.Items[0].ID != sent.ThreadID {
				t.Fatalf("unexpected sent thread: %s", res.Body.String())
			}
		} else if page.Items[0].ID != sent.ID {
			t.Fatalf("unexpected sent message: %s", res.Body.String())
		}
	}
}

func TestLabelFilteredThreadsUseMatchedMessagesAndStableCursors(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	key := adminKey(t, svc, u)
	inbox, err := svc.Store.GetSystemFolder(ctx, p.AccountID, box.ID, model.FolderRoleInbox)
	if err != nil {
		t.Fatal(err)
	}
	a := seedInbound(t, svc, box, "threads-label-alpha", "<threads-label-alpha@test>", "Alpha", "body")
	b := seedInbound(t, svc, box, "threads-label-bravo", "<threads-label-bravo@test>", "Bravo", "body")
	_ = seedInbound(t, svc, box, "threads-label-other", "<threads-label-other@test>", "Other", "body")
	for _, id := range []string{a.ID, b.ID} {
		if _, err := svc.Store.ReplaceMessageLabels(ctx, p, id, []string{"Selected"}); err != nil {
			t.Fatal(err)
		}
	}
	filters := "label_id=tag:Selected&label_id=" + url.QueryEscape("folder:"+inbox.ID)
	for _, prefix := range []string{
		"/v1/inboxes/" + box.ID + "/threads?",
		"/v1/threads?inbox=" + box.ID + "&",
	} {
		seen := map[string]bool{}
		before := ""
		for pageNo := 0; pageNo < 3; pageNo++ {
			endpoint := prefix + filters + "&limit=1" + before
			res := apiGet(t, h, endpoint, key)
			var page struct {
				Items []struct {
					ID           string `json:"id"`
					MessageCount int    `json:"message_count"`
				} `json:"items"`
				NextCursor string `json:"next_cursor"`
			}
			if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &page) != nil {
				t.Fatalf("%s: %d %s", endpoint, res.Code, res.Body.String())
			}
			if len(page.Items) == 0 {
				break
			}
			if len(page.Items) != 1 || page.Items[0].MessageCount != 1 || seen[page.Items[0].ID] {
				t.Fatalf("filtered thread count, cursor or duplicate: %s", res.Body.String())
			}
			seen[page.Items[0].ID] = true
			if page.NextCursor == "" {
				break
			}
			before = "&before=" + url.QueryEscape(page.NextCursor)
		}
		if len(seen) != 2 || !seen[a.ThreadID] || !seen[b.ThreadID] {
			t.Fatalf("%s: filtered threads = %v", prefix, seen)
		}
	}
	// Account-wide tag filters use the same matching/counting logic without
	// requiring a folder-backed scope, including a resumable merged cursor.
	res := apiGet(t, h, "/v1/threads?label_id=tag:Selected&limit=1", key)
	var first struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		NextCursor string `json:"next_cursor"`
	}
	if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &first) != nil || len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatalf("account-wide first thread page: %d %s", res.Code, res.Body.String())
	}
	next := apiGet(t, h, "/v1/threads?label_id=tag:Selected&limit=1&before="+url.QueryEscape(first.NextCursor), key)
	var second struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if next.Code != 200 || json.Unmarshal(next.Body.Bytes(), &second) != nil || len(second.Items) != 1 || second.Items[0].ID == first.Items[0].ID {
		t.Fatalf("account-wide second thread page: %d %s", next.Code, next.Body.String())
	}
}

func TestIMAPLabelFilteredThreadsAreExclusiveAndPaged(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, fake := standaloneFixture(t)
	fake.folders["Projects"] = 150
	for _, record := range []struct{ folder, mid, subject string }{
		{"INBOX", "<thread-imap-a@test>", "Thread A"},
		{"INBOX", "<thread-imap-b@test>", "Thread B"},
		{"Projects", "<thread-imap-project@test>", "Project Thread"},
	} {
		fake.add(record.folder, "From: a@b.test\r\nSubject: "+record.subject+"\r\nMessage-ID: "+record.mid+"\r\n\r\nbody", record.mid, record.subject)
	}
	key := adminKey(t, svc, u)
	refreshRemote(t, h, box.ID, key)
	cat := apiGet(t, h, "/v1/inboxes/"+box.ID+"/label-catalog", key)
	var catalog struct {
		Items []struct {
			ID, Role, Name string
		} `json:"items"`
	}
	if cat.Code != 200 || json.Unmarshal(cat.Body.Bytes(), &catalog) != nil {
		t.Fatalf("catalog: %d %s", cat.Code, cat.Body.String())
	}
	var inboxLabel, projectLabel string
	for _, item := range catalog.Items {
		if item.Role == model.FolderRoleInbox {
			inboxLabel = item.ID
		}
		if item.Name == "Projects" {
			projectLabel = item.ID
		}
	}
	if inboxLabel == "" || projectLabel == "" {
		t.Fatalf("missing IMAP classification: %s", cat.Body.String())
	}
	for _, test := range []struct {
		label string
		count int
	}{{inboxLabel, 2}, {projectLabel, 1}} {
		seen := map[string]bool{}
		before := ""
		for i := 0; i < test.count; i++ {
			query := "/v1/inboxes/" + box.ID + "/threads?label_id=" + url.QueryEscape(test.label) + "&limit=1" + before
			res := apiGet(t, h, query, key)
			var page struct {
				Items []struct {
					ID           string `json:"id"`
					MessageCount int    `json:"message_count"`
				} `json:"items"`
				NextCursor string `json:"next_cursor"`
			}
			if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].MessageCount != 1 || seen[page.Items[0].ID] {
				t.Fatalf("IMAP thread filter %s: %d %s", query, res.Code, res.Body.String())
			}
			seen[page.Items[0].ID] = true
			if i < test.count-1 {
				if page.NextCursor == "" {
					t.Fatalf("IMAP thread cursor missing: %s", res.Body.String())
				}
				before = "&before=" + url.QueryEscape(page.NextCursor)
			}
		}
		if len(seen) != test.count {
			t.Fatalf("IMAP distinct thread count = %d, want %d", len(seen), test.count)
		}
	}
}
