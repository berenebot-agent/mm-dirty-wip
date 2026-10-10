package httpapp_test

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestInboxAliasUIRoundTrip exercises the inbox edit form's alias fields: a
// repeated `alias` address plus a parallel repeated `alias_name` name. The
// dashboard renders the name/address rows, and the shared alias dialog markup
// is present.
func TestInboxAliasUIRoundTrip(t *testing.T) {
	svc, h, u, dom, box := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)

	addr := "sales@" + dom.Name
	form := url.Values{}
	form.Set("_csrf", csrf)
	form.Set("display", "Hermes")
	form.Add("alias", addr)
	form.Add("alias_name", "Acme Sales")
	form.Add("alias", "billing@"+dom.Name)
	form.Add("alias_name", "Acme Billing")
	form.Set("default_sender", addr)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/edit", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("edit inbox %d: %s", rr.Code, rr.Body.String())
	}

	got, err := svc.Store.GetInboxInternal(context.Background(), u.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AliasNames[addr] != "Acme Sales" || got.AliasNames["billing@"+dom.Name] != "Acme Billing" {
		t.Fatalf("alias names %#v", got.AliasNames)
	}
	if got.DefaultSender != addr {
		t.Fatalf("default sender %q", got.DefaultSender)
	}

	// The dashboard renders the alias name/address pair in the edit button's
	// data attributes, and the shared alias dialog is present.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("dashboard %d", rr.Code)
	}
	body := rr.Body.String()
	// Aliases are ordered by local part, so billing precedes sales.
	if !strings.Contains(body, `id="inbox-alias-editor"`) || !strings.Contains(body, `data-alias-names="Acme Billing,Acme Sales"`) {
		t.Fatalf("dashboard missing inline alias editor or names")
	}
	if !strings.Contains(body, `class="section-head">Managed aliases<`) || !strings.Contains(body, `class="section-head">Primary / Default Address<`) {
		t.Fatalf("dashboard missing aliases/primary section headers")
	}
	if i, j := strings.Index(body, `id="inbox-alias-add"`), strings.Index(body, `id="inbox-alias-list"`); i < 0 || j < 0 || i > j {
		t.Fatalf("add-alias button should precede the alias list (button=%d list=%d)", i, j)
	}
}
