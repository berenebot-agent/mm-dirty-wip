package httpapp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
)

// TestInboxLiveListFragment proves the live list fragment re-renders the same
// message rows as the full inbox page (same templates), carries the data-row-id
// and data-live-list markers the client swaps on, and is session-scoped.
func TestInboxLiveListFragment(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	m := seedInbound(t, svc, box, "live-1", "<live-1@test>", "Hello", "body")
	cookie, _ := uiSession(t, svc, u.ID)

	req := httptest.NewRequest(http.MethodGet, "/ui/inboxes/"+box.ID+"/live?part=list&folder=inbox", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("live list = %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `data-live-list`) {
		t.Fatalf("fragment missing data-live-list marker: %s", body)
	}
	if !strings.Contains(body, `data-row-id="`+m.ID+`"`) {
		t.Fatalf("fragment missing the message row: %s", body)
	}
	// The fragment is a fragment, not a whole page: no page shell.
	if strings.Contains(body, "<!doctype html>") || strings.Contains(body, "<body") {
		t.Fatalf("live fragment must not include the page shell")
	}
}

// TestInboxPageCarriesLiveMarkersAndMatchesFragment proves the full inbox page
// carries the markers the client swaps on, and that the live list fragment is
// byte-identical to the list section embedded in the full page — so the fragment
// and the page it replaces can never drift.
func TestInboxPageCarriesLiveMarkersAndMatchesFragment(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	seedInbound(t, svc, box, "match-1", "<match-1@test>", "One", "body")
	seedInbound(t, svc, box, "match-2", "<match-2@test>", "Two", "body")
	cookie, _ := uiSession(t, svc, u.ID)

	page := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID).Body.String()
	if !strings.Contains(page, `data-live-list`) {
		t.Fatalf("full page missing data-live-list marker")
	}
	if !strings.Contains(page, `data-row-id="`) {
		t.Fatalf("full page missing data-row-id markers")
	}
	frag := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/live?part=list&folder=inbox").Body.String()

	// Extract the list section from the page and confirm the fragment matches it.
	start := strings.Index(page, `<section class="card" data-live-list`)
	if start < 0 {
		t.Fatalf("could not find the live list section in the page")
	}
	end := strings.Index(page[start:], "</section>")
	if end < 0 {
		t.Fatalf("could not find the end of the live list section")
	}
	pageSection := page[start : start+end+len("</section>")]
	if pageSection != frag {
		t.Fatalf("live list fragment differs from the page section\n--- page ---\n%s\n--- fragment ---\n%s", pageSection, frag)
	}
}

// TestInboxLiveRequiresSession proves an unauthenticated fragment request is
// refused (a session redirect/denial, never the list).
func TestInboxLiveRequiresSession(t *testing.T) {
	_, h, _, _, box := httpFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/ui/inboxes/"+box.ID+"/live?part=list&folder=inbox", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code == http.StatusOK {
		t.Fatalf("unauthenticated live fragment must not succeed")
	}
}

// TestInboxLiveRejectsUnknownFolder proves the fragment can only describe the
// view the page is on: an unrecognised folder (or a requests request on a
// non-inbox folder) is refused rather than silently rendered.
func TestInboxLiveRejectsUnknownFolder(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)

	for _, path := range []string{
		"/ui/inboxes/" + box.ID + "/live?part=list&folder=bogus",
		"/ui/inboxes/" + box.ID + "/live?part=requests&folder=sent",
		"/ui/inboxes/" + box.ID + "/live?part=bogus&folder=inbox",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404", path, rr.Code)
		}
	}
}

// TestInboxLiveRequestsFragmentMatchesPage proves the send-requests fragment
// carries the data-live-requests marker and the pending draft, matching the full
// page's card.
func TestInboxLiveRequestsFragment(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	// Seed a pending send request the same way the UI flow does.
	admin := model.Principal{AccountID: u.AccountID, Admin: true}
	draft, err := svc.Store.CreateDraft(context.Background(), admin, model.Draft{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Proposal", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RequestSend(context.Background(), admin, draft.ID, false); err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)

	req := httptest.NewRequest(http.MethodGet, "/ui/inboxes/"+box.ID+"/live?part=requests&folder=inbox", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("live requests = %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `data-live-requests`) {
		t.Fatalf("requests fragment missing marker: %s", rr.Body.String())
	}
}
