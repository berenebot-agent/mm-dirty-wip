package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/config"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/gmail"
	"github.com/dellarb/mailmoose/tests/support/testdb"
)

// TestProviderLabelAllowsHierarchicalNames proves a provider-owned label name
// with a path separator is accepted, while the local free-text rule is unchanged.
func TestProviderLabelAllowsHierarchicalNames(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	m := seedInbound(t, svc, box, "deep-label", "<deep@test>", "Deep", "body")
	// The native rule rejects a slash; simulate a provider-owned label by marking
	// the action provider-scoped through the model directly.
	if _, ok := model.NormalizeLabel("Projects/2026"); ok {
		t.Fatal("local rule should reject a path separator")
	}
	if _, ok := model.NormalizeProviderLabel("Projects/2026"); !ok {
		t.Fatal("provider rule should accept a path separator")
	}
	if _, ok := model.NormalizeProviderLabel("bad\x00name"); ok {
		t.Fatal("provider rule should reject control characters")
	}
	key := adminKey(t, svc, u)
	// A native inbox still rejects a slash-named label through the API.
	req := httptest.NewRequest(http.MethodPost, "/v1/inboxes/"+box.ID+"/messages/"+m.ID+"/labels?wait=true",
		strings.NewReader(`{"name":"Projects/2026","operation":"add"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("native slash label = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	// A provider rule accepts it in the store/worker path.
	ev, err := svc.Store.ModifyMessageLabel(ctx, p, m.ID, "Projects/2026", "add", true)
	if err != nil || ev == nil {
		t.Fatalf("provider label add: %v", err)
	}
	after, err := svc.Store.GetMessage(ctx, p, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, name := range after.Labels {
		found = found || name == "Projects/2026"
	}
	if !found {
		t.Fatalf("provider label not applied: %v", after.Labels)
	}
}

// TestUnifiedLabelCatalogAcrossBackends proves a raw IMAP mailbox has no
// canonical labels at all: the catalogue is empty, every label capability is
// false, label mutations and label filters are refused, and the legacy local
// tags it may still carry are not canonical labels.
func TestUnifiedLabelCatalogAcrossBackends(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, fake := standaloneFixture(t)
	fake.mu.Lock()
	fake.folders["Projects"] = 150
	fake.mu.Unlock()
	fake.add("INBOX", "From: a@b.test\r\nSubject: Labels\r\nMessage-ID: <labels-imap@test>\r\n\r\nbody", "<labels-imap@test>", "Labels")
	key := adminKey(t, svc, u)
	refreshRemote(t, h, box.ID, key)

	catalogue, raw := fetchLabelCatalogue(t, h, key, box.ID)
	if len(catalogue.Items) != 0 {
		t.Fatalf("an IMAP mailbox has no labels: %+v", catalogue.Items)
	}
	if catalogue.Capabilities.CanCreate || catalogue.Capabilities.CanRename || catalogue.Capabilities.CanDelete {
		t.Fatalf("an IMAP mailbox supports no label operation: %+v", catalogue.Capabilities)
	}
	if catalogue.Capabilities.CreateKind != "" || catalogue.Capabilities.CreateMembership != "" {
		t.Fatalf("an IMAP mailbox declares no label kind: %+v", catalogue.Capabilities)
	}
	if strings.Contains(raw.Body.String(), "Projects") {
		t.Fatalf("an IMAP folder is not a label: %s", raw.Body.String())
	}

	// The inbox capabilities say the same thing.
	inbox := apiGet(t, h, "/v1/inboxes/"+box.ID, key)
	var caps struct {
		Capabilities struct {
			Labels        bool `json:"labels"`
			CreateLabels  bool `json:"create_labels"`
			RenameLabels  bool `json:"rename_labels"`
			DeleteLabels  bool `json:"delete_labels"`
			CreateFolders bool `json:"create_folders"`
			Move          bool `json:"move"`
		} `json:"capabilities"`
	}
	if inbox.Code != http.StatusOK || json.Unmarshal(inbox.Body.Bytes(), &caps) != nil {
		t.Fatalf("inbox: %d %s", inbox.Code, inbox.Body.String())
	}
	if caps.Capabilities.Labels || caps.Capabilities.CreateLabels || caps.Capabilities.RenameLabels || caps.Capabilities.DeleteLabels {
		t.Fatalf("an IMAP mailbox declares no labels: %+v", caps.Capabilities)
	}
	if !caps.Capabilities.CreateFolders || !caps.Capabilities.Move {
		t.Fatalf("an IMAP mailbox owns physical folders: %+v", caps.Capabilities)
	}

	// Every canonical label mutation is refused.
	listed := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages", key)
	var page struct {
		Items []struct {
			ID       string   `json:"id"`
			LabelIDs []string `json:"label_ids"`
			Labels   []string `json:"labels"`
			FolderID *string  `json:"folder_id"`
		} `json:"items"`
	}
	if listed.Code != http.StatusOK || json.Unmarshal(listed.Body.Bytes(), &page) != nil || len(page.Items) != 1 {
		t.Fatalf("IMAP listing: %d %s", listed.Code, listed.Body.String())
	}
	messageID := page.Items[0].ID
	// A legacy local tag stays visible in the legacy view but is not a label.
	if _, err := svc.Store.SetRemoteMessageLabels(context.Background(), u.AccountID, box.ID, messageID, []string{"LegacyTag"}); err != nil {
		t.Fatal(err)
	}
	after := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages", key)
	page.Items = nil
	if after.Code != http.StatusOK || json.Unmarshal(after.Body.Bytes(), &page) != nil || len(page.Items) != 1 {
		t.Fatalf("IMAP listing: %d %s", after.Code, after.Body.String())
	}
	if !stringSliceContains(page.Items[0].Labels, "LegacyTag") {
		t.Fatalf("the legacy tag must be preserved: %v", page.Items[0].Labels)
	}
	if len(page.Items[0].LabelIDs) != 0 {
		t.Fatalf("an IMAP mailbox has no canonical labels: %v", page.Items[0].LabelIDs)
	}
	if page.Items[0].FolderID == nil {
		t.Fatal("an IMAP message still lives in one physical folder")
	}

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	assertUnsupportedOp(t, "apply label", post("/v1/inboxes/"+box.ID+"/messages/"+messageID+"/labels?wait=true", `{"name":"Work","operation":"add"}`))
	assertUnsupportedOp(t, "create label", post("/v1/inboxes/"+box.ID+"/label-catalog", `{"name":"Work"}`))
	// The Beta 2 message PATCH route must no longer write hidden IMAP labels,
	// even when combined with an otherwise valid read-state change.
	for _, body := range []string{`{"labels":["NewTag"]}`, `{"labels":[],"read":true}`} {
		req := httptest.NewRequest(http.MethodPatch, "/v1/messages/"+messageID+"?wait=true", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		assertUnsupportedOp(t, "legacy IMAP PATCH labels", rr)
	}
	preserved := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages", key)
	page.Items = nil
	if preserved.Code != http.StatusOK || json.Unmarshal(preserved.Body.Bytes(), &page) != nil || len(page.Items) != 1 {
		t.Fatalf("IMAP tags after rejected PATCH: %d %s", preserved.Code, preserved.Body.String())
	}
	if !stringSliceContains(page.Items[0].Labels, "LegacyTag") || len(page.Items[0].LabelIDs) != 0 {
		t.Fatalf("legacy IMAP tag changed by rejected PATCH: %+v", page.Items[0])
	}
	rename := httptest.NewRequest(http.MethodPatch, "/v1/inboxes/"+box.ID+"/label-catalog/tag:Work", strings.NewReader(`{"name":"Other"}`))
	rename.Header.Set("Authorization", "Bearer "+key)
	rename.Header.Set("Content-Type", "application/json")
	renameRR := httptest.NewRecorder()
	h.ServeHTTP(renameRR, rename)
	assertUnsupportedOp(t, "rename label", renameRR)
	del := httptest.NewRequest(http.MethodDelete, "/v1/inboxes/"+box.ID+"/label-catalog/tag:Work", nil)
	del.Header.Set("Authorization", "Bearer "+key)
	delRR := httptest.NewRecorder()
	h.ServeHTTP(delRR, del)
	assertUnsupportedOp(t, "delete label", delRR)

	// A label filter is refused too: an IMAP mailbox has no labels to filter.
	for _, path := range []string{
		"/v1/inboxes/" + box.ID + "/messages?label=Work",
		"/v1/inboxes/" + box.ID + "/messages?label_id=" + url.QueryEscape("tag:Work"),
		"/v1/messages?inbox=" + box.ID + "&label=Work",
	} {
		rr := apiGet(t, h, path, key)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("IMAP label filter %s = %d, want 400: %s", path, rr.Code, rr.Body.String())
		}
	}
	// The account-wide label filter must not leak the raw IMAP mailbox: its
	// preserved legacy tags are not canonical labels, so nothing matches them.
	for _, path := range []string{
		"/v1/messages?label_id=" + url.QueryEscape("tag:LegacyTag"),
		"/v1/search?label_id=" + url.QueryEscape("tag:LegacyTag"),
		"/v1/threads?label_id=" + url.QueryEscape("tag:LegacyTag"),
	} {
		rr := apiGet(t, h, path, key)
		if rr.Code != http.StatusOK {
			t.Fatalf("account-wide label filter %s: %d %s", path, rr.Code, rr.Body.String())
		}
		var page struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
			t.Fatalf("account-wide label filter %s: %v %s", path, err, rr.Body.String())
		}
		if len(page.Items) != 0 {
			t.Fatalf("account-wide label filter %s returned %d rows, want none: %s", path, len(page.Items), rr.Body.String())
		}
	}
}

// TestGmailCanonicalFolderAndAdditiveLabels proves a Gmail mailbox projects
// one canonical folder (Trash, Spam, Drafts, Inbox, Sent in that order) and
// keeps its user labels as additive tags.
func TestGmailCanonicalFolderAndAdditiveLabels(t *testing.T) {
	t.Parallel()
	ids := []string{"a", "b", "c"}
	subjects := map[string]string{"a": "Gmail Inbox Message", "b": "Gmail Trash Message", "c": "Gmail Archived Message"}
	memberships := map[string][]string{
		"a": {"INBOX", "SENT", "Label_1"},
		"b": {"TRASH", "INBOX", "Label_1"},
		"c": {"Label_1"},
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/profile"):
			json.NewEncoder(w).Encode(map[string]any{"emailAddress": "gmail@example.net", "historyId": "10"})
		case strings.HasSuffix(r.URL.Path, "/labels"):
			w.Write([]byte(`{"labels":[{"id":"INBOX","name":"Inbox","type":"system"},{"id":"SENT","name":"Sent","type":"system"},{"id":"DRAFT","name":"Drafts","type":"system"},{"id":"TRASH","name":"Trash","type":"system"},{"id":"SPAM","name":"Spam","type":"system"},{"id":"Label_1","name":"Projects","type":"user"}]}`))
		case strings.HasSuffix(r.URL.Path, "/history"):
			json.NewEncoder(w).Encode(map[string]any{"historyId": "10", "history": []any{}})
		case strings.HasSuffix(r.URL.Path, "/messages"):
			page := make([]map[string]any, 0, len(ids))
			for _, id := range ids {
				page = append(page, map[string]any{"id": id, "threadId": "th-" + id})
			}
			json.NewEncoder(w).Encode(map[string]any{"messages": page})
		case strings.Contains(r.URL.Path, "/messages/"):
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/messages/")+len("/messages/"):]
			json.NewEncoder(w).Encode(map[string]any{
				"id": id, "threadId": "th-" + id, "labelIds": memberships[id],
				"internalDate": "1700000000000", "snippet": "preview",
				"payload": map[string]any{"headers": []any{
					map[string]string{"name": "Subject", "value": subjects[id]},
					map[string]string{"name": "From", "value": "Sender <sender@example.net>"},
				}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer fixture.Close()

	st, dir := testdb.OpenDir(t)
	svc, err := app.New(config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901"}, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateAccountAndAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	box, err := svc.Store.CreateStandaloneInbox(context.Background(), u.AccountID, store.StandaloneCreate{Address: "gmail@example.net"})
	if err != nil {
		t.Fatal(err)
	}
	rm := app.NewRemoteMailboxService(svc)
	defer rm.Stop()
	rm.Google = gmail.NewClient(fixture.Client())
	rm.Google.BaseURL = fixture.URL
	if err = rm.SaveGoogleGrant(context.Background(), u.AccountID, box.ID, "client", "secret", gmail.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: gmail.Scope}); err != nil {
		t.Fatal(err)
	}
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	h := settledActionFixture(t, svc, rm, srv.Handler(), u.AccountID)
	key := adminKey(t, svc, u)
	// Index the mailbox first: the catalogue and the folder list are served
	// from the cached provider state.
	if _, err := rm.ReconcileRemote(context.Background(), u.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}

	// The catalogue holds the user label only: no Gmail system label.
	catalogue, raw := fetchLabelCatalogue(t, h, key, box.ID)
	if len(catalogue.Items) != 1 || catalogue.Items[0].ID != "tag:Projects" {
		t.Fatalf("Gmail catalogue: %+v", catalogue.Items)
	}
	if catalogue.Items[0].Kind != "tag" || catalogue.Items[0].Membership != "multiple" {
		t.Fatalf("a Gmail user label is additive: %+v", catalogue.Items[0])
	}
	for _, system := range []string{"INBOX", "SENT", "TRASH", "SPAM", "DRAFT"} {
		if strings.Contains(raw.Body.String(), `"name":"`+system+`"`) {
			t.Fatalf("a Gmail system label must not be a catalogue entry: %s", raw.Body.String())
		}
	}

	// The folder list carries the stable ids a message's folder_id refers to.
	folders := apiGet(t, h, "/v1/inboxes/"+box.ID+"/folders", key)
	var folderPage struct {
		Items []struct {
			ID   string `json:"id"`
			Path string `json:"path"`
			Role string `json:"role"`
		} `json:"items"`
	}
	if folders.Code != http.StatusOK || json.Unmarshal(folders.Body.Bytes(), &folderPage) != nil {
		t.Fatalf("Gmail folders: %d %s", folders.Code, folders.Body.String())
	}
	byPath := map[string]string{}
	for _, f := range folderPage.Items {
		byPath[f.Path] = f.ID
	}
	inboxID, trashID := byPath["INBOX"], byPath["TRASH"]
	if inboxID == "" || trashID == "" {
		t.Fatalf("Gmail system folders missing: %s", folders.Body.String())
	}

	// Each message projects one canonical folder and keeps its user label.
	for _, tc := range []struct {
		view     string
		subject  string
		folderID string
	}{
		{"INBOX", subjects["a"], inboxID},
		{"TRASH", subjects["b"], trashID},
	} {
		rr := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages?folder="+url.QueryEscape(tc.view), key)
		var page struct {
			Items []struct {
				Subject  string   `json:"subject"`
				LabelIDs []string `json:"label_ids"`
				FolderID *string  `json:"folder_id"`
			} `json:"items"`
		}
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &page) != nil {
			t.Fatalf("Gmail %s listing: %d %s", tc.view, rr.Code, rr.Body.String())
		}
		found := false
		for _, item := range page.Items {
			if item.Subject != tc.subject {
				continue
			}
			found = true
			if len(item.LabelIDs) != 1 || item.LabelIDs[0] != "tag:Projects" {
				t.Fatalf("%s label_ids = %v, want only the user label", tc.subject, item.LabelIDs)
			}
			if item.FolderID == nil || *item.FolderID != tc.folderID {
				t.Fatalf("%s folder_id = %v, want %s", tc.subject, item.FolderID, tc.folderID)
			}
		}
		if !found {
			t.Fatalf("%s not listed in the %s view: %s", tc.subject, tc.view, rr.Body.String())
		}
	}
	// A message that carries no system label has no canonical folder, so it
	// belongs to no folder view at all.
	for _, view := range []string{"INBOX", "SENT", "TRASH"} {
		rr := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages?folder="+url.QueryEscape(view), key)
		var page struct {
			Items []struct {
				Subject string `json:"subject"`
			} `json:"items"`
		}
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &page) != nil {
			t.Fatalf("Gmail %s listing: %d %s", view, rr.Code, rr.Body.String())
		}
		for _, item := range page.Items {
			if item.Subject == subjects["c"] {
				t.Fatalf("%s must not be filed in the %s view", subjects["c"], view)
			}
		}
	}
}
