package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/config"
	"gatehouse-mail/internal/events"
	"gatehouse-mail/internal/httpapp"
	"gatehouse-mail/internal/store"
)

// forceHTTPSHandler builds a handler whose config forces HTTPS, for testing the
// redirect and the advertised scheme without a TLS socket.
func forceHTTPSHandler(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Config{DataDir: dir, BaseURL: "https://mail.example.test", Mode: "selfhosted", ForceHTTPS: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 20, SendLimitPerMinute: 60}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	return httpapp.New(svc, nil).Handler()
}

func adminToken(t *testing.T, svc *app.Service, accountID string) string {
	t.Helper()
	_, token, err := svc.Store.CreateAPIKey(context.Background(), accountID, "test", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// TestMessagesWaitDoesNotReplayBacklog is the regression for the long-poll
// returning the oldest message instantly: with no cursor it must block for a
// new message, not hand back history.
func TestMessagesWaitDoesNotReplayBacklog(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	token := adminToken(t, svc, u.AccountID)
	if _, _, err := svc.IngestInbound(context.Background(), "mailgun", signedMGRequest(t, testMailgunKey, "wait-seed", box.Address, inboundRawMessage())); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/messages/wait?inbox="+box.ID+"&timeout=1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("wait = %d body=%s, want 204 (must not replay backlog)", rr.Code, rr.Body.String())
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Fatalf("wait returned in %s; it did not block", time.Since(start))
	}
}

// TestWaitRejectsMalformedCursor asserts a non-evt_ cursor is a 400 rather than
// silently treated as the start of history.
func TestWaitRejectsMalformedCursor(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	token := adminToken(t, svc, u.AccountID)
	for _, path := range []string{
		"/v1/messages/wait?after=bogus&timeout=1",
		"/v1/events/wait?after=bogus&timeout=1",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d body=%s, want 400", path, rr.Code, rr.Body.String())
		}
	}
}

func TestV1LimitsEndpoint(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	token := adminToken(t, svc, u.AccountID)
	req := httptest.NewRequest(http.MethodGet, "/v1/limits", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /v1/limits = %d body=%s", rr.Code, rr.Body.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["page_size_default"]; !ok {
		t.Fatalf("limits missing page_size_default: %#v", doc)
	}
}

func TestHealthAliasAndDocsRedirect(t *testing.T) {
	_, h, _, _, _ := httpFixture(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /health = %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != "/agent" {
		t.Fatalf("GET /docs = %d Location=%q, want 302 /agent", rr.Code, rr.Header().Get("Location"))
	}
}

// TestExamplesDefaultToRequestOrigin asserts a downloaded client points at the
// instance it came from instead of localhost.
func TestExamplesDefaultToRequestOrigin(t *testing.T) {
	_, h, _, _, _ := httpFixture(t)
	get := func(path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "mail.example.org"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		return rr.Body.String()
	}
	if body := get("/examples/bash"); !strings.Contains(body, `DEFAULT_BASE_URL="http://mail.example.org"`) {
		t.Fatalf("bash client did not adopt request origin")
	}
	if body := get("/examples/python"); !strings.Contains(body, `DEFAULT_BASE_URL = "http://mail.example.org"`) {
		t.Fatalf("python client did not adopt request origin")
	}
}

func TestForceHTTPSRedirectsAndAdvertisesHTTPS(t *testing.T) {
	h := forceHTTPSHandler(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/bootstrap", nil)
	req.Host = "mail.example.org"
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusPermanentRedirect {
		t.Fatalf("plaintext /v1/bootstrap = %d, want 308", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "https://mail.example.org/v1/bootstrap" {
		t.Fatalf("redirect Location = %q", loc)
	}

	// An untrusted X-Forwarded-Proto must not bypass the redirect.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/bootstrap", nil)
	req.Host = "mail.example.org"
	req.Header.Set("X-Forwarded-Proto", "https")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusPermanentRedirect {
		t.Fatalf("untrusted X-Forwarded-Proto bypassed redirect: %d", rr.Code)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("health must not redirect, got %d", rr.Code)
	}

	rr = httptest.NewRecorder()
	// Fetch over https (TLS set) so the forced redirect does not apply; the
	// advertised server must still be https.
	req = httptest.NewRequest(http.MethodGet, "https://mail.example.org/openapi.json", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json = %d", rr.Code)
	}
	var doc struct {
		Servers []struct {
			URL string `json:"url"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Servers) != 1 || doc.Servers[0].URL != "https://mail.example.org" {
		t.Fatalf("servers = %#v, want https", doc.Servers)
	}
}
