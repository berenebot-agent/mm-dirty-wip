package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/apispec"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestUnifiedLabelMembershipOnMessages proves Message.label_ids is the single
// membership projection: it carries additive tag names and folder-backed labels
// on every inbox kind, and equivalent routes agree.
func TestUnifiedLabelMembershipOnMessages(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	folder, err := svc.Store.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	m := seedInbound(t, svc, box, "label-ids", "<label-ids@test>", "Membership", "body")
	if _, err := svc.Store.ReplaceMessageLabels(ctx, p, m.ID, []string{"Invoices"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Store.MoveMessageToFolder(ctx, p, m.ID, folder.ID); err != nil {
		t.Fatal(err)
	}
	key := adminKey(t, svc, u)

	detail := apiGet(t, h, "/v1/messages/"+m.ID, key)
	var got struct {
		LabelIDs []string `json:"label_ids"`
		Labels   []string `json:"labels"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"tag:Invoices": true, "folder:" + folder.ID: true}
	if len(got.LabelIDs) != len(want) {
		t.Fatalf("label_ids = %v, want tag + folder id", got.LabelIDs)
	}
	for _, id := range got.LabelIDs {
		if !want[id] {
			t.Fatalf("unexpected label id %q in %v", id, got.LabelIDs)
		}
	}
	// label_ids is always present, even with no membership.
	plain := seedInbound(t, svc, box, "label-ids-empty", "<label-ids-empty@test>", "Empty", "body")
	empty := apiGet(t, h, "/v1/messages/"+plain.ID, key)
	if !strings.Contains(empty.Body.String(), `"label_ids":[]`) {
		t.Fatalf("label_ids must be present and empty: %s", empty.Body.String())
	}
}

// TestLabelFilterRouteParity proves the inbox-scoped and account-wide routes
// resolve label filters identically, including repeated labels and catalogue ids.
func TestLabelFilterRouteParity(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	folder, err := svc.Store.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	both := seedInbound(t, svc, box, "both-labels", "<both@test>", "Both", "body")
	one := seedInbound(t, svc, box, "one-label", "<one@test>", "One", "body")
	for id, labels := range map[string][]string{both.ID: {"Alpha", "Beta"}, one.ID: {"Alpha"}} {
		if _, err := svc.Store.ReplaceMessageLabels(ctx, p, id, labels); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := svc.Store.MoveMessageToFolder(ctx, p, one.ID, folder.ID); err != nil {
		t.Fatal(err)
	}
	key := adminKey(t, svc, u)

	ids := func(path string) ([]string, string) {
		t.Helper()
		rr := apiGet(t, h, path, key)
		if rr.Code != 200 {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
		var page struct {
			Items        []struct{ ID string } `json:"items"`
			Completeness string                `json:"completeness"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(page.Items))
		for _, item := range page.Items {
			out = append(out, item.ID)
		}
		return out, page.Completeness
	}

	// Two labels AND together on both routes.
	for _, suffix := range []string{
		"/v1/inboxes/" + box.ID + "/messages?label=Alpha&label=Beta",
		"/v1/messages?inbox=" + box.ID + "&label=Alpha&label=Beta",
	} {
		got, _ := ids(suffix)
		if len(got) != 1 || got[0] != both.ID {
			t.Fatalf("AND labels %s = %v, want only %s", suffix, got, both.ID)
		}
	}
	// A single legacy label, a canonical tag id, and a folder id all agree.
	for _, suffix := range []string{
		"/v1/inboxes/" + box.ID + "/messages?label=Alpha",
		"/v1/inboxes/" + box.ID + "/messages?label_id=tag:Alpha",
		"/v1/messages?inbox=" + box.ID + "&label_id=tag:Alpha",
	} {
		got, _ := ids(suffix)
		if len(got) != 2 {
			t.Fatalf("Alpha filter %s = %v, want both messages", suffix, got)
		}
	}
	scoped := "/v1/inboxes/" + box.ID + "/messages?label_id=" + url.QueryEscape("folder:"+folder.ID)
	account := "/v1/messages?inbox=" + box.ID + "&label_id=" + url.QueryEscape("folder:"+folder.ID)
	scopedIDs, scopedCompleteness := ids(scoped)
	accountIDs, accountCompleteness := ids(account)
	if len(scopedIDs) != 1 || len(accountIDs) != 1 {
		t.Fatalf("folder label scope returned %d/%d rows, want 1/1", len(scopedIDs), len(accountIDs))
	}
	inside, _ := svc.Store.GetMessage(ctx, p, scopedIDs[0])
	accountRow, _ := svc.Store.GetMessage(ctx, p, accountIDs[0])
	if inside.FolderPath != "Projects" || accountRow.FolderPath != "Projects" {
		t.Fatalf("folder label scope disagrees: scoped=%s(%q) account=%s(%q)", scopedIDs[0], inside.FolderPath, accountIDs[0], accountRow.FolderPath)
	}
	if scopedCompleteness != accountCompleteness {
		t.Fatalf("completeness disagrees: scoped=%q account=%q", scopedCompleteness, accountCompleteness)
	}
	// A folder-backed label with no inbox scope is a client error, not a guess.
	bad := apiGet(t, h, "/v1/messages?label_id="+url.QueryEscape("folder:"+folder.ID), key)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("unscoped folder label = %d, want 400: %s", bad.Code, bad.Body.String())
	}
}

// TestLabelCatalogCapabilitiesAndDescriptorConsistency proves an empty
// catalogue still describes its capabilities, and a create response matches the
// later listing descriptor.
func TestLabelCatalogCapabilitiesAndDescriptorConsistency(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	key := adminKey(t, svc, u)
	empty := apiGet(t, h, "/v1/inboxes/"+box.ID+"/label-catalog", key)
	var catalog struct {
		Items        []map[string]any `json:"items"`
		Capabilities struct {
			CanCreate        bool   `json:"can_create"`
			CreateKind       string `json:"create_kind"`
			CreateMembership string `json:"create_membership"`
			CanRename        bool   `json:"can_rename"`
			CanDelete        bool   `json:"can_delete"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(empty.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if !catalog.Capabilities.CanCreate || catalog.Capabilities.CreateKind != "tag" || catalog.Capabilities.CreateMembership != "multiple" {
		t.Fatalf("native capabilities: %+v", catalog.Capabilities)
	}
	// Create through the API and compare descriptors with the listing.
	req := httptest.NewRequest(http.MethodPost, "/v1/inboxes/"+box.ID+"/label-catalog", strings.NewReader(`{"name":"Created"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create label: %d %s", rr.Code, rr.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	listed := apiGet(t, h, "/v1/inboxes/"+box.ID+"/label-catalog", key)
	var after struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	for _, item := range after.Items {
		if item["id"] != created["id"] {
			continue
		}
		for _, field := range []string{"kind", "membership", "source", "can_remove", "can_rename", "can_delete"} {
			if item[field] != created[field] {
				t.Fatalf("descriptor drift for %s: create=%v list=%v", field, created[field], item[field])
			}
		}
		return
	}
	t.Fatalf("created label missing from catalogue: %s", listed.Body.String())
}

// TestLabelSchemasMatchServedCatalog proves the OpenAPI schemas advertise the
// fields the handlers actually return.
func TestLabelSchemasMatchServedCatalog(t *testing.T) {
	t.Parallel()
	schemas := apispec.Schemas()
	label := schemas["MailboxLabel"].(map[string]any)
	required := label["required"].([]string)
	for _, field := range []string{"can_remove", "can_rename", "can_delete"} {
		found := false
		for _, name := range required {
			found = found || name == field
		}
		if !found {
			t.Fatalf("MailboxLabel.%s must be required", field)
		}
	}
	for _, name := range []string{"kind", "membership", "source"} {
		prop := label["properties"].(map[string]any)[name].(map[string]any)
		if _, ok := prop["enum"]; !ok {
			t.Fatalf("MailboxLabel.%s must declare an enum", name)
		}
	}
	message := schemas["Message"].(map[string]any)
	if _, ok := message["properties"].(map[string]any)["label_ids"]; !ok {
		t.Fatal("Message.label_ids missing from schema")
	}
	found := false
	for _, name := range message["required"].([]string) {
		found = found || name == "label_ids"
	}
	if !found {
		t.Fatal("Message.label_ids must be required")
	}
	caps := schemas["MailboxLabelCatalog"].(map[string]any)["properties"].(map[string]any)
	if _, ok := caps["capabilities"]; !ok {
		t.Fatal("MailboxLabelCatalog.capabilities missing from schema")
	}
}
