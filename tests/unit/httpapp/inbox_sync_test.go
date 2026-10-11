package httpapp_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
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
	body := rr.Body.String()
	if !strings.Contains(body, `formaction="/ui/inboxes/`+standalone.ID+`/sync"`) {
		t.Fatal("standalone inbox toolbar missing the sync button")
	}
	// Regression: the sync control must be a button inside the bulk form (using
	// formaction), not its own nested form — a nested form is dropped by the
	// browser, so the button submitted the bulk form and hit "unknown action".
	// It must also sit after the bulk form opens, in the read/unread/trash row.
	syncIdx := strings.Index(body, `formaction="/ui/inboxes/`+standalone.ID+`/sync"`)
	bulkIdx := strings.Index(body, `id="bulk-form"`)
	if syncIdx < 0 || bulkIdx < 0 || syncIdx < bulkIdx {
		t.Fatalf("sync button must be inside the bulk form (sync=%d bulk=%d)", syncIdx, bulkIdx)
	}
	rr = uiGet(t, h, cookie, "/ui/inboxes/"+domainBox.ID)
	if strings.Contains(rr.Body.String(), "/sync\"") {
		t.Fatal("domain inbox toolbar should not show the sync button")
	}
}

// renderedSyncBefore extracts the value of the reload form's hidden `before`
// field exactly as the browser would submit it. It finds the hidden input that
// carries name="before", which the sync view fields render inside the bulk form.
func renderedSyncBefore(t *testing.T, body string) string {
	t.Helper()
	const name = `name="before"`
	i := strings.Index(body, name)
	if i < 0 {
		t.Fatal("reload form has no hidden before field")
	}
	rest := body[i+len(name):]
	// Advance to the value attribute of this input.
	vi := strings.Index(rest, `value="`)
	if vi < 0 {
		t.Fatal("reload before field has no value attribute")
	}
	rest = rest[vi+len(`value="`):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		t.Fatal("reload before field value is unterminated")
	}
	return rest[:j]
}

// seedPaginatedRemote indexes count deterministic messages in folder, oldest
// last, with distinct dates so pagination is stable, then refreshes the index so
// the UI list is served from the cache.
func seedPaginatedRemote(t *testing.T, h http.Handler, svc *app.Service, u model.User, fake *fakeIMAP, inbox model.Inbox, folder string, count int) {
	t.Helper()
	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for i := 0; i < count; i++ {
		fake.addAt(folder,
			fmt.Sprintf("From: a@b.test\r\nSubject: msg-%03d\r\nMessage-ID: <m%d@remote>\r\n\r\nbody", i, i),
			fmt.Sprintf("<m%d@remote>", i),
			fmt.Sprintf("msg-%03d", i),
			base.Add(-time.Duration(i)*time.Hour))
	}
	refreshRemote(t, h, inbox.ID, adminKey(t, svc, u))
}

// TestReloadNewestPageStaysNewest is the regression guard for the reported bug:
// pressing reload on the newest page submitted the page's OUTGOING (next-page)
// cursor, so it acted like "Load older" and hid the newest messages. The reload
// form must submit the INCOMING cursor (empty on the newest page).
func TestReloadNewestPageStaysNewest(t *testing.T) {
	svc, h, u, _, standalone, fake := standaloneFixture(t)
	// More than one page (inboxPageSize = 50) so a next-page cursor exists.
	seedPaginatedRemote(t, h, svc, u, fake, standalone, "INBOX", 60)
	cookie, csrf := uiSession(t, svc, u.ID)

	rr := uiGet(t, h, cookie, "/ui/inboxes/"+standalone.ID)
	if rr.Code != 200 {
		t.Fatalf("inbox page = %d", rr.Code)
	}
	body := rr.Body.String()
	// The newest page shows msg-000 and not the older page's msg-059.
	if !strings.Contains(body, "msg-000") || strings.Contains(body, "msg-059") {
		t.Fatalf("newest page is wrong before reload")
	}
	// The outgoing next-page cursor exists (so the bug is reachable).
	next := renderedCursor(t, body)
	if next == "" {
		t.Fatal("expected a next-page cursor on the first page")
	}
	// The reload form must submit the INCOMING cursor, which is empty here.
	if got := renderedSyncBefore(t, body); got != "" {
		t.Fatalf("reload submits the outgoing cursor %q on the newest page; want empty", got)
	}
	// Submitting the rendered form's value must return to the newest page.
	rr = uiPost(t, h, cookie, "/ui/inboxes/"+standalone.ID+"/sync", "_csrf="+csrf+"&folder=inbox&before=")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("sync = %d", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/ui/inboxes/"+standalone.ID {
		t.Fatalf("newest-page reload redirected to %q; want the newest page", loc)
	}
	rr = uiGet(t, h, cookie, rr.Header().Get("Location"))
	if got := rr.Body.String(); !strings.Contains(got, "msg-000") || strings.Contains(got, "msg-059") {
		t.Fatalf("newest messages disappeared after reload")
	}
}

// TestReloadDoesNotWalkBackwards presses reload repeatedly on the newest page and
// asserts each press stays on the newest page rather than advancing through
// older pages.
func TestReloadDoesNotWalkBackwards(t *testing.T) {
	svc, h, u, _, standalone, fake := standaloneFixture(t)
	seedPaginatedRemote(t, h, svc, u, fake, standalone, "INBOX", 120)
	cookie, csrf := uiSession(t, svc, u.ID)

	path := "/ui/inboxes/" + standalone.ID
	for press := 0; press < 3; press++ {
		rr := uiGet(t, h, cookie, path)
		if rr.Code != 200 {
			t.Fatalf("press %d page = %d", press, rr.Code)
		}
		body := rr.Body.String()
		if got := renderedSyncBefore(t, body); got != "" {
			t.Fatalf("press %d: reload submits %q; want empty on the newest page", press, got)
		}
		if !strings.Contains(body, "msg-000") {
			t.Fatalf("press %d: newest message missing", press)
		}
		rr = uiPost(t, h, cookie, "/ui/inboxes/"+standalone.ID+"/sync", "_csrf="+csrf+"&folder=inbox&before=")
		if loc := rr.Header().Get("Location"); loc != path {
			t.Fatalf("press %d: redirect %q walked away from the newest page", press, loc)
		}
	}
}

// TestReloadOlderPageStaysOnThatPage proves reload on an older page preserves
// that page's INCOMING cursor rather than advancing to the following page.
func TestReloadOlderPageStaysOnThatPage(t *testing.T) {
	svc, h, u, _, standalone, fake := standaloneFixture(t)
	seedPaginatedRemote(t, h, svc, u, fake, standalone, "INBOX", 120)
	cookie, csrf := uiSession(t, svc, u.ID)

	// Page 1 -> its outgoing cursor is page 2's incoming cursor.
	rr := uiGet(t, h, cookie, "/ui/inboxes/"+standalone.ID)
	incoming := renderedCursor(t, rr.Body.String())
	if incoming == "" {
		t.Fatal("no pagination cursor on the first page")
	}
	// Load page 2 with that cursor.
	page2 := "/ui/inboxes/" + standalone.ID + "?before=" + incoming
	rr = uiGet(t, h, cookie, page2)
	body := rr.Body.String()
	if rr.Code != 200 || !strings.Contains(body, "msg-") {
		t.Fatalf("page 2 did not render")
	}
	if strings.Contains(body, "msg-000") {
		t.Fatal("page 2 unexpectedly shows the newest message")
	}
	// The reload form on page 2 must submit page 2's INCOMING cursor (incoming),
	// not page 2's own outgoing cursor.
	if got := renderedSyncBefore(t, body); got != incoming {
		t.Fatalf("reload on page 2 submits %q; want the page's incoming cursor %q", got, incoming)
	}
	rr = uiPost(t, h, cookie, "/ui/inboxes/"+standalone.ID+"/sync", "_csrf="+csrf+"&folder=inbox&before="+incoming)
	loc := rr.Header().Get("Location")
	if loc != page2 {
		t.Fatalf("older-page reload redirected to %q; want %q", loc, page2)
	}
	rr = uiGet(t, h, cookie, loc)
	if got := rr.Body.String(); strings.Contains(got, "msg-000") {
		t.Fatal("reload on an older page jumped to the newest page or a later page")
	}
}

// renderedCursor extracts the live list's data-cursor attribute, which is the
// view's outgoing (next-page) cursor.
func renderedCursor(t *testing.T, body string) string {
	t.Helper()
	const marker = `data-cursor="`
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	rest := body[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		t.Fatal("data-cursor value is unterminated")
	}
	return rest[:j]
}

// TestReloadCustomFolderPreservesView proves the custom-folder view behaves the
// same: reload stays on the folder's newest page instead of advancing.
func TestReloadCustomFolderPreservesView(t *testing.T) {
	svc, h, u, _, standalone, fake := standaloneFixture(t)
	// Archive is a custom (non-system) folder in the fixture.
	seedPaginatedRemote(t, h, svc, u, fake, standalone, "Archive", 60)
	cookie, csrf := uiSession(t, svc, u.ID)

	folders, err := svc.Store.ListFolders(context.Background(), u.AccountID, standalone.ID)
	if err != nil {
		t.Fatal(err)
	}
	var archiveID string
	for _, f := range folders {
		if f.Path == "Archive" {
			archiveID = f.ID
		}
	}
	if archiveID == "" {
		t.Fatal("Archive folder not indexed")
	}
	view := "/ui/inboxes/" + standalone.ID + "/folder?folder=" + archiveID
	rr := uiGet(t, h, cookie, view)
	if rr.Code != 200 {
		t.Fatalf("folder view = %d", rr.Code)
	}
	body := rr.Body.String()
	if got := renderedSyncBefore(t, body); got != "" {
		t.Fatalf("custom-folder reload submits %q; want empty on its newest page", got)
	}
	if !strings.Contains(body, "/folder?folder="+archiveID) {
		t.Fatal("custom folder link missing from the rendered view")
	}
	// Reload must preserve the custom folder.
	rr = uiPost(t, h, cookie, "/ui/inboxes/"+standalone.ID+"/sync", "_csrf="+csrf+"&folder=folder&before=&folder_id="+archiveID)
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "/folder?folder="+archiveID) {
		t.Fatalf("custom-folder reload redirected to %q", loc)
	}
	rr = uiGet(t, h, cookie, loc)
	if got := rr.Body.String(); !strings.Contains(got, "msg-000") {
		t.Fatal("custom-folder reload lost the newest messages")
	}
}
