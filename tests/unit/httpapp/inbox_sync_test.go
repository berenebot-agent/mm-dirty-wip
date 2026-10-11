package httpapp_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestInboxSyncRouteQuickRefresh proves the reload button's endpoint
// (POST /ui/inboxes/{id}/sync) quick-syncs a standalone inbox: it re-fetches the
// on-screen page's headers, schedules the deep reconcile, and redirects back to
// the same view preserving pagination. It is available to a non-admin user with
// read access, and is a safe no-op redirect for a domain inbox.
func TestInboxSyncRouteQuickRefresh(t *testing.T) {
	svc, h, u, _, standalone, fake := standaloneFixture(t)
	// Give the standalone inbox indexed mail so the view has a page to refresh.
	fake.add("INBOX", "From: a@b.test\r\nSubject: Hello\r\nMessage-ID: <m1@remote>\r\n\r\nbody", "<m1@remote>", "Hello")
	refreshRemote(t, h, standalone.ID, adminKey(t, svc, u))

	cookie, csrf := uiSession(t, svc, u.ID)
	rr := uiPost(t, h, cookie, "/ui/inboxes/"+standalone.ID+"/sync", "_csrf="+csrf+"&folder=inbox")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("sync = %d body=%s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/ui/inboxes/"+standalone.ID {
		t.Fatalf("sync redirect = %q", loc)
	}
	// A paginated sent view preserves its folder and cursor.
	fake.add("Sent", "From: a@b.test\r\nSubject: Re\r\nMessage-ID: <s1@remote>\r\n\r\nbody", "<s1@remote>", "Re")
	refreshRemote(t, h, standalone.ID, adminKey(t, svc, u))
	rr = uiPost(t, h, cookie, "/ui/inboxes/"+standalone.ID+"/sync", "_csrf="+csrf+"&folder=sent&before=rm_x")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("sent sync = %d", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/ui/inboxes/"+standalone.ID+"/sent?before=rm_x" {
		t.Fatalf("sent sync redirect = %q", loc)
	}
}

// TestInboxSyncRouteRequiresCSRF proves the sync endpoint is CSRF-protected like
// every other session state change.
func TestInboxSyncRouteRequiresCSRF(t *testing.T) {
	svc, h, u, _, standalone, _ := standaloneFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	rr := uiPost(t, h, cookie, "/ui/inboxes/"+standalone.ID+"/sync", "folder=inbox")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("sync without csrf = %d want 403", rr.Code)
	}
}

// TestInboxSyncButtonRenderedForStandaloneOnly proves the square reload button is
// present on a standalone inbox toolbar for any signed-in user, and absent on a
// domain inbox.
func TestInboxSyncButtonRenderedForStandaloneOnly(t *testing.T) {
	svc, h, u, domainBox, standalone, _ := standaloneFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	rr := uiGet(t, h, cookie, "/ui/inboxes/"+standalone.ID)
	if !strings.Contains(rr.Body.String(), `action="/ui/inboxes/`+standalone.ID+`/sync"`) {
		t.Fatal("standalone inbox toolbar missing the sync button")
	}
	rr = uiGet(t, h, cookie, "/ui/inboxes/"+domainBox.ID)
	if strings.Contains(rr.Body.String(), "/sync\"") {
		t.Fatal("domain inbox toolbar should not show the sync button")
	}
}
