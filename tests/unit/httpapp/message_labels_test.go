package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"gatehouse-mail/internal/model"
)

func TestAPIMessageLabelsPatch(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	m := seedInbound(t, svc, box, "api-lbl-1", "<api-lbl-1@test>", "Label me", "body")
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("PATCH", "/v1/messages/"+m.ID, strings.NewReader(`{"labels":["Invoices","Unpaid"]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("patch %d %s", rr.Code, rr.Body.String())
	}
	var patched model.Message
	if err = json.Unmarshal(rr.Body.Bytes(), &patched); err != nil {
		t.Fatal(err)
	}
	if len(patched.Labels) != 2 || patched.Labels[0] != "Invoices" || patched.Labels[1] != "Unpaid" {
		t.Fatalf("patched labels %#v", patched.Labels)
	}

	req = httptest.NewRequest("GET", "/v1/messages/"+m.ID, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"labels":["Invoices","Unpaid"]`) {
		t.Fatalf("get %d %s", rr.Code, rr.Body.String())
	}

	// An empty array clears the set; omitempty then drops the field.
	req = httptest.NewRequest("PATCH", "/v1/messages/"+m.ID, strings.NewReader(`{"labels":[]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || strings.Contains(rr.Body.String(), `"labels"`) {
		t.Fatalf("clear %d %s", rr.Code, rr.Body.String())
	}
}

func TestAPIMessagesLabelFilter(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	both := seedInbound(t, svc, box, "api-flt-both", "<api-flt-both@test>", "Both labels", "body")
	one := seedInbound(t, svc, box, "api-flt-one", "<api-flt-one@test>", "One label", "body")
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	if _, err := svc.Store.ReplaceMessageLabels(ctx, p, both.ID, []string{"Alpha", "Beta"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.ReplaceMessageLabels(ctx, p, one.ID, []string{"Alpha"}); err != nil {
		t.Fatal(err)
	}
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/v1/messages?inbox="+box.ID+"&label=Alpha&label=Beta", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("filter %d %s", rr.Code, rr.Body.String())
	}
	var msgs []model.Message
	if err = json.Unmarshal(rr.Body.Bytes(), &msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].ID != both.ID {
		t.Fatalf("AND filter %#v", msgs)
	}
}

func TestAPILabels(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	m := seedInbound(t, svc, box, "api-list-1", "<api-list-1@test>", "List", "body")
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	if _, err := svc.Store.ReplaceMessageLabels(ctx, p, m.ID, []string{"Invoices"}); err != nil {
		t.Fatal(err)
	}
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/v1/labels", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("labels %d %s", rr.Code, rr.Body.String())
	}
	var labels []string
	if err = json.Unmarshal(rr.Body.Bytes(), &labels); err != nil {
		t.Fatal(err)
	}
	if len(labels) != 1 || labels[0] != "Invoices" {
		t.Fatalf("labels %#v", labels)
	}
}

func TestUIMessageLabelsAddRemove(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	m := seedInbound(t, svc, box, "ui-lbl-1", "<ui-lbl-1@test>", "Tagged", "body")
	cookie, csrf := uiSession(t, svc, u.ID)

	post := func(action, label string) *httptest.ResponseRecorder {
		form := "action=" + action + "&label=" + label + "&_csrf=" + csrf
		req := httptest.NewRequest("POST", "/ui/messages/"+m.ID+"/labels", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := post("add", "Work"); rr.Code != 303 {
		t.Fatalf("add %d: %s", rr.Code, rr.Body.String())
	}
	got, _ := svc.Store.GetMessageByID(context.Background(), u.AccountID, m.ID)
	if len(got.Labels) != 1 || got.Labels[0] != "Work" {
		t.Fatalf("after add %#v", got.Labels)
	}

	// The message page renders the pill and the add/remove controls.
	req := httptest.NewRequest("GET", "/ui/messages/"+m.ID, nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	page := rr.Body.String()
	if !strings.Contains(page, "labelpill") || !strings.Contains(page, "Work") {
		t.Fatalf("message page missing label pill: %s", page)
	}
	for _, want := range []string{`aria-label="Add label"`, `aria-label="Remove label"`, `class="labelx"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("message page missing %q", want)
		}
	}

	if rr = post("remove", "work"); rr.Code != 303 {
		t.Fatalf("remove %d: %s", rr.Code, rr.Body.String())
	}
	got, _ = svc.Store.GetMessageByID(context.Background(), u.AccountID, m.ID)
	if len(got.Labels) != 0 {
		t.Fatalf("after remove %#v", got.Labels)
	}
}

func TestUIInboxShowsLabelPills(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	tagged := seedInbound(t, svc, box, "ui-flt-tagged", "<ui-flt-tagged@test>", "Tagged subject", "body")
	seedInbound(t, svc, box, "ui-flt-plain", "<ui-flt-plain@test>", "Plain subject", "body")
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	if _, err := svc.Store.ReplaceMessageLabels(ctx, p, tagged.ID, []string{"Work"}); err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)

	req := httptest.NewRequest("GET", "/ui/inboxes/"+box.ID, nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("inbox %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Tagged subject") || !strings.Contains(body, "Plain subject") {
		t.Fatalf("inbox missing messages: %s", body)
	}
	if !strings.Contains(body, `class="labelpill"`) || !strings.Contains(body, "Work") {
		t.Fatalf("inbox missing label pill: %s", body)
	}
	// Labeling lives on the message view only: no toolbar label controls.
	if strings.Contains(body, `name="label"`) || strings.Contains(body, "Add label") || strings.Contains(body, `class="labelbar"`) {
		t.Fatalf("inbox should not offer bulk labeling: %s", body)
	}
}
