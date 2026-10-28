package httpapp_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dellarb/mammothmail/internal/httpapp"
)

func TestInviteSharesRegistrationRateLimit(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := httpFixture(t)
	svc.Config.RegisterLimitPerMinute = 2
	svc.Config.AllowRegistration = true
	h := httpapp.New(svc, nil).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/login", nil))
	var csrf *http.Cookie
	for _, cookie := range rr.Result().Cookies() {
		if cookie.Name == "mmm_csrf" {
			csrf = cookie
		}
	}
	if csrf == nil {
		t.Fatal("login did not issue a CSRF cookie")
	}
	form := url.Values{"_csrf": {csrf.Value}, "password": {"correct horse battery staple"}, "confirm_password": {"correct horse battery staple"}}
	for i, path := range []string{"/invite/unknown", "/invite/unknown", "/invite/unknown", "/register"} {
		req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(csrf)
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		want := http.StatusOK
		if i >= 2 {
			want = http.StatusTooManyRequests
		}
		if rr.Code != want {
			t.Fatalf("attempt %d %s = %d, want %d: %s", i, path, rr.Code, want, rr.Body.String())
		}
	}
}
