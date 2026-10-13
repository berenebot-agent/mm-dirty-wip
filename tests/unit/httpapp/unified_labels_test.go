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
	if err != nil { t.Fatal(err) }
	m := seedInbound(t, svc, box, "unified-label", "<unified-label@test>", "Organise", "body")
	if _, err := svc.Store.ReplaceMessageLabels(context.Background(), p, m.ID, []string{"Invoices"}); err != nil { t.Fatal(err) }
	_, key, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "labels test", true, nil)
	if err != nil { t.Fatal(err) }
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	got := request(http.MethodGet, "/v1/inboxes/"+box.ID+"/label-catalog", "")
	if got.Code != 200 { t.Fatalf("catalog %d: %s", got.Code, got.Body.String()) }
	var envelope struct { Items []struct {
		ID string `json:"id"`
		Membership string `json:"membership"`
		Kind string `json:"kind"`
	} `json:"items"` }
	if err := json.Unmarshal(got.Body.Bytes(), &envelope); err != nil { t.Fatal(err) }
	seen := map[string]string{}
	for _, item := range envelope.Items { seen[item.ID] = item.Membership }
	if seen["tag:Invoices"] != "multiple" || seen["folder:"+folder.ID] != "exclusive" {
		t.Fatalf("catalog labels: %#v", seen)
	}
	path := "/v1/inboxes/"+box.ID+"/messages/"+m.ID+"/labels?wait=true"
	got = request(http.MethodPost, path, `{"label_id":"folder:`+folder.ID+`","operation":"remove"}`)
	if got.Code != http.StatusConflict { t.Fatalf("remove folder %d: %s", got.Code, got.Body.String()) }
	got = request(http.MethodPost, path, `{"label_id":"folder:not_this_inbox","operation":"add"}`)
	if got.Code != http.StatusNotFound { t.Fatalf("unknown folder %d: %s", got.Code, got.Body.String()) }
	got = request(http.MethodPost, path, `{"label_id":"tag:Invoices","operation":"remove"}`)
	if got.Code != 200 { t.Fatalf("remove tag %d: %s", got.Code, got.Body.String()) }
	after, err := svc.Store.GetMessage(context.Background(), p, m.ID)
	if err != nil || len(after.Labels) != 0 { t.Fatalf("remove tag: %+v %v", after.Labels, err) }
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
	if err != nil { t.Fatal(err) }
	m := seedInbound(t, svc, box, "unified-ui", "<unified-ui@test>", "Organise", "body")
	cookie, _ := uiSession(t, svc, u.ID)
	page := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID).Body.String()
	if strings.Count(page, `class="navgroup">Labels`) != 1 || !strings.Contains(page, "Projects") {
		t.Fatalf("expected single Labels navigation section")
	}
	msg := uiGet(t, h, cookie, "/ui/messages/"+m.ID).Body.String()
	if !strings.Contains(msg, `aria-label="Apply label"`) || !strings.Contains(msg, "(moves message)") {
		t.Fatal("message selector must disclose folder-backed move semantics")
	}
}
