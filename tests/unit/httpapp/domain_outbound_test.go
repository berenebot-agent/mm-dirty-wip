package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gatehouse-mail/internal/model"
)

func TestUIDomainCreateNameOnlyRedirectsToDomainPage(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{"name": {"brand-new.example"}, "_csrf": {csrf}}
	req := httptest.NewRequest("POST", "/ui/domains", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create domain %d %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/" && !strings.HasPrefix(loc, "/?") {
		t.Fatalf("create domain redirect %q", loc)
	}
}

func TestUIDomainSendingConfigWorkflow(t *testing.T) {
	svc, h, u, dom, _ := httpFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)

	// Choosing a provider renders its non-secret form fields.
	req := httptest.NewRequest("GET", "/?domain="+dom.ID+"&kind=sending&provider=brevo", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("domain page %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `name="cfg_brevo_api_key"`) || !strings.Contains(body, `action="/ui/domains/`+dom.ID+`/sending"`) {
		t.Fatalf("domain page missing sending form")
	}

	// Saving persists the config and redirects back to the domain page.
	form := url.Values{
		"provider":           {"brevo"},
		"cfg_brevo_api_key":  {"k"},
		"cfg_brevo_api_base": {"https://api.brevo.com"},
		"_csrf":              {csrf},
	}
	req = httptest.NewRequest("POST", "/ui/domains/"+dom.ID+"/sending", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("save sending %d %s", rr.Code, rr.Body.String())
	}
	if got, _ := svc.Store.GetDomain(ctx, u.AccountID, dom.ID); got.SendingProvider != "brevo" {
		t.Fatalf("sending provider %+v", got)
	}

	// Clearing removes it.
	req = httptest.NewRequest("POST", "/ui/domains/"+dom.ID+"/sending/clear", strings.NewReader("_csrf="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("clear sending %d %s", rr.Code, rr.Body.String())
	}
	if got, _ := svc.Store.GetDomain(ctx, u.AccountID, dom.ID); got.SendingProvider != "" {
		t.Fatalf("sending provider not cleared %+v", got)
	}
}

func TestAPIInboxNoOutboundCredentialField(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	patch := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PATCH", "/v1/inboxes/"+box.ID, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	rr := patch(`{"display_name":"Renamed"}`)
	if rr.Code != 200 {
		t.Fatalf("patch inbox %d %s", rr.Code, rr.Body.String())
	}
	var got model.Inbox
	if err = json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "Renamed" {
		t.Fatalf("display name not updated: %+v", got)
	}
	if strings.Contains(rr.Body.String(), "outbound_credential_id") {
		t.Fatalf("inbox response must not expose outbound_credential_id: %s", rr.Body.String())
	}

	// The deprecated field is rejected outright rather than silently ignored.
	rr = patch(`{"outbound_credential_id":"out_legacy"}`)
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "outbound_credential_id") {
		t.Fatalf("removed field should be rejected: %d %s", rr.Code, rr.Body.String())
	}
}

func TestAPICreateDomainNameOnly(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/admin/domains", strings.NewReader(`{"name":"api.example"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 201 {
		t.Fatalf("create domain %d %s", rr.Code, rr.Body.String())
	}
	var dom model.Domain
	if err = json.Unmarshal(rr.Body.Bytes(), &dom); err != nil {
		t.Fatal(err)
	}
	if dom.Name != "api.example" {
		t.Fatalf("created domain %+v", dom)
	}
	// Assignment selectors were removed from the domain DTO.
	req = httptest.NewRequest("POST", "/v1/admin/domains", strings.NewReader(`{"name":"api2.example","outbound_credential_id":"out_x"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("removed assignment field should be rejected: %d %s", rr.Code, rr.Body.String())
	}
}

func TestDashboardInboxIssueDot(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	ctx := context.Background()
	d, err := svc.Store.CreateDomain(ctx, u.AccountID, "broken.example")
	if err != nil {
		t.Fatal(err)
	}
	box, err := svc.Store.CreateInbox(ctx, u.AccountID, d.ID, "ops", "Ops")
	if err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("dashboard %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, box.Address) {
		t.Fatalf("dashboard missing inbox %q", box.Address)
	}
	if !strings.Contains(body, `class="issue-dot"`) {
		t.Fatalf("dashboard missing issue-dot for inbox in unconfigured domain")
	}
	if !strings.Contains(body, "No Sender Configured for Inbox") {
		t.Fatalf("dashboard missing sender issue tooltip")
	}
	if !strings.Contains(body, "No Receiver Configured for Domain") {
		t.Fatalf("dashboard missing receiver issue tooltip")
	}
	for _, banned := range []string{"No sending provider for", "No receive path for"} {
		if strings.Contains(body, banned) {
			t.Fatalf("dashboard must not show global banner %q", banned)
		}
	}
}

func TestDashboardInboxIssueDotReceiveOnly(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	ctx := context.Background()
	d, err := svc.Store.CreateDomain(ctx, u.AccountID, "norcv.example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "brevo", map[string]any{"api_key": "k", "api_base": "https://api.brevo.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Store.CreateInbox(ctx, u.AccountID, d.ID, "ops", "Ops"); err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("dashboard %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "No Receiver Configured for Domain") {
		t.Fatalf("dashboard missing receive-path issue tooltip")
	}
}

func TestInboxViewPerInboxBanners(t *testing.T) {
	svc, h, u, _, fixtureBox := httpFixture(t)
	ctx := context.Background()
	d, err := svc.Store.CreateDomain(ctx, u.AccountID, "broken.example")
	if err != nil {
		t.Fatal(err)
	}
	brokenBox, err := svc.Store.CreateInbox(ctx, u.AccountID, d.ID, "ops", "Ops")
	if err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)
	get := func(path string) string {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}
	// Fixture domain has a receive path but no sending provider.
	fixtureBody := get("/ui/inboxes/" + fixtureBox.ID)
	if !strings.Contains(fixtureBody, "Sending is paused until a provider is configured for this domain.") {
		t.Fatalf("inbox view missing sending banner")
	}
	if strings.Contains(fixtureBody, "Not receiving — no receive path is configured for this domain.") {
		t.Fatalf("inbox view must not show receiving banner for configured domain")
	}
	// Domain with neither provider shows both banners.
	brokenBody := get("/ui/inboxes/" + brokenBox.ID)
	for _, want := range []string{
		"Sending is paused until a provider is configured for this domain.",
		"Not receiving — no receive path is configured for this domain.",
	} {
		if !strings.Contains(brokenBody, want) {
			t.Fatalf("inbox view missing %q", want)
		}
	}
}

func TestMessageViewPerInboxBanners(t *testing.T) {
	svc, h, u, _, fixtureBox := httpFixture(t)
	ctx := context.Background()
	d, err := svc.Store.CreateDomain(ctx, u.AccountID, "broken.example")
	if err != nil {
		t.Fatal(err)
	}
	brokenBox, err := svc.Store.CreateInbox(ctx, u.AccountID, d.ID, "ops", "Ops")
	if err != nil {
		t.Fatal(err)
	}
	mFixture := seedInbound(t, svc, fixtureBox, "del-1", "<m1>", "Hello", "body one")
	mBroken := seedInbound(t, svc, brokenBox, "del-2", "<m2>", "Hello", "body two")
	cookie, _ := uiSession(t, svc, u.ID)
	get := func(path string) string {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, rr.Code, rr.Body.String())
		}
		return rr.Body.String()
	}
	fixtureBody := get("/ui/messages/" + mFixture.ID)
	if !strings.Contains(fixtureBody, "Sending is paused until a provider is configured for this domain.") {
		t.Fatalf("message view missing sending banner")
	}
	if strings.Contains(fixtureBody, "Not receiving — no receive path is configured for this domain.") {
		t.Fatalf("message view must not show receiving banner for configured domain")
	}
	brokenBody := get("/ui/messages/" + mBroken.ID)
	for _, want := range []string{
		"Sending is paused until a provider is configured for this domain.",
		"Not receiving — no receive path is configured for this domain.",
	} {
		if !strings.Contains(brokenBody, want) {
			t.Fatalf("message view missing %q", want)
		}
	}
}
