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

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

// systemFolderIDs returns the stable ids of an inbox's system folders, by role.
func systemFolderIDs(t *testing.T, h http.Handler, key, inboxID string) map[string]string {
	t.Helper()
	rr := apiGet(t, h, "/v1/inboxes/"+inboxID+"/folders", key)
	var page struct {
		Items []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"items"`
	}
	if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &page) != nil {
		t.Fatalf("folders: %d %s", rr.Code, rr.Body.String())
	}
	out := map[string]string{}
	for _, f := range page.Items {
		out[f.Role] = f.ID
	}
	return out
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestSystemLabelsLocalClassification proves a domain mailbox's system
// classifications are locations, not labels: the catalogue holds additive tags
// only, the folder endpoint carries the system folders, a system folder id is
// refused as a label, and the reader shows no Inbox label pill.
func TestSystemLabelsLocalClassification(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	key := adminKey(t, svc, u)

	// The catalogue holds additive tags only: no system classification.
	catalogue, raw := fetchLabelCatalogue(t, h, key, box.ID)
	for _, item := range catalogue.Items {
		if item.Kind != "tag" || item.Membership != "multiple" {
			t.Fatalf("catalogue entry must be an additive tag: %+v", item)
		}
	}
	if strings.Contains(raw.Body.String(), `"role"`) {
		t.Fatalf("the catalogue must not carry system classifications: %s", raw.Body.String())
	}

	// The system folders are discovered through the folder endpoint.
	folders := systemFolderIDs(t, h, key, box.ID)
	for _, role := range []string{model.FolderRoleInbox, model.FolderRoleSent, model.FolderRoleSpam, model.FolderRoleTrash} {
		if folders[role] == "" {
			t.Fatalf("missing system folder %s", role)
		}
	}

	m := seedInbound(t, svc, box, "system-labels", "<system-labels@test>", "Classification", "body")
	if _, err := svc.Store.ReplaceMessageLabels(ctx, p, m.ID, []string{"Alpha", "Beta"}); err != nil {
		t.Fatal(err)
	}
	apply := func(id, op string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/inboxes/"+box.ID+"/messages/"+m.ID+"/labels?wait=true", strings.NewReader(`{"label_id":"`+id+`","operation":"`+op+`"}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	// A system folder id is not a label: it is refused, and it never files the
	// message.
	for _, role := range []string{model.FolderRoleInbox, model.FolderRoleSent, model.FolderRoleSpam, model.FolderRoleTrash} {
		got := apply(folders[role], "add")
		if got.Code != http.StatusBadRequest && got.Code != http.StatusNotFound && got.Code != http.StatusConflict {
			t.Fatalf("system folder %s as label = %d: %s", role, got.Code, got.Body.String())
		}
	}
	state := fetchMessageState(t, h, key, m.ID)
	if len(state.LabelIDs) != 2 {
		t.Fatalf("label_ids = %v, want the two additive tags", state.LabelIDs)
	}
	for _, id := range state.LabelIDs {
		if !strings.HasPrefix(id, "tag:") {
			t.Fatalf("a folder is never a label: %v", state.LabelIDs)
		}
	}
	if state.FolderID == nil || *state.FolderID != folders[model.FolderRoleInbox] {
		t.Fatalf("folder_id = %v, want the system Inbox %s", state.FolderID, folders[model.FolderRoleInbox])
	}
	// The reader shows the additive labels, never an Inbox pill.
	cookie, _ := uiSession(t, svc, u.ID)
	page := uiGet(t, h, cookie, "/ui/messages/"+m.ID).Body.String()
	if strings.Contains(page, `class="labelpill">Inbox`) {
		t.Fatalf("the system Inbox must not be a label pill: %s", page)
	}
	for _, want := range []string{"Alpha", "Beta"} {
		if !strings.Contains(page, want) {
			t.Fatalf("the additive label %s is missing from the reader: %s", want, page)
		}
	}
}

// TestSystemClassificationEventsAreDurable proves the spam, trash and
// restore transitions still publish their durable events, so a client that
// follows the event log sees the same state the API reports.
func TestSystemClassificationEventsAreDurable(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	key := adminKey(t, svc, u)
	m := seedInbound(t, svc, box, "system-events", "<system-events@test>", "Events", "body")

	call := func(method, path, body string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code < 200 || w.Code > 299 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
	}
	call(http.MethodPatch, "/v1/messages/"+m.ID, `{"spam":true}`)
	call(http.MethodPatch, "/v1/messages/"+m.ID, `{"spam":false}`)
	call(http.MethodDelete, "/v1/messages/"+m.ID, "")
	call(http.MethodPost, "/v1/messages/"+m.ID+"/restore", "")

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
}

// TestSystemLabelsIMAPHasNoLabels proves a raw IMAP mailbox has no canonical
// labels: the catalogue is empty, label_ids is empty, a label mutation is
// refused, and the message keeps its one physical folder.
func TestSystemLabelsIMAPHasNoLabels(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, fake := standaloneFixture(t)
	fake.mu.Lock()
	fake.folders["Spam"] = 100
	fake.folders["Trash"] = 100
	fake.mu.Unlock()
	fake.add("INBOX", "From: a@b.test\r\nSubject: Labels\r\nMessage-ID: <labels-imap@test>\r\n\r\nbody", "<labels-imap@test>", "Labels")
	key := adminKey(t, svc, u)
	seed := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages", key)
	if seed.Code != 200 {
		t.Fatalf("seed: %d %s", seed.Code, seed.Body.String())
	}
	var page struct {
		Items []struct {
			ID       string   `json:"id"`
			LabelIDs []string `json:"label_ids"`
			FolderID *string  `json:"folder_id"`
		} `json:"items"`
	}
	if json.Unmarshal(seed.Body.Bytes(), &page) != nil || len(page.Items) != 1 {
		t.Fatalf("IMAP listing: %s", seed.Body.String())
	}
	messageID := page.Items[0].ID
	if len(page.Items[0].LabelIDs) != 0 {
		t.Fatalf("an IMAP mailbox has no canonical labels: %v", page.Items[0].LabelIDs)
	}
	if page.Items[0].FolderID == nil {
		t.Fatal("an IMAP message lives in exactly one physical folder")
	}
	// A label mutation is refused and never moves the message.
	req := httptest.NewRequest(http.MethodPost, "/v1/inboxes/"+box.ID+"/messages/"+messageID+"/labels?wait=true", strings.NewReader(`{"name":"Work","operation":"add"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assertUnsupportedOp(t, "IMAP label mutation", w)
	if got := fake.folderOf("<labels-imap@test>"); got != "INBOX" {
		t.Fatalf("a refused label must not move the message: %s", got)
	}
	// A label filter is refused too: there is no label to filter on.
	for _, path := range []string{
		"/v1/inboxes/" + box.ID + "/messages?label=Work",
		"/v1/inboxes/" + box.ID + "/threads?label=Work",
	} {
		rr := apiGet(t, h, path, key)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("IMAP label filter %s = %d, want 400: %s", path, rr.Code, rr.Body.String())
		}
	}
}

// TestSentFolderExcludesPendingAndFailedOutbound proves the Sent folder is a
// location: only a completed send is filed there, and no system folder is ever
// a label membership.
func TestSentFolderExcludesPendingAndFailedOutbound(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	key := adminKey(t, svc, u)
	newOutbound := func(subject string) model.Message {
		t.Helper()
		m, _, err := svc.Store.CommitOutbound(ctx, store.OutboundRecord{
			Inbox: box, Provider: "smtp",
			RFCMessageID: "<" + subject + "@example.test>",
			From:         model.Address{Address: box.Address},
			To:           []string{"recipient@example.test"}, Subject: subject, Text: subject,
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
	folders := systemFolderIDs(t, h, key, box.ID)
	sentFolder := folders[model.FolderRoleSent]
	if sentFolder == "" {
		t.Fatal("Sent folder not advertised")
	}
	// Only the completed send is filed in Sent, and no message carries a system
	// folder as a label.
	for _, tc := range []struct {
		id      string
		hasSent bool
	}{
		{pending.ID, false}, {failed.ID, false}, {sent.ID, true},
	} {
		state := fetchMessageState(t, h, key, tc.id)
		hasSent := state.FolderID != nil && *state.FolderID == sentFolder
		if hasSent != tc.hasSent {
			t.Fatalf("%s folder_id = %v, want Sent=%v", tc.id, state.FolderID, tc.hasSent)
		}
		for _, id := range state.LabelIDs {
			if !strings.HasPrefix(id, "tag:") {
				t.Fatalf("%s carries a system membership %q", tc.id, id)
			}
		}
	}
	// The Sent folder is a filter scope like any other folder.
	for _, path := range []string{
		"/v1/inboxes/" + box.ID + "/messages?folder=",
		"/v1/messages?inbox=" + box.ID + "&folder=",
		"/v1/inboxes/" + box.ID + "/search?folder=",
		"/v1/inboxes/" + box.ID + "/threads?folder=",
	} {
		res := apiGet(t, h, path+url.QueryEscape(sentFolder), key)
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

// TestLabelFilteredThreadsUseMatchedMessagesAndStableCursors proves a thread
// filter combines the folder scope with the additive label filter, and that
// the cursor stays stable across pages.
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
	filters := "label_id=tag:Selected&folder=" + url.QueryEscape(inbox.ID)
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

// TestIMAPThreadsAreFilteredByFolder proves a raw IMAP mailbox filters its
// threads by physical folder, with stable cursors.
func TestIMAPThreadsAreFilteredByFolder(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, fake := standaloneFixture(t)
	fake.mu.Lock()
	fake.folders["Projects"] = 150
	fake.mu.Unlock()
	for _, record := range []struct{ folder, mid, subject string }{
		{"INBOX", "<thread-imap-a@test>", "Thread A"},
		{"INBOX", "<thread-imap-b@test>", "Thread B"},
		{"Projects", "<thread-imap-project@test>", "Project Thread"},
	} {
		fake.add(record.folder, "From: a@b.test\r\nSubject: "+record.subject+"\r\nMessage-ID: "+record.mid+"\r\n\r\nbody", record.mid, record.subject)
	}
	key := adminKey(t, svc, u)
	refreshRemote(t, h, box.ID, key)
	for _, test := range []struct {
		folder string
		count  int
	}{{"INBOX", 2}, {"Projects", 1}} {
		seen := map[string]bool{}
		before := ""
		for i := 0; i < test.count; i++ {
			query := "/v1/inboxes/" + box.ID + "/threads?folder=" + url.QueryEscape(test.folder) + "&limit=1" + before
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
