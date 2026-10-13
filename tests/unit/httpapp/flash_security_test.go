package httpapp_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type flashReadProbe struct{ read bool }

func (b *flashReadProbe) Read([]byte) (int, error) {
	b.read = true
	return 0, io.EOF
}

func TestFlashFabricatedCookieDoesNotReadBody(t *testing.T) {
	t.Parallel()
	_, h, _, _, box := httpFixture(t)
	for _, cookie := range []string{"", "x"} {
		body := &flashReadProbe{}
		r := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/send", body)
		r.Header.Set("Content-Type", "multipart/form-data; boundary=fake")
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: "mmm_session", Value: cookie})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if body.read || strings.Contains(w.Header().Get("Location"), "next=") {
			t.Fatalf("unverified cookie %q admitted compose body", cookie)
		}
	}
}

func TestFlashExpiredSessionAdmission(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	for _, tc := range []struct {
		name            string
		ttl             time.Duration
		revoke, badCSRF bool
	}{
		{name: "revoked", ttl: -time.Second, revoke: true},
		{name: "outside-grace", ttl: -16 * time.Minute},
		{name: "bad-csrf", ttl: -time.Second, badCSRF: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, csrf, err := svc.Store.CreateSession(t.Context(), u.ID, tc.ttl)
			if err != nil {
				t.Fatal(err)
			}
			if tc.revoke {
				svc.Store.DeleteSession(t.Context(), token)
			}
			if tc.badCSRF {
				csrf = "wrong"
			}
			buf, ct := multipartBody(t, map[string]string{"_csrf": csrf, "text": "must not persist"}, "", "")
			r := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/send", buf)
			r.Header.Set("Content-Type", ct)
			r.AddCookie(&http.Cookie{Name: "mmm_session", Value: token})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			loc, _ := url.Parse(w.Header().Get("Location"))
			// Non-draft auth notices are allowed, but a draft recovery destination
			// must never carry a token for an unverified form.
			if loc.Query().Get("next") != "" && loc.Query().Get("_flash") != "" {
				t.Fatal("invalid expired session or CSRF stored a draft")
			}
		})
	}
}

func TestFlashDraftOwnerAndAttachmentIsolation(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	other := accessMember(t, svc, u.AccountID, u.ID, "flash-other@example.net", box.ID)
	expired, csrf, err := svc.Store.CreateSession(t.Context(), u.ID, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	buf, ct := multipartBody(t, map[string]string{"_csrf": csrf, "text": "private draft text", "to": "friend@example.net"}, "private.txt", "private attachment")
	r := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/send", buf)
	r.Header.Set("Content-Type", ct)
	r.AddCookie(&http.Cookie{Name: "mmm_session", Value: expired})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	loc, _ := url.Parse(w.Header().Get("Location"))
	tok := loc.Query().Get("_flash")
	if tok == "" {
		t.Fatal("real expired session lost draft")
	}
	dest := "/ui/inboxes/" + box.ID + "/compose?_flash=" + tok
	otherCookie, _ := uiSession(t, svc, other.ID)
	get := func(cookie *http.Cookie) string {
		r := httptest.NewRequest("GET", dest, nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("compose: %d", w.Code)
		}
		return w.Body.String()
	}
	if b := get(otherCookie); strings.Contains(b, "private draft text") || strings.Contains(b, "private.txt") {
		t.Fatal("same-account user read another user's draft")
	}
	// Exercise the formerly unowned attachment-copy path on expiry too.
	otherExpired, otherCSRF, err := svc.Store.CreateSession(t.Context(), other.ID, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	buf, ct = multipartBody(t, map[string]string{"_csrf": otherCSRF, "_flash": tok, "text": "other draft"}, "", "")
	r = httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/send", buf)
	r.Header.Set("Content-Type", ct)
	r.AddCookie(&http.Cookie{Name: "mmm_session", Value: otherExpired})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	otherLoc, _ := url.Parse(w.Header().Get("Location"))
	dest = "/ui/inboxes/" + box.ID + "/compose?_flash=" + otherLoc.Query().Get("_flash")
	if b := get(otherCookie); !strings.Contains(b, "other draft") || strings.Contains(b, "private.txt") {
		t.Fatal("foreign flash attachments copied into expired draft")
	}
	dest = "/ui/inboxes/" + box.ID + "/compose?_flash=" + tok
	ownerCookie, _ := uiSession(t, svc, u.ID)
	if b := get(ownerCookie); !strings.Contains(b, "private draft text") || !strings.Contains(b, "private.txt") {
		t.Fatal("foreign access consumed draft or owner re-login lost it")
	}
}
