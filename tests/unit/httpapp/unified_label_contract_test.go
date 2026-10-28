package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dellarb/mammothmail/internal/apispec"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

// labelCatalogue is the Beta3 label catalogue: additive tags only, plus the
// capability block that describes what the inbox supports even when the
// catalogue is empty.
type labelCatalogue struct {
	Items []struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Kind       string `json:"kind"`
		Membership string `json:"membership"`
		Source     string `json:"source"`
		CanAdd     bool   `json:"can_add"`
		CanRemove  bool   `json:"can_remove"`
		CanRename  bool   `json:"can_rename"`
		CanDelete  bool   `json:"can_delete"`
	} `json:"items"`
	Capabilities struct {
		CanCreate        bool   `json:"can_create"`
		CreateKind       string `json:"create_kind"`
		CreateMembership string `json:"create_membership"`
		CanRename        bool   `json:"can_rename"`
		CanDelete        bool   `json:"can_delete"`
	} `json:"capabilities"`
}

// fetchLabelCatalogue reads the additive label catalogue of one inbox.
func fetchLabelCatalogue(t *testing.T, h http.Handler, key, inboxID string) (labelCatalogue, *httptest.ResponseRecorder) {
	t.Helper()
	rr := apiGet(t, h, "/v1/inboxes/"+inboxID+"/label-catalog", key)
	var out labelCatalogue
	if rr.Code != http.StatusOK {
		t.Fatalf("label catalogue: %d %s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("label catalogue: %v %s", err, rr.Body.String())
	}
	return out, rr
}

// messageLabelState is the Beta3 membership projection of one message:
// additive label ids, the legacy name view and the canonical folder.
type messageLabelState struct {
	LabelIDs []string `json:"label_ids"`
	Labels   []string `json:"labels"`
	FolderID *string  `json:"folder_id"`
	Mailbox  string   `json:"mailbox_id"`
}

// fetchMessageState reads one message's membership projection.
func fetchMessageState(t *testing.T, h http.Handler, key, messageID string) messageLabelState {
	t.Helper()
	rr := apiGet(t, h, "/v1/messages/"+messageID, key)
	if rr.Code != http.StatusOK {
		t.Fatalf("message %s: %d %s", messageID, rr.Code, rr.Body.String())
	}
	var out messageLabelState
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("message %s: %v %s", messageID, err, rr.Body.String())
	}
	return out
}

// assertUnsupportedOp proves a mutation was refused because the operation is
// not supported, and not because it failed for some other reason.
func assertUnsupportedOp(t *testing.T, what string, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusConflict {
		t.Fatalf("%s: status %d, want an unsupported refusal: %s", what, rr.Code, rr.Body.String())
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body.Code != "unsupported_operation" {
		t.Fatalf("%s: refusal does not identify an unsupported operation: %s", what, rr.Body.String())
	}
}

// TestUnifiedLabelMembershipOnMessages proves the two memberships are
// independent: label_ids carries additive tags only, and the canonical folder
// is reported by folder_id. A system folder is never a label.
func TestUnifiedLabelMembershipOnMessages(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	m := seedInbound(t, svc, box, "label-ids", "<label-ids@test>", "Membership", "body")
	if _, err := svc.Store.ReplaceMessageLabels(ctx, p, m.ID, []string{"Invoices"}); err != nil {
		t.Fatal(err)
	}
	key := adminKey(t, svc, u)

	got := fetchMessageState(t, h, key, m.ID)
	if len(got.LabelIDs) != 1 || got.LabelIDs[0] != "tag:Invoices" {
		t.Fatalf("label_ids = %v, want only the additive tag", got.LabelIDs)
	}
	for _, id := range got.LabelIDs {
		if strings.HasPrefix(id, "folder:") {
			t.Fatalf("a folder is not a label: %v", got.LabelIDs)
		}
	}
	// The legacy name view keeps the additive names.
	if !stringSliceContains(got.Labels, "Invoices") {
		t.Fatalf("legacy labels = %v, want the additive name", got.Labels)
	}
	// Fresh mail is filed in the system Inbox: that is a location, not a label.
	inbox, err := svc.Store.GetSystemFolder(ctx, p.AccountID, box.ID, model.FolderRoleInbox)
	if err != nil {
		t.Fatal(err)
	}
	plain := seedInbound(t, svc, box, "label-ids-empty", "<label-ids-empty@test>", "Empty", "body")
	empty := fetchMessageState(t, h, key, plain.ID)
	if len(empty.LabelIDs) != 0 {
		t.Fatalf("a system folder must not appear as a label: %v", empty.LabelIDs)
	}
	if empty.FolderID == nil || *empty.FolderID != inbox.ID {
		t.Fatalf("folder_id = %v, want the system Inbox %s", empty.FolderID, inbox.ID)
	}
}

// TestLabelFilterRouteParity proves the inbox-scoped and account-wide routes
// resolve filters identically, and that a folder is scoped with folder= rather
// than with a label id.
func TestLabelFilterRouteParity(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	// A custom folder is a migration fixture: a domain inbox has system
	// folders only, but older data still files mail in one.
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
	// A single legacy label and a canonical tag id agree.
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
	// A folder is scoped with folder=, never with a label id. The scope is a
	// system folder id, the one a domain mailbox exposes.
	inboxFolder, err := svc.Store.GetSystemFolder(ctx, p.AccountID, box.ID, model.FolderRoleInbox)
	if err != nil {
		t.Fatal(err)
	}
	scoped := "/v1/inboxes/" + box.ID + "/messages?folder=" + url.QueryEscape(inboxFolder.ID)
	account := "/v1/messages?inbox=" + box.ID + "&folder=" + url.QueryEscape(inboxFolder.ID)
	scopedIDs, scopedCompleteness := ids(scoped)
	accountIDs, accountCompleteness := ids(account)
	if len(scopedIDs) != 1 || scopedIDs[0] != both.ID || len(accountIDs) != 1 || accountIDs[0] != both.ID {
		t.Fatalf("folder scope returned %v/%v, want only %s", scopedIDs, accountIDs, both.ID)
	}
	if scopedCompleteness != accountCompleteness {
		t.Fatalf("completeness disagrees: scoped=%q account=%q", scopedCompleteness, accountCompleteness)
	}
	// A folder id used as a label is a client error, on both routes.
	for _, suffix := range []string{
		"/v1/inboxes/" + box.ID + "/messages?label_id=" + url.QueryEscape("folder:"+inboxFolder.ID),
		"/v1/messages?inbox=" + box.ID + "&label_id=" + url.QueryEscape("folder:"+inboxFolder.ID),
		"/v1/messages?label_id=" + url.QueryEscape("folder:"+inboxFolder.ID),
	} {
		rr := apiGet(t, h, suffix, key)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("folder label filter %s = %d, want 400: %s", suffix, rr.Code, rr.Body.String())
		}
	}
}

// TestLabelCatalogCapabilitiesAndDescriptorConsistency proves the catalogue
// holds additive tags only, starts empty on a domain inbox, and still reports
// what the inbox supports. A create response matches the later listing.
func TestLabelCatalogCapabilitiesAndDescriptorConsistency(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	key := adminKey(t, svc, u)
	catalogue, rr := fetchLabelCatalogue(t, h, key, box.ID)
	if len(catalogue.Items) != 0 {
		t.Fatalf("a domain catalogue starts empty, got %+v", catalogue.Items)
	}
	if strings.Contains(rr.Body.String(), "folder:") || strings.Contains(rr.Body.String(), `"role"`) {
		t.Fatalf("the catalogue must hold additive tags only: %s", rr.Body.String())
	}
	if !catalogue.Capabilities.CanCreate || catalogue.Capabilities.CreateKind != "tag" || catalogue.Capabilities.CreateMembership != "multiple" {
		t.Fatalf("native capabilities: %+v", catalogue.Capabilities)
	}
	// Create through the API and compare descriptors with the listing.
	req := httptest.NewRequest(http.MethodPost, "/v1/inboxes/"+box.ID+"/label-catalog", strings.NewReader(`{"name":"Created"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	h.ServeHTTP(created, req)
	if created.Code != http.StatusCreated {
		t.Fatalf("create label: %d %s", created.Code, created.Body.String())
	}
	var createdLabel map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &createdLabel); err != nil {
		t.Fatal(err)
	}
	if createdLabel["id"] != "tag:Created" {
		t.Fatalf("created label id = %v, want tag:Created", createdLabel["id"])
	}
	listed, _ := fetchLabelCatalogue(t, h, key, box.ID)
	if len(listed.Items) != 1 || listed.Items[0].ID != "tag:Created" {
		t.Fatalf("catalogue after create: %+v", listed.Items)
	}
	for _, item := range listed.Items {
		if item.Kind != "tag" || item.Membership != "multiple" {
			t.Fatalf("catalogue entry must be an additive tag: %+v", item)
		}
	}
	entry := listed.Items[0]
	item := map[string]any{
		"kind": entry.Kind, "membership": entry.Membership, "source": entry.Source,
		"can_remove": entry.CanRemove, "can_rename": entry.CanRename, "can_delete": entry.CanDelete,
	}
	for _, field := range []string{"kind", "membership", "source", "can_remove", "can_rename", "can_delete"} {
		if item[field] != createdLabel[field] {
			t.Fatalf("descriptor drift for %s: create=%v list=%v", field, createdLabel[field], item[field])
		}
	}
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
	// Every catalogue entry is an additive tag: a folder is not a label.
	for name, want := range map[string]string{"kind": "tag", "membership": "multiple"} {
		prop := label["properties"].(map[string]any)[name].(map[string]any)
		values := prop["enum"].([]string)
		if len(values) != 1 || values[0] != want {
			t.Fatalf("MailboxLabel.%s enum = %v, want only %q", name, values, want)
		}
	}
	message := schemas["Message"].(map[string]any)
	properties := message["properties"].(map[string]any)
	if _, ok := properties["label_ids"]; !ok {
		t.Fatal("Message.label_ids missing from schema")
	}
	if _, ok := properties["folder_id"]; !ok {
		t.Fatal("Message.folder_id missing from schema")
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
