package httpapp_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrandAssetsServed(t *testing.T) {
	_, h, _, _, _ := httpFixture(t)
	for _, tc := range []struct {
		path string
		ct   string
	}{
		{"/favicon.ico", "image/x-icon"},
		{"/favicon.svg", "image/svg+xml"},
		{"/apple-touch-icon.png", "image/png"},
		{"/assets/logo-horizontal.png", "image/png"},
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", tc.path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s = %d", tc.path, rr.Code)
		}
		if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, tc.ct) {
			t.Fatalf("%s content-type = %q, want %q", tc.path, got, tc.ct)
		}
	}
}

func TestHeaderUsesLogoHomeLink(t *testing.T) {
	_, h, _, _, _ := httpFixture(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/login", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/login = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "logo-horizontal.png") {
		t.Fatalf("header missing logo image")
	}
	if !strings.Contains(body, `rel="icon"`) {
		t.Fatalf("head missing favicon link")
	}
	if strings.Contains(body, "<b>Gatehouse Email</b>") || strings.Contains(body, "<b>Gatehouse Mail</b>") {
		t.Fatalf("header still renders a text brand label")
	}
}
