package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestUnifiedLabelsCatalogAndMessageAction proves the catalogue holds additive
// tags only and that a label action is additive: it never files a message and
// a folder id is not a label.
func TestUnifiedLabelsCatalogAndMessageAction(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	folder, err := svc.Store.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	m := seedInbound(t, svc, box, "unified-label", "<unified-label@test>", "Organise", "body")
	if _, err := svc.Store.ReplaceMessageLabels(ctx, p, m.ID, []string{"Invoices"}); err != nil {
		t.Fatal(err)
	}
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "labels test", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	catalogue, _ := fetchLabelCatalogue(t, h, key, box.ID)
	seen := map[string]string{}
	for _, item := range catalogue.Items {
		seen[item.ID] = item.Membership
	}
	if seen["tag:Invoices"] != "multiple" {
		t.Fatalf("catalog labels: %#v", seen)
	}
	if _, ok := seen["folder:"+folder.ID]; ok {
		t.Fatalf("a folder must not be a catalogue label: %#v", seen)
	}
	for id := range seen {
		if !strings.HasPrefix(id, "tag:") {
			t.Fatalf("catalogue holds additive tags only: %#v", seen)
		}
	}
	path := "/v1/inboxes/" + box.ID + "/messages/" + m.ID + "/labels?wait=true"
	// A folder id is not a label: it is refused and never files the message.
	for _, labelID := range []string{"folder:" + folder.ID, folder.ID} {
		got := request(http.MethodPost, path, `{"label_id":"`+labelID+`","operation":"add"}`)
		if got.Code != http.StatusBadRequest && got.Code != http.StatusNotFound && got.Code != http.StatusConflict {
			t.Fatalf("folder as label %q: %d %s", labelID, got.Code, got.Body.String())
		}
	}
	// An unknown folder id is not a label either.
	got := request(http.MethodPost, path, `{"label_id":"folder:fld_missing","operation":"add"}`)
	if got.Code != http.StatusNotFound {
		t.Fatalf("unknown folder as label = %d, want 404: %s", got.Code, got.Body.String())
	}
	after := fetchMessageState(t, h, key, m.ID)
	if after.FolderID != nil && *after.FolderID == folder.ID {
		t.Fatal("a label action must never file a message")
	}
	// The system Inbox is a location, not a label: it is not in the catalogue.
	inboxFolder, err := svc.Store.GetSystemFolder(ctx, p.AccountID, box.ID, model.FolderRoleInbox)
	if err != nil {
		t.Fatal(err)
	}
	got = request(http.MethodPost, path, `{"label_id":"folder:`+inboxFolder.ID+`","operation":"add"}`)
	if got.Code == http.StatusOK {
		t.Fatalf("the system Inbox must not be applicable as a label: %s", got.Body.String())
	}
	// An additive tag applies and removes without touching the folder.
	got = request(http.MethodPost, path, `{"label_id":"tag:Invoices","operation":"remove"}`)
	if got.Code != 200 {
		t.Fatalf("remove tag %d: %s", got.Code, got.Body.String())
	}
	removed := fetchMessageState(t, h, key, m.ID)
	if len(removed.LabelIDs) != 0 {
		t.Fatalf("label_ids after remove: %v", removed.LabelIDs)
	}
	got = request(http.MethodPost, path, `{"label_id":"tag:Invoices","operation":"add"}`)
	if got.Code != 200 {
		t.Fatalf("add tag %d: %s", got.Code, got.Body.String())
	}
	readded := fetchMessageState(t, h, key, m.ID)
	if len(readded.LabelIDs) != 1 || readded.LabelIDs[0] != "tag:Invoices" {
		t.Fatalf("label_ids after add: %v", readded.LabelIDs)
	}
	if readded.FolderID == nil || *readded.FolderID != inboxFolder.ID {
		t.Fatalf("a label must not change the folder: %v", readded.FolderID)
	}
	// An unknown label id is refused.
	got = request(http.MethodPost, path, `{"label_id":"tag:Missing","operation":"remove"}`)
	if got.Code != http.StatusNotFound && got.Code != http.StatusBadRequest {
		t.Fatalf("unknown label %d: %s", got.Code, got.Body.String())
	}
}

// TestMoveMessageAPI proves placement is a move, addressed by the stable folder
// id, and that a move keeps the message's labels.
func TestMoveMessageAPI(t *testing.T) {
	t.Parallel()
	svc, h, u, domain, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	m := seedInbound(t, svc, box, "move-api", "<move-api@test>", "Move", "body")
	if _, err := svc.Store.ReplaceMessageLabels(ctx, p, m.ID, []string{"Invoices"}); err != nil {
		t.Fatal(err)
	}
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "move test", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	inboxFolder, err := svc.Store.GetSystemFolder(ctx, p.AccountID, box.ID, model.FolderRoleInbox)
	if err != nil {
		t.Fatal(err)
	}
	request := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/inboxes/"+box.ID+"/messages/"+m.ID+"/move?wait=true", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if got := request(`{"folder_id":"missing-folder"}`); got.Code != http.StatusNotFound {
		t.Fatalf("unknown destination returned %d: %s", got.Code, got.Body.String())
	}
	// A folder id from another inbox is refused.
	otherBox, err := svc.Store.CreateInbox(ctx, u.AccountID, domain.ID, "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	otherInbox, err := svc.Store.GetSystemFolder(ctx, u.AccountID, otherBox.ID, model.FolderRoleInbox)
	if err != nil {
		t.Fatal(err)
	}
	if got := request(`{"folder_id":"` + otherInbox.ID + `"}`); got.Code != http.StatusNotFound {
		t.Fatalf("cross-inbox destination returned %d: %s", got.Code, got.Body.String())
	}
	// Sent is never a move destination: a sent copy comes from sending.
	sentFolder, err := svc.Store.GetSystemFolder(ctx, u.AccountID, box.ID, model.FolderRoleSent)
	if err != nil {
		t.Fatal(err)
	}
	if got := request(`{"folder_id":"` + sentFolder.ID + `"}`); got.Code != http.StatusBadRequest && got.Code != http.StatusConflict {
		t.Fatalf("move to Sent returned %d, want a refusal: %s", got.Code, got.Body.String())
	}
	// Moving into the system Inbox files the message there and keeps its labels.
	if got := request(`{"folder_id":"` + inboxFolder.ID + `"}`); got.Code != http.StatusOK {
		t.Fatalf("move returned %d: %s", got.Code, got.Body.String())
	}
	moved, err := svc.Store.GetMessage(ctx, p, m.ID)
	if err != nil || moved.MailboxID != inboxFolder.ID {
		t.Fatalf("message folder = %q, want %q (err %v)", moved.MailboxID, inboxFolder.ID, err)
	}
	state := fetchMessageState(t, h, key, m.ID)
	if len(state.LabelIDs) != 1 || state.LabelIDs[0] != "tag:Invoices" {
		t.Fatalf("a move must keep the labels: %v", state.LabelIDs)
	}
	if state.FolderID == nil || *state.FolderID != inboxFolder.ID {
		t.Fatalf("folder_id after move = %v, want %s", state.FolderID, inboxFolder.ID)
	}
	// A null folder id clears the canonical folder: the message leaves the Inbox.
	if got := request(`{"folder_id":null}`); got.Code != http.StatusOK {
		t.Fatalf("archive returned %d: %s", got.Code, got.Body.String())
	}
	archived := fetchMessageState(t, h, key, m.ID)
	if archived.FolderID != nil {
		t.Fatalf("folder_id after archive = %v, want null", *archived.FolderID)
	}
	if len(archived.LabelIDs) != 1 || archived.LabelIDs[0] != "tag:Invoices" {
		t.Fatalf("archiving must keep the labels: %v", archived.LabelIDs)
	}
	listed := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages?folder="+url.QueryEscape(inboxFolder.ID), key)
	var page struct {
		Items []struct{ ID string } `json:"items"`
	}
	if listed.Code != http.StatusOK || json.Unmarshal(listed.Body.Bytes(), &page) != nil {
		t.Fatalf("inbox listing: %d %s", listed.Code, listed.Body.String())
	}
	for _, item := range page.Items {
		if item.ID == m.ID {
			t.Fatal("an archived message must not stay in the Inbox")
		}
	}
}

func TestReaderLabelActionPreservesReader(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	m := seedInbound(t, svc, box, "reader-label", "<reader-label@test>", "Reader", "body")
	cookie, csrf := uiSession(t, svc, u.ID)
	response := uiPost(t, h, cookie, "/ui/messages/"+m.ID+"/labels",
		"_csrf="+csrf+"&action=add&label=Projects")
	if response.Code != http.StatusSeeOther {
		t.Fatalf("label action returned %d: %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Location"); !strings.HasPrefix(got, "/ui/messages/"+m.ID+"?notice=") {
		t.Fatalf("label action navigated away from reader: %q", got)
	}
}

func TestNativeNewLabelIsAdditive(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	rr := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/labels", "_csrf="+csrf+"&name=NewAdditiveLabel")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create label: %d %s", rr.Code, rr.Body.String())
	}
	labels, err := svc.Store.ListDefinedLabels(context.Background(), u.AccountID, box.ID)
	if err != nil || len(labels) != 1 || labels[0] != "NewAdditiveLabel" {
		t.Fatalf("defined labels: %v %v", labels, err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	if _, err := svc.Store.ManageDefinedLabel(context.Background(), p, box.ID, "NewAdditiveLabel", "NewAdditiveLabel", "rename"); err != nil {
		t.Fatalf("same-name rename: %v", err)
	}
	labels, err = svc.Store.ListDefinedLabels(context.Background(), u.AccountID, box.ID)
	if err != nil || len(labels) != 1 || labels[0] != "NewAdditiveLabel" {
		t.Fatalf("same-name rename deleted catalogue row: %v %v", labels, err)
	}
	folders, err := svc.Store.ListFolders(context.Background(), u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range folders {
		if f.Name == "NewAdditiveLabel" {
			t.Fatal("a native label must not create an exclusive folder")
		}
	}
	// The catalogue publishes it as an additive tag.
	catalogue, _ := fetchLabelCatalogue(t, h, adminKey(t, svc, u), box.ID)
	found := false
	for _, item := range catalogue.Items {
		if item.ID == "tag:NewAdditiveLabel" && item.Kind == "tag" && item.Membership == "multiple" {
			found = true
		}
	}
	if !found {
		t.Fatalf("created label missing from the catalogue: %+v", catalogue.Items)
	}
	body := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID).Body.String()
	if !strings.Contains(body, "NewAdditiveLabel") {
		t.Fatal("empty native label absent from sidebar")
	}
}

// TestUnifiedLabelSidebarAndPicker proves the sidebar separates the two
// classifications: a domain mailbox lists labels, a raw IMAP mailbox lists
// physical folders, and neither lists the other's entries.
func TestUnifiedLabelSidebarAndPicker(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	m := seedInbound(t, svc, box, "unified-ui", "<unified-ui@test>", "Organise", "body")
	if _, err := svc.Store.ReplaceMessageLabels(context.Background(), p, m.ID, []string{"Work"}); err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)
	page := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID).Body.String()
	if !strings.Contains(page, `<span>Labels</span>`) {
		t.Fatal("a domain mailbox must list its labels")
	}
	if !strings.Contains(page, `<span>Folders</span>`) {
		t.Fatal("the system views must be grouped under Folders")
	}
	if strings.Contains(page, `<span>Custom folders</span>`) {
		t.Fatal("a domain mailbox owns no physical custom folders")
	}
	if !strings.Contains(page, `data-label="Work"`) {
		t.Fatal("the label must be listed as a label")
	}
	if strings.Contains(page, `data-label="Inbox"`) {
		t.Fatal("the system Inbox is a view, not a label")
	}
	msg := uiGet(t, h, cookie, "/ui/messages/"+m.ID).Body.String()
	if !strings.Contains(msg, `name="label" list="mail-label-suggestions"`) || !strings.Contains(msg, `aria-label="Add label"`) {
		t.Fatal("message must use one free-text label control with suggestions")
	}
	if strings.Contains(msg, `<select name="label_id"`) || strings.Contains(msg, `class="move-select"`) {
		t.Fatal("legacy label or move dropdown still rendered")
	}
}

// TestIMAPSidebarListsFoldersNotLabels proves a raw IMAP mailbox has physical
// folders and no additive labels, and the sidebar says so.
func TestIMAPSidebarListsFoldersNotLabels(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, fake := standaloneFixture(t)
	fake.mu.Lock()
	fake.folders["Projects"] = 150
	fake.mu.Unlock()
	key := adminKey(t, svc, u)
	refreshRemote(t, h, box.ID, key)
	cookie, _ := uiSession(t, svc, u.ID)
	page := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID).Body.String()
	if !strings.Contains(page, `<span>Folders</span>`) {
		t.Fatal("the system views must be grouped under Folders")
	}
	if !strings.Contains(page, `<span>Custom folders</span>`) {
		t.Fatal("an IMAP mailbox must group its physical folders")
	}
	if strings.Contains(page, `<span>Labels</span>`) {
		t.Fatal("an IMAP mailbox has no additive labels to list")
	}
	if !strings.Contains(page, "Projects") {
		t.Fatal("the IMAP folder must be listed")
	}
}

func TestNativeLabelActionsComposeWithoutLostUpdates(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	m := seedInbound(t, svc, box, "lbl-race-add", "<lbl-race-add@test>", "Queue labels", "body")
	_, key, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "label delta", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Alpha", "Beta"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/inboxes/"+box.ID+"/messages/"+m.ID+"/labels?wait=true",
			strings.NewReader(`{"name":"`+name+`","operation":"add"}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("add %q: %d %s", name, w.Code, w.Body.String())
		}
	}
	after, err := svc.Store.GetMessage(context.Background(), p, m.ID)
	if err != nil || len(after.Labels) != 2 {
		t.Fatalf("label additions lost: %+v %v", after.Labels, err)
	}
	state := fetchMessageState(t, h, key, m.ID)
	if len(state.LabelIDs) != 2 {
		t.Fatalf("label_ids = %v, want both tags", state.LabelIDs)
	}
}

func TestNativeLabelRenameDeleteAndSidebar(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	msg := seedInbound(t, svc, box, "label-manage", "<label-manage@test>", "Manage", "body")
	if _, err := svc.Store.ReplaceMessageLabels(context.Background(), p, msg.ID, []string{"Old"}); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)
	rename := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/labels/tag:Old/rename", "_csrf="+csrf+"&name=Renamed")
	if rename.Code != http.StatusSeeOther {
		t.Fatalf("rename: %d %s", rename.Code, rename.Body.String())
	}
	after, err := svc.Store.GetMessage(context.Background(), p, msg.ID)
	if err != nil || len(after.Labels) != 1 || after.Labels[0] != "Renamed" {
		t.Fatalf("rename labels %+v %v", after.Labels, err)
	}
	page := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID).Body.String()
	if !strings.Contains(page, "Renamed") || !strings.Contains(page, `<span>Labels</span>`) {
		t.Fatal("renamed label or label section missing")
	}
	del := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/labels/tag:Renamed/delete", "_csrf="+csrf)
	if del.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d %s", del.Code, del.Body.String())
	}
	after, err = svc.Store.GetMessage(context.Background(), p, msg.ID)
	if err != nil || len(after.Labels) != 0 {
		t.Fatalf("delete labels %+v %v", after.Labels, err)
	}
}
