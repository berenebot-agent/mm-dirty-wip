package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func TestUnifiedLabelsCatalogAndMessageAction(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	folder, err := svc.Store.CreateFolder(context.Background(), p, box.ID, store.FolderCreate{Path: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	m := seedInbound(t, svc, box, "unified-label", "<unified-label@test>", "Organise", "body")
	if _, err := svc.Store.ReplaceMessageLabels(context.Background(), p, m.ID, []string{"Invoices"}); err != nil {
		t.Fatal(err)
	}
	_, key, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "labels test", true, nil)
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
	got := request(http.MethodGet, "/v1/inboxes/"+box.ID+"/label-catalog", "")
	if got.Code != 200 {
		t.Fatalf("catalog %d: %s", got.Code, got.Body.String())
	}
	var envelope struct {
		Items []struct {
			ID         string `json:"id"`
			Membership string `json:"membership"`
			Kind       string `json:"kind"`
		} `json:"items"`
	}
	if err := json.Unmarshal(got.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, item := range envelope.Items {
		seen[item.ID] = item.Membership
	}
	if seen["tag:Invoices"] != "multiple" || seen["folder:"+folder.ID] != "exclusive" {
		t.Fatalf("catalog labels: %#v", seen)
	}
	path := "/v1/inboxes/" + box.ID + "/messages/" + m.ID + "/labels?wait=true"
	got = request(http.MethodPost, path, `{"label_id":"folder:`+folder.ID+`","operation":"remove"}`)
	if got.Code != http.StatusConflict {
		t.Fatalf("remove folder %d: %s", got.Code, got.Body.String())
	}
	got = request(http.MethodPost, path, `{"label_id":"folder:not_this_inbox","operation":"add"}`)
	if got.Code != http.StatusNotFound {
		t.Fatalf("unknown folder %d: %s", got.Code, got.Body.String())
	}
	got = request(http.MethodPost, path, `{"label_id":"tag:Invoices","operation":"remove"}`)
	if got.Code != 200 {
		t.Fatalf("remove tag %d: %s", got.Code, got.Body.String())
	}
	after, err := svc.Store.GetMessage(context.Background(), p, m.ID)
	if err != nil || len(after.Labels) != 0 {
		t.Fatalf("remove tag: %+v %v", after.Labels, err)
	}
}

func TestMoveMessageAPI(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	folder, err := svc.Store.CreateFolder(context.Background(), p, box.ID, store.FolderCreate{Path: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	m := seedInbound(t, svc, box, "move-api", "<move-api@test>", "Move", "body")
	_, key, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "move test", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := func(folderID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/inboxes/"+box.ID+"/messages/"+m.ID+"/move?wait=true", strings.NewReader(`{"folder_id":"`+folderID+`"}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if got := request("missing-folder"); got.Code != http.StatusNotFound {
		t.Fatalf("unknown destination returned %d: %s", got.Code, got.Body.String())
	}
	if got := request(folder.ID); got.Code != http.StatusOK {
		t.Fatalf("move returned %d: %s", got.Code, got.Body.String())
	}
	moved, err := svc.Store.GetMessage(context.Background(), p, m.ID)
	if err != nil || moved.MailboxID != folder.ID {
		t.Fatalf("message folder = %q, want %q (err %v)", moved.MailboxID, folder.ID, err)
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
	body := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID).Body.String()
	if !strings.Contains(body, "NewAdditiveLabel") {
		t.Fatal("empty native label absent from sidebar")
	}
}

func TestUnifiedLabelSidebarAndPicker(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	_, err := svc.Store.CreateFolder(context.Background(), p, box.ID, store.FolderCreate{Path: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	m := seedInbound(t, svc, box, "unified-ui", "<unified-ui@test>", "Organise", "body")
	cookie, _ := uiSession(t, svc, u.ID)
	page := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID).Body.String()
	if strings.Count(page, `class="navgroup labelgroup-head"`) != 1 || !strings.Contains(page, "Projects") {
		t.Fatalf("expected single Labels navigation section")
	}
	msg := uiGet(t, h, cookie, "/ui/messages/"+m.ID).Body.String()
	if !strings.Contains(msg, `name="label" list="mail-label-suggestions"`) || !strings.Contains(msg, `aria-label="Add label"`) {
		t.Fatal("message must use one free-text label control with suggestions")
	}
	if strings.Contains(msg, `<select name="label_id"`) || strings.Contains(msg, `class="move-select"`) {
		t.Fatal("legacy label or move dropdown still rendered")
	}
	if strings.Index(page, `data-folder="trash"`) > strings.Index(page, `class="navgroup labelgroup-head"`) {
		t.Fatal("system destinations should appear above Labels")
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
	if !strings.Contains(page, "Renamed") || !strings.Contains(page, `class="navgroup labelgroup-head"`) {
		t.Fatal("renamed label or compact sidebar missing")
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
