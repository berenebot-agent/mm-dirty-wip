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

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// accessMember creates a non-admin account member (an owner of box) via the
// invitation flow and returns the user.
func accessMember(t *testing.T, svc *app.Service, accountID, ownerID, email string, inboxIDs ...string) model.User {
	t.Helper()
	ctx := context.Background()
	inv, _, err := svc.Store.CreateInvite(ctx, store.InviteInput{
		AccountID: accountID, Email: email, Kind: model.InviteKindOperator,
		InboxIDs: inboxIDs, CreatedBy: ownerID, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	token, err := svc.Store.RotateInviteToken(ctx, accountID, inv.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, err := svc.Store.RedeemInvite(ctx, token, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestDashboardRendersClientsAccessTab(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	key, _, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "Agent", false, map[string]string{box.ID: "read"})
	if err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`data-inbox-tab="access"`,
		`Clients &amp; Access`,
		`data-inbox-panel="access"`,
		`id="inbox-access-keys"`,
		`id="inbox-access-users"`,
		`id="inbox-access-invites"`,
		`id="access-add-key"`,
		`id="access-add-user"`,
		`data-inbox-subview="access-add"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	// The key's binding must be embedded, secret-free, in the edit button.
	if !strings.Contains(body, `data-access=`) || !strings.Contains(body, `"`+key.ID+`"`) {
		t.Fatalf("access grant not embedded; body missing key id %s", key.ID)
	}
	if strings.Contains(body, "onclick=") {
		t.Fatal("inline event handlers are blocked by CSP and must not be used")
	}
}

func TestInboxAccessCreateKey(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{"name": {"Scoped"}, "role": {"read"}, "_csrf": {csrf}}
	req := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/access/keys", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create key = %d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if got["notice"] != "API key created" || got["secret"] == "" {
		t.Fatalf("unexpected response %#v", got)
	}
	keys, _ := svc.Store.ListAPIKeys(context.Background(), u.AccountID)
	found := false
	for _, k := range keys {
		if k.Name == "Scoped" {
			found = true
			if k.Roles[box.ID] != "read" {
				t.Fatalf("new key role = %#v", k.Roles)
			}
			if len(k.Roles) != 1 {
				t.Fatalf("new key must be scoped to one inbox: %#v", k.Roles)
			}
		}
	}
	if !found {
		t.Fatal("new key not persisted")
	}
}

func TestInboxAccessSetKeyRoleAndRemove(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	second, err := svc.Store.CreateInbox(context.Background(), u.AccountID, box.DomainID, "second", "Second")
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "Agent", false, map[string]string{box.ID: "read", second.ID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)

	set := url.Values{"role": {"assistant"}, "_csrf": {csrf}}
	setReq := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/access/keys/"+key.ID, strings.NewReader(set.Encode()))
	setReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	setReq.AddCookie(cookie)
	setRR := httptest.NewRecorder()
	h.ServeHTTP(setRR, setReq)
	if setRR.Code != http.StatusSeeOther {
		t.Fatalf("set role = %d body=%s", setRR.Code, setRR.Body.String())
	}
	if loc := setRR.Header().Get("Location"); !strings.Contains(loc, "inbox_tab=access") || !strings.Contains(loc, "inbox="+box.ID) {
		t.Fatalf("redirect = %q", loc)
	}

	rem := url.Values{"_csrf": {csrf}}
	remReq := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/access/keys/"+key.ID+"/remove", strings.NewReader(rem.Encode()))
	remReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	remReq.AddCookie(cookie)
	remRR := httptest.NewRecorder()
	h.ServeHTTP(remRR, remReq)
	if remRR.Code != http.StatusSeeOther {
		t.Fatalf("remove = %d body=%s", remRR.Code, remRR.Body.String())
	}
	keys, _ := svc.Store.ListAPIKeys(context.Background(), u.AccountID)
	for _, k := range keys {
		if k.ID != key.ID {
			continue
		}
		if _, ok := k.Roles[box.ID]; ok {
			t.Fatalf("binding for %s should be gone: %#v", box.ID, k.Roles)
		}
		if k.Roles[second.ID] != "owner" {
			t.Fatalf("other inbox binding lost: %#v", k.Roles)
		}
	}
}

func TestInboxAccessRejectsAdminKeyEdit(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	adminKey, _, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "Admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{"role": {"owner"}, "_csrf": {csrf}}
	req := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/access/keys/"+adminKey.ID, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("admin key edit = %d, want 400 body=%s", rr.Code, rr.Body.String())
	}
}

func TestInboxAccessAddUserMerges(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	second, err := svc.Store.CreateInbox(context.Background(), u.AccountID, box.DomainID, "second", "Second")
	if err != nil {
		t.Fatal(err)
	}
	member := accessMember(t, svc, u.AccountID, u.ID, "member@example.com", second.ID)
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{"user": {member.ID}, "_csrf": {csrf}}
	req := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/access/users", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("add user = %d body=%s", rr.Code, rr.Body.String())
	}
	users, _ := svc.Store.ListAccountUsers(context.Background(), u.AccountID)
	for _, m := range users {
		if m.ID != member.ID {
			continue
		}
		if m.Roles[box.ID] != "owner" || m.Roles[second.ID] != "owner" {
			t.Fatalf("merged roles = %#v", m.Roles)
		}
	}
}

func TestInboxAccessInviteForwardsToAccount(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{"email": {"new@example.com"}, "_csrf": {csrf}}
	req := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/access/invites", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("invite = %d body=%s", rr.Code, rr.Body.String())
	}
	invites, _ := svc.Store.ListInvites(context.Background(), u.AccountID)
	found := false
	for _, inv := range invites {
		if inv.Email == "new@example.com" {
			found = true
			if inv.Kind != model.InviteKindOperator {
				t.Fatalf("invite kind = %q", inv.Kind)
			}
			hasInbox := false
			for _, id := range inv.InboxIDs {
				if id == box.ID {
					hasInbox = true
				}
			}
			if !hasInbox {
				t.Fatalf("invite not scoped to inbox: %#v", inv.InboxIDs)
			}
		}
	}
	if !found {
		t.Fatal("invitation not created")
	}
}

func TestInboxAccessRejectsForeignInbox(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	key, _, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "Agent", false, map[string]string{box.ID: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{"role": {"read"}, "_csrf": {csrf}}
	req := httptest.NewRequest("POST", "/ui/inboxes/does-not-exist/access/keys/"+key.ID, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("foreign inbox = %d, want 404 body=%s", rr.Code, rr.Body.String())
	}
}
