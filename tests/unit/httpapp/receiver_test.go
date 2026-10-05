package httpapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/httpapp"
)

func TestReceiverURLAndRoutes(t *testing.T) {
	svc, _, u, dom, _ := httpFixture(t)
	svc.Config.BaseURL = "https://mail.example.com"
	_, key, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, receiverURL := range []string{"", "https://receive.example.com"} {
		svc.Config.DedicatedReceiverURL = receiverURL
		s := httpapp.New(svc, nil)
		h := s.Handler()
		rr := adminDo(h, "GET", "/v1/admin/domains/"+dom.ID+"/receiving", "", key)
		want := svc.Config.ReceiverURL() + "/internal/ingest/"
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("receiving URLs: %d %s, want %s", rr.Code, rr.Body.String(), want)
		}
		for _, handler := range []http.Handler{h, s.InboundHandler()} {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest("GET", "/healthz", nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("health status = %d", rr.Code)
			}
			rr = httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest("POST", "/internal/ingest/cloudflare", strings.NewReader("invalid")))
			if rr.Code == http.StatusNotFound || rr.Code == http.StatusMethodNotAllowed || rr.Code >= 500 {
				t.Fatalf("webhook route unavailable: %d %s", rr.Code, rr.Body.String())
			}
		}
		for _, path := range []string{"/login", "/v1/inboxes", "/openapi.json", "/"} {
			rr := httptest.NewRecorder()
			s.InboundHandler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
			if rr.Code != http.StatusNotFound {
				t.Fatalf("receiver exposes %s: %d", path, rr.Code)
			}
		}
		cookie, csrf := uiSession(t, svc, u.ID)
		page := domainGet(t, h, cookie, "/?domain="+dom.ID+"&kind=receiving&provider=resend")
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), want+"resend") {
			t.Fatalf("receiving instructions missing receiver URL: %d", page.Code)
		}
		// Regeneration exercises the rendered Worker and its one-time setup URL.
		if _, _, err := svc.SaveDomainReceivingConfig(context.Background(), u.AccountID, dom.ID, "cloudflare", nil, false); err != nil {
			t.Fatal(err)
		}
		flash := domainPost(t, h, cookie, "/ui/domains/"+dom.ID+"/receiving/regenerate", url.Values{"_csrf": {csrf}})
		if flash.Code != http.StatusSeeOther {
			t.Fatalf("worker regeneration: %d %s", flash.Code, flash.Body.String())
		}
		page = domainGet(t, h, cookie, flash.Header().Get("Location"))
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "WEBHOOK_URL = ") || !strings.Contains(page.Body.String(), want+"cloudflare") {
			t.Fatalf("generated Worker missing receiver URL: %d", page.Code)
		}
	}
}
