package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestHermesEnvBlock(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	_, key, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/admin/hermes/enroll", strings.NewReader(`{"inbox_id":"`+box.ID+`","name":"gw"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("enroll %d %s", rr.Code, rr.Body.String())
	}
	var got struct {
		GatewayID string `json:"gateway_id"`
		Secret    string `json:"secret"`
		Env       string `json:"env"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	want := "GATEWAY_RELAY_URL=http://example.test\n" +
		"GATEWAY_RELAY_ID=" + got.GatewayID + "\n" +
		"GATEWAY_RELAY_SECRET=" + got.Secret + "\n" +
		"GATEWAY_RELAY_PLATFORMS=email\n" +
		"GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS=true"
	if got.Env != want {
		t.Fatalf("env block:\n%s\nwant:\n%s", got.Env, want)
	}
}

func TestDashboardRendersKeyDialog(t *testing.T) {
	t.Parallel()
	svc, h, u, _, _ := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	guide := regexp.MustCompile(`(?s)<ol class="steps" id="key-hermes-guide" hidden>(.*?)</ol><p class="muted" id="key-result-label">`).FindString(body)
	for _, want := range []string{
		"Optional but recommended — reduce extra emails:",
		"same <code>.env</code>",
		"EMAIL_HOME_ADDRESS=you@yourdomain.com\nEMAIL_HOME_ADDRESS_NAME=Your Agent Name",
		"~/.hermes/config.yaml",
		"merge into any existing <code>display</code> section",
		"display:\n  memory_notifications: off\n  suppress_warning_notifications: true",
		"Restart the gateway to reload the configuration",
	} {
		if !strings.Contains(guide, want) {
			t.Fatalf("Hermes guide missing %q", want)
		}
	}
	for _, unwanted := range []string{"notice_delivery", "platforms.email.memory_notifications", "/sethome", "GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS=true", "gateway.log", "relay ws reconnected"} {
		if strings.Contains(guide, unwanted) {
			t.Fatalf("Hermes guide contains unsupported remedy %q", unwanted)
		}
	}
	for _, want := range []string{"Clients", `id="key-dialog"`, `id="key-form"`, `id="key-result"`, `id="key-copy"`, `data-type="hermes"`, "Hermes relay", "Add Client", `data-inbox-tab="connectors"`, `id="inbox-connectors-list"`, `id="key-openclaw-alpha"`, `id="key-hermes-guide"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	// The Add-client dialog and its ids must appear exactly once. A duplicated
	// copy makes the ids ambiguous for getElementById and the labels for/for
	// associations, so a regression that renders it twice is caught here.
	for _, id := range []string{"key-dialog", "key-form", "key-matrix", "key-type", "key-copy"} {
		if n := strings.Count(body, `id="`+id+`"`); n != 1 {
			t.Fatalf("dashboard has %d %q elements, want 1", n, id)
		}
	}
	// The per-dialog copy note was replaced by the shared JS copy helper; the
	// obsolete paragraph must not render.
	if strings.Contains(body, "key-copy-note") {
		t.Fatal("dashboard still renders the obsolete key-copy-note paragraph")
	}
	if !strings.Contains(body, `id="inbox-auto-trash"`) || !strings.Contains(body, `id="inbox-connectors-form"`) {
		t.Fatal("inbox-level delivery settings are missing from inbox settings")
	}
	assetReq := httptest.NewRequest("GET", "/assets/app.js", nil)
	assetRR := httptest.NewRecorder()
	h.ServeHTTP(assetRR, assetReq)
	asset := assetRR.Body.String()
	if assetRR.Code != http.StatusOK ||
		!strings.Contains(asset, "section.remove();") ||
		!strings.Contains(asset, "insertBefore(bearerFields, webhookAuth.nextElementSibling)") ||
		!strings.Contains(asset, "key-form-scroll") ||
		!strings.Contains(asset, "connector-config-row") ||
		!strings.Contains(asset, "inboxSaveButton.setAttribute('form', editing ? 'inbox-connector-edit-form' : 'inbox-edit-form')") ||
		!strings.Contains(asset, "showInboxSubview") ||
		!strings.Contains(asset, "#key-dialog #key-form>.dialog-actions") ||
		!strings.Contains(asset, "openclawAlpha.hidden = sel.value !== 'openclaw'") ||
		// The Hermes guide carries the .env path, the replace-not-append trap and
		// the restart, so it must render for a Hermes creation and stay hidden for
		// every other kind.
		!strings.Contains(asset, "key-hermes-guide').hidden = sel.value !== 'hermes'") {
		t.Fatalf("connector setup script does not remove inbox-level controls: status=%d", assetRR.Code)
	}
	if strings.Contains(body, "<h2>Hermes Relay</h2>") {
		t.Fatal("standalone Hermes Relay card should be removed")
	}
	// The no-allow-list acknowledgement row carries an inline display:flex for
	// layout, and the page CSS is [hidden]{display:none!important}, so only the
	// hidden attribute can hide or reveal it. Both connector kinds must toggle
	// that attribute (never the inline style) and recompute the gate when their
	// inbox changes, or the checkbox is permanently invisible and Create stays
	// disabled with no way to override.
	for _, want := range []string{
		"ackRow.hidden = !warn",
		"hermesAckRow.hidden = true",
		"openclawAckRow.hidden = true",
		"hermesInbox.addEventListener('change', syncHermesRisk)",
		"hermesAck.addEventListener('change', syncHermesRisk)",
		"openclawInbox.addEventListener('change', syncHermesRisk)",
		"openclawAck.addEventListener('change', syncHermesRisk)",
	} {
		if !strings.Contains(asset, want) {
			t.Fatalf("connector no-allow-list risk gate is not wired: asset missing %q", want)
		}
	}
	if strings.Contains(body, "onclick=") {
		t.Fatal("inline event handlers are blocked by CSP and must not be used")
	}
}

// TestDashboardRendersClientDeleteDialog locks in that a client row routes its
// delete through the confirmation dialog (which names the client and type)
// rather than a data-confirm attribute, and that the delete route still works.
func TestDashboardRendersClientDeleteDialog(t *testing.T) {
	t.Parallel()
	svc, h, u, _, _ := httpFixture(t)
	key, _, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "Agent", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := uiSession(t, svc, u.ID)

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`id="client-delete-dialog"`,
		`id="client-delete-form"`,
		`id="client-delete-name"`,
		`id="client-delete-input"`,
		`id="client-delete-submit"`,
		`id="client-delete-copy"`,
		`class="secondary icon-btn danger open-delete-client" data-kind="keys" data-id="` + key.ID + `" data-name="Agent" data-type="API key"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}

	// The delete now requires the client's name to be retyped.
	post := httptest.NewRecorder()
	delReq := httptest.NewRequest("POST", "/ui/keys/"+key.ID+"/delete", strings.NewReader("_csrf="+csrf+"&confirm=Agent"))
	delReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	delReq.AddCookie(cookie)
	h.ServeHTTP(post, delReq)
	if post.Code != http.StatusSeeOther {
		t.Fatalf("delete key %d: %s", post.Code, post.Body.String())
	}
	if keys, _ := svc.Store.ListAPIKeys(context.Background(), u.AccountID); len(keys) != 0 {
		t.Fatal("key survived a correct confirmation")
	}
}

func TestCreateKeyReturnsJSONSecret(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{"type": {"api"}, "name": {"Agent"}, "_csrf": {csrf}, "role_" + box.ID: {"read"}}
	req := httptest.NewRequest("POST", "/ui/keys", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create client = %d body=%s", rr.Code, rr.Body.String())
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q, want no-store", cc)
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	if got["notice"] != "API key created" || !strings.Contains(got["label"], "will not be shown again") || got["secret"] == "" {
		t.Fatalf("unexpected response %#v", got)
	}
}

func TestCreateKeyRedirectsWithoutJSONAccept(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	form := url.Values{"type": {"api"}, "name": {"Agent"}, "_csrf": {csrf}, "role_" + box.ID: {"read"}}
	req := httptest.NewRequest("POST", "/ui/keys", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("create client = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestKeyFlashRendersCopyButton(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"create", "rotate"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			svc, h, u, _, box := httpFixture(t)
			cookie, csrf := uiSession(t, svc, u.ID)
			path := "/ui/keys"
			if action == "rotate" {
				key, _, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "Agent", false, map[string]string{box.ID: "read"})
				if err != nil {
					t.Fatal(err)
				}
				path += "/" + key.ID + "/rotate"
			}
			form := url.Values{"type": {"api"}, "name": {"Agent"}, "_csrf": {csrf}, "role_" + box.ID: {"read"}}
			req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.AddCookie(cookie)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusSeeOther || !strings.HasPrefix(rr.Header().Get("Location"), "/?_flash=") {
				t.Fatalf("key %s = %d, location=%q", action, rr.Code, rr.Header().Get("Location"))
			}
			location := rr.Header().Get("Location")
			get := func() *httptest.ResponseRecorder {
				req := httptest.NewRequest("GET", location, nil)
				req.AddCookie(cookie)
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, req)
				if rr.Code != http.StatusOK {
					t.Fatalf("dashboard = %d", rr.Code)
				}
				return rr
			}
			body := get().Body.String()
			match := regexp.MustCompile(`<pre>(mmm_[A-Za-z0-9_-]+)</pre><button type="button" class="secondary" data-copy-value="([^"]+)">Copy</button>`).FindStringSubmatch(body)
			if len(match) != 3 || match[1] != match[2] {
				t.Fatal("one-time secret needs a Copy button for exactly the displayed key")
			}
			if strings.Contains(get().Body.String(), match[1]) {
				t.Fatal("refresh exposed the one-time key again")
			}
		})
	}
}
