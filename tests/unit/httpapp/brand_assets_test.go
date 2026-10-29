package httpapp_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mammothmail/internal/httpapp"
)

func TestBrandAssetsServed(t *testing.T) {
	t.Parallel()
	_, h, _, _, _ := httpFixture(t)
	for _, tc := range []struct {
		path string
		ct   string
	}{
		{"/favicon.ico", "image/x-icon"},
		{"/favicon-16x16.png", "image/png"},
		{"/favicon-32x32.png", "image/png"},
		{"/apple-touch-icon.png", "image/png"},
		{"/assets/logo-horizontal.png", "image/png"},
		{"/assets/tusk-mx-diagram.svg", "image/svg+xml"},
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

// The Tusk MX setup wizard opens with a short service intro and the mail-flow
// diagram, mirroring docs/EMAIL_PROVIDERS.md. The wizard markup is built in
// app.js, so assert the intro and its asset reference survive there.
func TestTuskWizardIntro(t *testing.T) {
	t.Parallel()
	bundle := string(httpapp.AppJS())
	for _, want := range []string{
		"/assets/tusk-mx-diagram.svg",
		"free hosted SMTP receiver for your domain",
		"outbound connection",
		"Tusk MX is a community service, provided on a best-efforts basis",
	} {
		if !strings.Contains(bundle, want) {
			t.Fatalf("Tusk wizard intro missing %q", want)
		}
	}
}

func TestHeaderUsesLogoHomeLink(t *testing.T) {
	t.Parallel()
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
	if strings.Contains(body, "<b>Mammoth Mail</b>") || strings.Contains(body, "<b>Mammoth Mail</b>") {
		t.Fatalf("header still renders a text brand label")
	}
}
