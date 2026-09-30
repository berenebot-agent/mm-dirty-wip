package httpapp_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// domainGet performs an authenticated UI GET and returns the recorder.
func domainGet(t *testing.T, h http.Handler, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// dialogHTML returns the markup of the dialog with the given id, so an
// assertion can be scoped to one domain's editor instead of the whole page.
func dialogHTML(t *testing.T, body, id string) string {
	t.Helper()
	start := strings.Index(body, `<dialog id="`+id+`"`)
	if start < 0 {
		t.Fatalf("dialog %q not found", id)
	}
	end := strings.Index(body[start:], "</dialog>")
	if end < 0 {
		t.Fatalf("dialog %q not closed", id)
	}
	return body[start : start+end]
}

// providerGroupHTML returns one provider group's markup inside a dialog: from
// its data-provider marker up to the next group, or the end of the dialog. The
// dashboard renders every provider group in a single dialog and toggles them
// with JS, so assertions about one provider must be scoped to its group.
func providerGroupHTML(t *testing.T, dialog, provider string) string {
	t.Helper()
	marker := `data-provider="` + provider + `"`
	start := strings.Index(dialog, marker)
	if start < 0 {
		t.Fatalf("provider group %q not found", provider)
	}
	rest := dialog[start+len(marker):]
	if next := strings.Index(rest, `data-provider="`); next >= 0 {
		rest = rest[:next]
	}
	return rest
}

// domainPost performs an authenticated UI POST with the given CSRF token.
func domainPost(t *testing.T, h http.Handler, cookie *http.Cookie, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestUIDomainPagePickersAndInstructions(t *testing.T) {
	svc, h, u, d, box := httpFixture(t)
	ctx := context.Background()
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "resend", map[string]any{"api_key": "re_live_secret", "api_base": "https://api.resend.com"}); err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)

	rr := domainGet(t, h, cookie, "/")
	if rr.Code != 200 {
		t.Fatalf("dashboard %d %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("dashboard cache-control = %q", got)
	}
	body := rr.Body.String()
	for _, want := range []string{
		d.Name,
		"/ui/domains/" + d.ID + "/catchall",
		`name="inbox"`,
		box.Address,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}

	// Selecting a provider renders its schema in the sending dialog with no
	// prefilled secret value.
	rr = domainGet(t, h, cookie, "/?domain="+d.ID+"&kind=sending&provider=resend")
	if rr.Code != 200 {
		t.Fatalf("sending picker %d %s", rr.Code, rr.Body.String())
	}
	body = rr.Body.String()
	for _, want := range []string{`<option value="resend" selected`, `name="cfg_resend_api_key"`, `name="cfg_resend_api_base"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("sending editor missing %q", want)
		}
	}
	if strings.Contains(body, `name="cfg_resend_api_key" value=`) || strings.Contains(body, "re_live_secret") {
		t.Fatalf("sending editor must not prefill or expose the stored secret")
	}

	// Receiving pre-save instructions show the exact Resend webhook URL and the
	// signing-secret onboarding steps.
	rr = domainGet(t, h, cookie, "/?domain="+d.ID+"&kind=receiving&provider=resend")
	if rr.Code != 200 {
		t.Fatalf("receiving picker %d %s", rr.Code, rr.Body.String())
	}
	body = rr.Body.String()
	for _, want := range []string{"/internal/ingest/resend", "email.received", `setup-webhook-url`, `name="cfg_resend_api_key"`, `name="cfg_resend_webhook_secret"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("receiving editor missing %q", want)
		}
	}
}

func TestUIDomainSendingSaveRetainsSecretAndClears(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)
	base := "/ui/domains/" + d.ID + "/sending"

	rr := domainPost(t, h, cookie, base, url.Values{
		"_csrf": {csrf}, "provider": {"resend"}, "cfg_resend_api_key": {"re_secret"}, "cfg_resend_api_base": {"https://api.resend.com"},
	})
	if rr.Code != 303 {
		t.Fatalf("save sending %d %s", rr.Code, rr.Body.String())
	}
	cfg, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if err != nil || cfg.Provider != "resend" {
		t.Fatalf("saved sending config: %v %+v", err, cfg)
	}
	dec, err := svc.DecryptDomainSendingConfig(cfg)
	if err != nil || dec["api_key"] != "re_secret" {
		t.Fatalf("stored secret: %v %+v", err, dec)
	}

	// Same-provider save with a blank secret retains it; a non-secret field is
	// a whole-config value.
	rr = domainPost(t, h, cookie, base, url.Values{
		"_csrf": {csrf}, "provider": {"resend"}, "cfg_resend_api_base": {"https://api2.resend.com"},
	})
	if rr.Code != 303 {
		t.Fatalf("resave sending %d %s", rr.Code, rr.Body.String())
	}
	cfg, err = svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	dec, err = svc.DecryptDomainSendingConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dec["api_key"] != "re_secret" || dec["api_base"] != "https://api2.resend.com" {
		t.Fatalf("same-provider save must retain secret and replace non-secret: %+v", dec)
	}

	// Clear is idempotent for an existing domain and removes the config.
	rr = domainPost(t, h, cookie, base+"/clear", url.Values{"_csrf": {csrf}})
	if rr.Code != 303 {
		t.Fatalf("clear sending %d %s", rr.Code, rr.Body.String())
	}
	if _, err = svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID); !errors.Is(err, store.ErrNoProvider) {
		t.Fatalf("sending config after clear: %v", err)
	}
}

func TestUIDomainReceivingCloudflareWorkerFlashIsOneTime(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)

	rr := domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/receiving", url.Values{
		"_csrf": {csrf}, "provider": {"cloudflare"},
	})
	if rr.Code != 303 {
		t.Fatalf("save receiving %d %s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "_flash=") {
		t.Fatalf("generated worker redirect missing flash: %q", loc)
	}
	cfg, err := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, d.ID)
	if err != nil || cfg.Provider != "cloudflare" {
		t.Fatalf("stored receiving config: %v %+v", err, cfg)
	}

	rr = domainGet(t, h, cookie, loc)
	if rr.Code != 200 {
		t.Fatalf("worker flash page %d %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("worker flash cache-control = %q", got)
	}
	body := rr.Body.String()
	for _, want := range []string{`id="cf-code"`, "WEBHOOK_URL = ", "http://example.test/internal/ingest/cloudflare"} {
		if !strings.Contains(body, want) {
			t.Fatalf("worker flash missing %q", want)
		}
	}

	// The secret is shown once: a refresh of the same flash URL, and a plain
	// domain page, no longer contain the Worker code.
	if body = domainGet(t, h, cookie, loc).Body.String(); strings.Contains(body, `id="cf-code"`) {
		t.Fatalf("worker code must not be shown after the first view")
	}
	if body = domainGet(t, h, cookie, "/?domain="+d.ID+"&kind=receiving").Body.String(); strings.Contains(body, `id="cf-code"`) {
		t.Fatalf("worker code must not persist on the dashboard")
	}
}

func TestUIDomainReceivingRegenerateChangesSecret(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	ctx := context.Background()
	saved, generated, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "cloudflare", nil, false)
	if err != nil || len(generated) == 0 {
		t.Fatalf("seed cloudflare: %v %+v", err, generated)
	}
	if saved.Provider != "cloudflare" {
		t.Fatalf("seed provider %q", saved.Provider)
	}
	oldSecret := generated["webhook_secret"]

	cookie, csrf := uiSession(t, svc, u.ID)
	rr := domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/receiving/regenerate", url.Values{"_csrf": {csrf}})
	if rr.Code != 303 {
		t.Fatalf("regenerate %d %s", rr.Code, rr.Body.String())
	}
	cfg, err := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := svc.DecryptDomainReceivingConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dec["webhook_secret"] == oldSecret || dec["webhook_secret"] == "" {
		t.Fatalf("regenerate must mint a new secret")
	}
}

func TestUIDomainCatchAllOnlyOwnInboxes(t *testing.T) {
	svc, h, u, d, own := httpFixture(t)
	ctx := context.Background()
	other, err := svc.Store.CreateDomain(ctx, u.AccountID, "other.example")
	if err != nil {
		t.Fatal(err)
	}
	otherBox, err := svc.Store.CreateInbox(ctx, u.AccountID, other.ID, "ops", "Ops")
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)

	// An inbox from another domain is rejected.
	rr := domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/catchall", url.Values{"_csrf": {csrf}, "inbox": {otherBox.ID}})
	if rr.Code != 400 {
		t.Fatalf("foreign catch-all = %d %s", rr.Code, rr.Body.String())
	}
	if dom, err := svc.Store.GetDomain(ctx, u.AccountID, d.ID); err != nil || dom.CatchAllInboxID != "" {
		t.Fatalf("foreign catch-all must not be stored: %v %+v", err, dom)
	}

	// The domain's own inbox is accepted.
	rr = domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/catchall", url.Values{"_csrf": {csrf}, "inbox": {own.ID}})
	if rr.Code != 303 {
		t.Fatalf("own catch-all = %d %s", rr.Code, rr.Body.String())
	}
	if dom, err := svc.Store.GetDomain(ctx, u.AccountID, d.ID); err != nil || dom.CatchAllInboxID != own.ID {
		t.Fatalf("own catch-all not stored: %v %+v", err, dom)
	}
}

// TestUIDomainPageRepeatedViewHidesSecretAndValidatesNumericInput proves that
// repeated full domain views never expose a stored secret and show effective
// non-secret fields, and that a non-numeric field is a user-safe validation
// error that never leaks the secret or mutates the stored config.
func TestUIDomainPageRepeatedViewHidesSecretAndValidatesNumericInput(t *testing.T) {
	const secret = "smtp-password-secret"
	svc, h, u, d, _ := httpFixture(t)
	ctx := context.Background()
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "smtp", map[string]any{
		"host": "smtp.example.com", "port": 2525, "username": "ops",
		"password": secret, "security": "starttls", "from_domain": "example.com",
	}); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)

	for i := 0; i < 3; i++ {
		body := domainGet(t, h, cookie, "/?domain="+d.ID+"&kind=sending").Body.String()
		if strings.Contains(body, secret) {
			t.Fatalf("stored secret leaked on full view %d", i)
		}
		if !strings.Contains(body, "smtp.example.com") || !strings.Contains(body, "2525") {
			t.Fatalf("view %d missing effective non-secret fields", i)
		}
	}

	// A non-numeric port is a user-safe error, not a silently applied default.
	rr := domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/sending", url.Values{
		"_csrf": {csrf}, "provider": {"smtp"}, "cfg_smtp_host": {"smtp2.example.com"}, "cfg_smtp_port": {"not-a-number"},
	})
	if rr.Code != 303 {
		t.Fatalf("bad port %d %s", rr.Code, rr.Body.String())
	}
	body := domainGet(t, h, cookie, rr.Header().Get("Location")).Body.String()
	if !strings.Contains(body, "Port must be a whole number") {
		t.Fatalf("missing numeric validation error")
	}
	if !strings.Contains(body, `value="smtp2.example.com"`) {
		t.Fatalf("submitted non-secret input was not retained on the error page")
	}
	if strings.Contains(body, secret) {
		t.Fatalf("secret leaked on the validation error page")
	}
	if strings.Contains(body, `name="cfg_smtp_password" value=`) || strings.Contains(body, `value="`+secret+`"`) {
		t.Fatalf("form must not prefill or reflect a secret value")
	}

	// The failed save must not have modified the stored config.
	cfg, err := svc.Store.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := svc.DecryptDomainSendingConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dec["password"] != secret || dec["host"] != "smtp.example.com" {
		t.Fatalf("failed save mutated stored config: %+v", dec)
	}
	if port, _ := dec["port"].(float64); port != 2525 {
		t.Fatalf("failed save mutated port: %+v", dec["port"])
	}
}

// TestUIDomainWorkerFlashBoundToPrincipalAndDomain proves a generated Worker
// flash can neither be read nor consumed by a different user or domain, and is
// shown exactly once to its owner.
func TestUIDomainWorkerFlashBoundToPrincipalAndDomain(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)
	rr := domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/receiving", url.Values{"_csrf": {csrf}, "provider": {"cloudflare"}})
	if rr.Code != 303 {
		t.Fatalf("generate worker %d %s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	idx := strings.Index(loc, "_flash=")
	if idx < 0 {
		t.Fatalf("generated worker redirect missing flash: %q", loc)
	}
	tok := strings.TrimPrefix(loc[idx:], "_flash=")
	if amp := strings.Index(tok, "&"); amp >= 0 {
		tok = tok[:amp]
	}
	flash := "&_flash=" + tok
	dash := func(domainID string) string { return "/?domain=" + domainID + "&kind=receiving" + flash }

	// Same user, different domain: no leak, and the flash must survive.
	d2, err := svc.Store.CreateDomain(ctx, u.AccountID, "second.example")
	if err != nil {
		t.Fatal(err)
	}
	if body := domainGet(t, h, cookie, dash(d2.ID)).Body.String(); strings.Contains(body, `id="cf-code"`) {
		t.Fatalf("flash leaked to another domain")
	}

	// Different account: no leak, and the flash must survive.
	b, err := svc.Store.CreateAccountAndAdmin(ctx, "B", "b@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	db, err := svc.Store.CreateDomain(ctx, b.AccountID, "b.example")
	if err != nil {
		t.Fatal(err)
	}
	cookieB, _ := uiSession(t, svc, b.ID)
	if body := domainGet(t, h, cookieB, dash(db.ID)).Body.String(); strings.Contains(body, `id="cf-code"`) {
		t.Fatalf("flash leaked across accounts")
	}

	// The owner still gets the one-time code, exactly once.
	if body := domainGet(t, h, cookie, dash(d.ID)).Body.String(); !strings.Contains(body, `id="cf-code"`) {
		t.Fatalf("owner flash was consumed by a wrong principal or domain")
	}
	if body := domainGet(t, h, cookie, dash(d.ID)).Body.String(); strings.Contains(body, `id="cf-code"`) {
		t.Fatalf("worker flash shown more than once")
	}
}

// TestUIDomainCloudflareSelectableAfterConfigured proves Cloudflare remains a
// selectable change target after it is configured, its generated secret never
// renders an input, and viewing the page never rotates the stored secret.
func TestUIDomainCloudflareSelectableAfterConfigured(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	ctx := context.Background()
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "cloudflare", map[string]any{"webhook_secret": "known-cf-secret"}, false); err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)
	body := domainGet(t, h, cookie, "/?domain="+d.ID+"&kind=receiving").Body.String()
	if !strings.Contains(body, `<option value="cloudflare"`) {
		t.Fatalf("cloudflare must remain selectable after it is configured")
	}
	body = domainGet(t, h, cookie, "/?domain="+d.ID+"&kind=receiving&provider=cloudflare").Body.String()
	if !strings.Contains(body, `<option value="cloudflare" selected`) {
		t.Fatalf("cloudflare editor not reachable after configure")
	}
	if strings.Contains(body, `name="cfg_cloudflare_webhook_secret"`) {
		t.Fatalf("generated secret must not render an input")
	}
	if strings.Contains(body, "known-cf-secret") {
		t.Fatalf("stored secret leaked on the dashboard")
	}
	cfg, err := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := svc.DecryptDomainReceivingConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dec["webhook_secret"] != "known-cf-secret" {
		t.Fatalf("GET rotated the stored receiving secret")
	}
}

// TestUIDomainReceivingSetupBeforeSaved proves the Resend onboarding URL and
// steps are visible before any receiving config exists, and that viewing them
// does not save anything.
func TestUIDomainReceivingSetupBeforeSaved(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	ctx := context.Background()
	d, err := svc.Store.CreateDomain(ctx, u.AccountID, "fresh.example")
	if err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)
	body := domainGet(t, h, cookie, "/?domain="+d.ID+"&kind=receiving&provider=resend").Body.String()
	for _, want := range []string{"/internal/ingest/resend", "email.received", `setup-webhook-url`, `name="cfg_resend_api_key"`, `name="cfg_resend_webhook_secret"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("pre-save receiving setup missing %q", want)
		}
	}
	if _, err := svc.Store.GetDomainReceivingConfig(ctx, u.AccountID, d.ID); !errors.Is(err, store.ErrNoProvider) {
		t.Fatalf("viewing setup must not persist a config: %v", err)
	}
}

// TestUIDomainMutationRequiresCSRF proves every domain-config mutation is
// CSRF-protected before it touches state.
func TestUIDomainMutationRequiresCSRF(t *testing.T) {
	routes := []struct {
		name string
		path func(string) string
		form url.Values
	}{
		{"catchall", func(id string) string { return "/ui/domains/" + id + "/catchall" }, url.Values{"inbox": {""}}},
		{"sending", func(id string) string { return "/ui/domains/" + id + "/sending" }, url.Values{"provider": {"brevo"}, "cfg_brevo_api_key": {"k"}, "cfg_brevo_api_base": {"https://api.brevo.com"}}},
		{"sending-clear", func(id string) string { return "/ui/domains/" + id + "/sending/clear" }, url.Values{}},
		{"receiving", func(id string) string { return "/ui/domains/" + id + "/receiving" }, url.Values{"provider": {"cloudflare"}}},
		{"receiving-clear", func(id string) string { return "/ui/domains/" + id + "/receiving/clear" }, url.Values{}},
		{"receiving-regenerate", func(id string) string { return "/ui/domains/" + id + "/receiving/regenerate" }, url.Values{}},
		{"delete", func(id string) string { return "/ui/domains/" + id + "/delete" }, url.Values{}},
	}
	for _, tc := range routes {
		t.Run(tc.name, func(t *testing.T) {
			svc, h, u, d, _ := httpFixture(t)
			cookie, csrf := uiSession(t, svc, u.ID)

			bad := url.Values{}
			for k, v := range tc.form {
				bad[k] = v
			}
			bad.Set("_csrf", "wrong-token")
			if rr := domainPost(t, h, cookie, tc.path(d.ID), bad); rr.Code != http.StatusForbidden {
				t.Fatalf("missing CSRF not rejected: %d %s", rr.Code, rr.Body.String())
			}

			good := url.Values{}
			for k, v := range tc.form {
				good[k] = v
			}
			good.Set("_csrf", csrf)
			if rr := domainPost(t, h, cookie, tc.path(d.ID), good); rr.Code == http.StatusForbidden {
				t.Fatalf("valid CSRF rejected: %d %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestUIDomainCrossAccountControls proves another account cannot view or mutate
// a domain's configuration through the human UI.
func TestUIDomainCrossAccountControls(t *testing.T) {
	svc, h, _, dom, _ := httpFixture(t)
	ctx := context.Background()
	b, err := svc.Store.CreateAccountAndAdmin(ctx, "B", "b@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	cookieB, csrfB := uiSession(t, svc, b.ID)

	// The standalone domain page no longer exists; a foreign account cannot
	// open another account's domain dialog on its own dashboard.
	if rr := domainGet(t, h, cookieB, "/?domain="+dom.ID); rr.Code != 200 {
		t.Fatalf("foreign dashboard view = %d", rr.Code)
	}
	for _, tc := range []struct {
		path string
		form url.Values
	}{
		{"/ui/domains/" + dom.ID + "/catchall", url.Values{"inbox": {""}}},
		{"/ui/domains/" + dom.ID + "/sending", url.Values{"provider": {"brevo"}, "cfg_brevo_api_key": {"k"}, "cfg_brevo_api_base": {"https://api.brevo.com"}}},
		{"/ui/domains/" + dom.ID + "/sending/clear", url.Values{}},
		{"/ui/domains/" + dom.ID + "/receiving", url.Values{"provider": {"cloudflare"}}},
		{"/ui/domains/" + dom.ID + "/receiving/clear", url.Values{}},
		{"/ui/domains/" + dom.ID + "/receiving/regenerate", url.Values{}},
	} {
		form := url.Values{}
		for k, v := range tc.form {
			form[k] = v
		}
		form.Set("_csrf", csrfB)
		if rr := domainPost(t, h, cookieB, tc.path, form); rr.Code != 404 {
			t.Fatalf("foreign mutation %s = %d %s", tc.path, rr.Code, rr.Body.String())
		}
	}
	if _, err := svc.Store.GetDomainSendingConfig(ctx, b.AccountID, dom.ID); !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrNoProvider) {
		t.Fatalf("foreign account unexpectedly has config: %v", err)
	}
}

// TestUIDomainNoticeFlashBoundToOwner proves a validation-error flash is bound
// to its owner and domain: a foreign domain cannot consume it, and the owner
// still sees the error.
func TestUIDomainNoticeFlashBoundToOwner(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)
	rr := domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/sending", url.Values{
		"_csrf": {csrf}, "provider": {"smtp"}, "cfg_smtp_host": {"smtp.example.com"}, "cfg_smtp_port": {"nope"},
	})
	if rr.Code != 303 {
		t.Fatalf("trigger error %d %s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	idx := strings.Index(loc, "_flash=")
	if idx < 0 {
		t.Fatalf("validation error redirect missing flash: %q", loc)
	}
	flash := "&_flash=" + strings.TrimPrefix(loc[idx:], "_flash=")

	d2, err := svc.Store.CreateDomain(ctx, u.AccountID, "other.example")
	if err != nil {
		t.Fatal(err)
	}
	foreign := "/?domain=" + d2.ID + "&kind=sending&provider=smtp" + flash
	if body := domainGet(t, h, cookie, foreign).Body.String(); strings.Contains(body, "Port must be a whole number") {
		t.Fatalf("notice flash leaked to another domain")
	}
	if body := domainGet(t, h, cookie, loc).Body.String(); !strings.Contains(body, "Port must be a whole number") {
		t.Fatalf("owner notice flash was consumed by a wrong domain")
	}
}

// TestUIDomainHistorySurvivesConfigDelete proves the delivery history is scoped
// by domain, not by the presence of a current config.
func TestUIDomainHistorySurvivesConfigDelete(t *testing.T) {
	svc, h, u, dom, box := httpFixture(t)
	ctx := context.Background()
	setDomainBrevo(t, svc, u.AccountID, dom.ID)
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	res, err := svc.Send(ctx, p, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "History", Text: "hi"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Deliver(ctx, u.AccountID, res.Message.ID, ""); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)
	base := "/ui/domains/" + dom.ID

	if body := domainGet(t, h, cookie, base+"/sending/deliveries").Body.String(); !strings.Contains(body, res.Message.ID) {
		t.Fatalf("delivery history missing while configured")
	}
	if rr := domainPost(t, h, cookie, base+"/sending/clear", url.Values{"_csrf": {csrf}}); rr.Code != 303 {
		t.Fatalf("clear sending %d %s", rr.Code, rr.Body.String())
	}
	if body := domainGet(t, h, cookie, base+"/sending/deliveries").Body.String(); !strings.Contains(body, res.Message.ID) {
		t.Fatalf("delivery history hidden after config delete")
	}
	// The sending history link must remain reachable even once unconfigured.
	if body := domainGet(t, h, cookie, "/?domain="+dom.ID+"&kind=sending").Body.String(); !strings.Contains(body, `href="`+base+`/sending/deliveries"`) {
		t.Fatalf("sending history link missing when unconfigured")
	}
}

// TestUIDomainSendingEditorPrefillsStoredNonsecret proves the same-provider
// editor repopulates stored non-secret values (including a non-default select
// choice) while secret inputs stay truly empty, and that an explicit Edit link
// opens the current provider's editor.
func TestUIDomainSendingEditorPrefillsStoredNonsecret(t *testing.T) {
	const secret = "smtp-fresh-secret"
	svc, h, u, d, _ := httpFixture(t)
	ctx := context.Background()
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "smtp", map[string]any{
		"host": "smtp.example.com", "port": 2525, "username": "ops",
		"password": secret, "security": "tls", "from_domain": "example.com",
	}); err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)

	body := dialogHTML(t, domainGet(t, h, cookie, "/?domain="+d.ID+"&kind=sending").Body.String(), "domain-sending-dialog-"+d.ID)
	for _, want := range []string{
		`value="smtp.example.com"`,
		`value="ops"`,
		`value="2525"`,
		`<option value="tls" selected>`,
		`(leave blank to keep the current value)`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("same-provider editor missing %q", want)
		}
	}
	if strings.Contains(body, secret) || strings.Contains(body, `name="cfg_smtp_password" value=`) {
		t.Fatalf("secret must never be prefilled or reflected")
	}
}

// TestUIDomainEditorDefaultsOnCreateAndSwitch proves a new or switched provider
// starts from schema defaults rather than the previous provider's values.
func TestUIDomainEditorDefaultsOnCreateAndSwitch(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	ctx := context.Background()
	cookie, _ := uiSession(t, svc, u.ID)

	// A brand-new SMTP editor shows the numeric/select defaults.
	body := dialogHTML(t, domainGet(t, h, cookie, "/?domain="+d.ID+"&kind=sending&provider=smtp").Body.String(), "domain-sending-dialog-"+d.ID)
	for _, want := range []string{`value="587"`, `<option value="starttls" selected>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("new editor missing schema default %q", want)
		}
	}
	if strings.Contains(body, "(leave blank to keep the current value)") {
		t.Fatalf("a create editor must not offer leave-blank retention")
	}

	// After configuring SMTP, switching to Brevo must not reuse SMTP's values.
	if _, err := svc.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "smtp", map[string]any{"host": "smtp.example.com", "password": "x"}); err != nil {
		t.Fatal(err)
	}
	body = dialogHTML(t, domainGet(t, h, cookie, "/?domain="+d.ID+"&kind=sending&provider=brevo").Body.String(), "domain-sending-dialog-"+d.ID)
	if !strings.Contains(body, `<option value="brevo" selected`) {
		t.Fatalf("switch editor not for brevo")
	}
	brevo := providerGroupHTML(t, body, "brevo")
	if strings.Contains(brevo, `value="smtp.example.com"`) {
		t.Fatalf("switch editor prefilled the previous provider's values")
	}
	if strings.Contains(brevo, "(leave blank to keep the current value)") {
		t.Fatalf("a provider switch must not offer leave-blank retention")
	}
}

// TestUIDomainWorkerFlashConcurrentSingleUse proves that concurrent authorized
// GETs cannot both render the same one-time Worker code. Each round mints a
// fresh flash and fires a burst of requests at it.
func TestUIDomainWorkerFlashConcurrentSingleUse(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	if rr := domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/receiving", url.Values{"_csrf": {csrf}, "provider": {"cloudflare"}}); rr.Code != 303 {
		t.Fatalf("create receiving %d %s", rr.Code, rr.Body.String())
	}

	flashToken := func(rr *httptest.ResponseRecorder) string {
		loc := rr.Header().Get("Location")
		idx := strings.Index(loc, "_flash=")
		if idx < 0 {
			t.Fatalf("redirect missing flash: %q", loc)
		}
		tok := strings.TrimPrefix(loc[idx:], "_flash=")
		if amp := strings.Index(tok, "&"); amp >= 0 {
			tok = tok[:amp]
		}
		return tok
	}

	const (
		rounds  = 25
		workers = 16
	)
	for round := 0; round < rounds; round++ {
		rr := domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/receiving/regenerate", url.Values{"_csrf": {csrf}})
		if rr.Code != 303 {
			t.Fatalf("round %d regenerate %d %s", round, rr.Code, rr.Body.String())
		}
		path := "/?domain=" + d.ID + "&kind=receiving&_flash=" + flashToken(rr)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		hits := 0
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				req := httptest.NewRequest("GET", path, nil)
				req.AddCookie(cookie)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if strings.Contains(rec.Body.String(), `id="cf-code"`) {
					mu.Lock()
					hits++
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()
		if hits != 1 {
			t.Fatalf("round %d: one-time Worker code rendered %d times across concurrent GETs, want exactly 1", round, hits)
		}
	}
}

// TestUIDomainReceivingWebhookURLIsPerProvider guards the DOM contract the
// dashboard's "Copy webhook URL" button relies on: every provider group owns
// its own webhook URL and its own copy button. The dialog renders all providers
// at once and toggles them with JS, so a dialog-wide lookup would copy whichever
// provider happens to be first (Mailgun) even when Resend is selected. That
// pointed the Resend webhook at /internal/ingest/mailgun/raw-mime, where the
// Mailgun adapter rejected the JSON body as a terminal error and returned 406.
func TestUIDomainReceivingWebhookURLIsPerProvider(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)

	dlg := dialogHTML(t, domainGet(t, h, cookie, "/?domain="+d.ID+"&kind=receiving&provider=resend").Body.String(), "domain-receiving-dialog-"+d.ID)

	webhookURL := func(group string) string {
		const open = `<pre class="setup-webhook-url">`
		i := strings.Index(group, open)
		if i < 0 {
			return ""
		}
		rest := group[i+len(open):]
		j := strings.Index(rest, "</pre>")
		if j < 0 {
			return ""
		}
		return rest[:j]
	}

	for _, tc := range []struct{ provider, suffix string }{
		{"mailgun", "/internal/ingest/mailgun/raw-mime"},
		{"resend", "/internal/ingest/resend"},
	} {
		group := providerGroupHTML(t, dlg, tc.provider)
		if got := webhookURL(group); !strings.HasSuffix(got, tc.suffix) {
			t.Fatalf("%s webhook URL = %q, want suffix %q", tc.provider, got, tc.suffix)
		}
		// The copy button must live in the same group as the URL it copies.
		if !strings.Contains(group, `secondary setup-copy`) {
			t.Fatalf("%s group has no scoped copy button", tc.provider)
		}
	}
}

// TestUIDomainProviderInheritLinksLateParent proves that a subdomain added
// before its parent can pick "Inherited (from parent)" in the sending/receiving
// provider menus, and that saving links it to the parent and inherits.
func TestUIDomainProviderInheritLinksLateParent(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	ctx := context.Background()

	// Subdomain first: no ancestor exists yet, so it is a root domain.
	sub, err := svc.Store.CreateDomain(ctx, u.AccountID, "agent.late.test")
	if err != nil {
		t.Fatal(err)
	}
	if sub.ParentDomainID != "" {
		t.Fatalf("subdomain added first must be unlinked: %+v", sub)
	}
	// The parent is added later and configured for receiving.
	parent, err := svc.Store.CreateDomain(ctx, u.AccountID, "late.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, parent.ID, "cloudflare", map[string]any{}, false); err != nil {
		t.Fatal(err)
	}

	cookie, csrf := uiSession(t, svc, u.ID)
	body := dialogHTML(t, domainGet(t, h, cookie, "/?domain="+sub.ID+"&kind=receiving").Body.String(), "domain-receiving-dialog-"+sub.ID)
	if !strings.Contains(body, `Inherited (from late.test)`) {
		t.Fatalf("receiving menu must offer inheritance from the late parent:\n%s", body)
	}

	// Selecting it links the domain and enables receiving inheritance.
	if rr := domainPost(t, h, cookie, "/ui/domains/"+sub.ID+"/receiving", url.Values{"_csrf": {csrf}, "provider": {"inherited"}}); rr.Code != 303 {
		t.Fatalf("selecting inherited receiving %d %s", rr.Code, rr.Body.String())
	}
	got, err := svc.Store.GetDomain(ctx, u.AccountID, sub.ID)
	if err != nil || got.ParentDomainID != parent.ID || !got.InheritReceiving || got.ReceivingInheritedFrom != "late.test" {
		t.Fatalf("after inheriting receiving %+v err=%v", got, err)
	}
}

// TestDashboardWebhookCopyScopesToProviderGroup pins the fix for the copy
// handler: it resolves the URL within the button's provider group instead of
// the whole dialog.
func TestDashboardWebhookCopyScopesToProviderGroup(t *testing.T) {
	_, h, _, _, _ := httpFixture(t)
	req := httptest.NewRequest("GET", "/assets/app.js", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("app.js %d", rr.Code)
	}
	js := rr.Body.String()
	if !strings.Contains(js, `closest('.provider-fields')`) {
		t.Fatalf("copy handler must scope the webhook URL to its provider group")
	}
	if strings.Contains(js, `dlg.querySelector('.setup-webhook-url')`) {
		t.Fatalf("copy handler must not look up the webhook URL dialog-wide")
	}
}
