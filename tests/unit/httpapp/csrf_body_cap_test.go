package httpapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestWithCSRFCapsSessionPostBody proves every session POST body is bounded by
// a MaxBytesReader before the CSRF decorator parses it. A form deliberately
// padded past the configured message budget must not reach the handler in full:
// the typed confirmation placed beyond the cap cannot be parsed, so the guarded
// action is refused and the object survives. This is the position a
// memory-abuse request (an unbounded body buffered by ParseForm or by the
// multipart spool) would occupy.
func TestWithCSRFCapsSessionPostBody(t *testing.T) {
	t.Parallel()
	svc, h, u, d, _ := httpFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)

	// The same budget withCSRF applies (MaxMessageBytes plus headroom).
	var capBytes int64 = 26 << 20
	if svc.Config.MaxMessageBytes > 0 {
		capBytes = svc.Config.MaxMessageBytes + 1<<20
	}
	pad := strings.Repeat("A", int(capBytes)+1<<20)
	over := "_csrf=" + csrf + "&pad=" + pad + "&confirm=" + d.Name
	req := httptest.NewRequest("POST", "/ui/domains/"+d.ID+"/delete", strings.NewReader(over))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	// Either the CSRF token was cut with the body (403) or the parsed form lost
	// the confirmation beyond the cap (an error notice); in neither case may the
	// guarded delete have run.
	if rr.Code == http.StatusSeeOther {
		if strings.Contains(rr.Header().Get("Location"), "deleted-domain") {
			t.Fatalf("over-cap form executed the guarded delete: %d %s", rr.Code, rr.Header().Get("Location"))
		}
	}
	if _, err := svc.Store.GetDomain(ctx, u.AccountID, d.ID); err != nil {
		t.Fatal("domain was deleted by an over-cap request")
	}

	// Control: a normal-sized form with the same token parses in full, so the
	// cap does not break legitimate traffic.
	rr = domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/delete", url.Values{"_csrf": {csrf}, "confirm": {strings.ToUpper(d.Name)}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("normal delete after a capped attempt = %d body=%s", rr.Code, rr.Body.String())
	}
	if _, err := svc.Store.GetDomain(ctx, u.AccountID, d.ID); err == nil {
		t.Fatal("domain survived a correct out-of-band confirmation")
	}
}
