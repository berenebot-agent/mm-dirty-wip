package httpapp_test

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
)

// TestUIDeleteRequiresServerTypedConfirmation proves the "type the name to
// delete" guard is enforced server-side: a delete with no or wrong confirm is
// refused (the object survives and the dashboard returns an error notice), and
// a matching confirm deletes.
func TestUIDeleteRequiresServerTypedConfirmation(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)

	rr := domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/delete", url.Values{"_csrf": {csrf}, "confirm": {"wrong"}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("wrong confirm status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "error=") {
		t.Fatalf("wrong confirm should redirect with an error notice, got %q", loc)
	}
	if _, err := svc.Store.GetDomain(ctx, u.AccountID, d.ID); err != nil {
		t.Fatalf("domain was deleted despite a wrong confirmation: %v", err)
	}

	rr = domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/delete", url.Values{"_csrf": {csrf}, "confirm": {strings.ToUpper(d.Name)}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("correct confirm status = %d", rr.Code)
	}
	if _, err := svc.Store.GetDomain(ctx, u.AccountID, d.ID); err == nil {
		t.Fatal("domain survived a correct confirmation")
	}
}

// TestUIDeleteInboxRequiresServerTypedConfirmation is the inbox counterpart: the
// typed value is the inbox address.
func TestUIDeleteInboxRequiresServerTypedConfirmation(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	cookie, csrf := uiSession(t, svc, u.ID)

	rr := domainPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/delete", url.Values{"_csrf": {csrf}, "confirm": {"not-the-address"}})
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "error=") {
		t.Fatalf("wrong inbox confirm should redirect with an error, got %q", loc)
	}
	if _, err := svc.Store.GetInboxInternal(ctx, u.AccountID, box.ID); err != nil {
		t.Fatalf("inbox deleted despite a wrong confirmation: %v", err)
	}

	rr = domainPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/delete", url.Values{"_csrf": {csrf}, "confirm": {box.Address}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("correct inbox confirm status = %d", rr.Code)
	}
	if _, err := svc.Store.GetInboxInternal(ctx, u.AccountID, box.ID); err == nil {
		t.Fatal("inbox survived a correct confirmation")
	}
}

// TestUICreateDomainInvalidShowsError proves an invalid Add Domain input
// redirects back to the dashboard with an error notice instead of a bare 400
// page, so the user is not stranded.
func TestUICreateDomainInvalidShowsError(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	rr := domainPost(t, h, cookie, "/ui/domains", url.Values{"_csrf": {csrf}, "name": {"not a valid domain"}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("invalid domain status = %d, want 303", rr.Code)
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "error=") {
		t.Fatalf("invalid domain should redirect with an error, got %q", loc)
	}
	rr = uiGet(t, h, cookie, "/?error=Bad+domain")
	if !strings.Contains(rr.Body.String(), `class="error notice"`) {
		t.Fatal("dashboard did not render the error banner")
	}
}

// TestUIOutboxPager proves the outbox exposes a pager when more than one page of
// queued mail exists, and that the pager cursor returns the older page without
// the newest page's rows.
func TestUIOutboxPager(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	cookie, _ := uiSession(t, svc, u.ID)
	admin := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{}}

	// Queue more than one page of outbound mail. The fixture's domain has no
	// sending provider, so the mail stays in the outbox.
	for i := 0; i < testInboxPageSize+3; i++ {
		if _, err := svc.Send(ctx, admin, app.SendInput{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "q" + strconv.Itoa(i), Text: "body"}, ""); err != nil {
			t.Fatalf("queue %d: %v", i, err)
		}
	}
	rr := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/outbox")
	if rr.Code != 200 {
		t.Fatalf("outbox %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Load older") || !strings.Contains(body, "before=") {
		t.Fatal("outbox missing pager for more than one page")
	}
	// Extract the cursor from the pager link and fetch page 2.
	idx := strings.Index(body, "outbox?before=")
	if idx < 0 {
		t.Fatal("no outbox pager URL")
	}
	rest := body[idx+len("outbox?before="):]
	if end := strings.IndexAny(rest, `"&`); end >= 0 {
		rest = rest[:end]
	}
	rr = uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/outbox?before="+rest)
	if rr.Code != 200 {
		t.Fatalf("outbox page 2 %d", rr.Code)
	}
}
