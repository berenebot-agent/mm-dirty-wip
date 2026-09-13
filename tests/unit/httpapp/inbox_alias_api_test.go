package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"gatehouse-mail/internal/model"
)

// TestInboxAliasREST exercises the inbox alias replace-set on PATCH
// /v1/inboxes/{id}: setting a cross-domain alias, reading it back, clearing it
// with an explicit empty array, and rejecting shadowed mailboxes and unknown
// domains.
func TestInboxAliasREST(t *testing.T) {
	svc, h, u, dom, box := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.Store.CreateDomain(ctx, u.AccountID, "other.com")
	if err != nil {
		t.Fatal(err)
	}

	do := func(method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/v1/inboxes/"+box.ID, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	rr := do("PATCH", `{"aliases":["sales@`+other.Name+`","info@`+dom.Name+`"]}`)
	if rr.Code != 200 {
		t.Fatalf("patch aliases %d %s", rr.Code, rr.Body.String())
	}
	var got model.Inbox
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Aliases) != 2 || got.Aliases[0] != "info@"+dom.Name || got.Aliases[1] != "sales@"+other.Name {
		t.Fatalf("aliases %#v", got.Aliases)
	}

	rr = do("GET", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"sales@`+other.Name+`"`) {
		t.Fatalf("get aliases %d %s", rr.Code, rr.Body.String())
	}

	// An explicit empty array clears the set.
	rr = do("PATCH", `{"aliases":[]}`)
	if rr.Code != 200 {
		t.Fatalf("clear aliases %d %s", rr.Code, rr.Body.String())
	}
	var cleared model.Inbox
	if err = json.Unmarshal(rr.Body.Bytes(), &cleared); err != nil {
		t.Fatal(err)
	}
	if len(cleared.Aliases) != 0 {
		t.Fatalf("aliases not cleared: %#v", cleared.Aliases)
	}

	// Shadowing an existing mailbox is rejected.
	if rr = do("PATCH", `{"aliases":["`+box.LocalPart+`@`+dom.Name+`"]}`); rr.Code != 400 {
		t.Fatalf("shadow mailbox %d %s", rr.Code, rr.Body.String())
	}
	// A domain the account does not own is rejected.
	if rr = do("PATCH", `{"aliases":["x@nope.test"]}`); rr.Code != 400 {
		t.Fatalf("unknown domain %d %s", rr.Code, rr.Body.String())
	}
}

// TestInboxAliasNamesREST exercises alias sender display names: setting them
// alongside an alias, reading them back, and clearing one with an empty string.
func TestInboxAliasNamesREST(t *testing.T) {
	svc, h, u, dom, box := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	do := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PATCH", "/v1/inboxes/"+box.ID, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	addr := "sales@" + dom.Name
	rr := do(`{"aliases":["` + addr + `"],"alias_names":{"` + addr + `":"Acme Sales"}}`)
	if rr.Code != 200 {
		t.Fatalf("set alias name %d %s", rr.Code, rr.Body.String())
	}
	var got model.Inbox
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.AliasNames[addr] != "Acme Sales" {
		t.Fatalf("alias names %#v", got.AliasNames)
	}

	// A names-only update keeps the alias set and clears the name.
	rr = do(`{"alias_names":{"` + addr + `":""}}`)
	if rr.Code != 200 {
		t.Fatalf("clear alias name %d %s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Aliases) != 1 || got.Aliases[0] != addr || got.AliasNames[addr] != "" {
		t.Fatalf("after clear aliases=%#v names=%#v", got.Aliases, got.AliasNames)
	}

	// A comma in a name is rejected.
	if rr = do(`{"alias_names":{"` + addr + `":"a,b"}}`); rr.Code != 400 {
		t.Fatalf("comma name %d %s", rr.Code, rr.Body.String())
	}
}
