package httpapp_test

import (
	"html"
	"net/url"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/dkimsign"
)

// mxSendingGroup returns the Direct MX provider group from the sending dialog.
func mxSendingGroup(t *testing.T, page, domainID string) string {
	t.Helper()
	return providerGroupHTML(t, dialogHTML(t, page, "domain-sending-dialog-"+domainID), "mx")
}

// TestDKIMRecordShownInlineWithSigning proves the whole DKIM story lives in one
// place: switching signing on and saving shows the DNS record to publish inside
// the sending editor, with the private key available on demand rather than
// splashed on the page.
func TestDKIMRecordShownInlineWithSigning(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)

	// Before enabling, there is no record to publish.
	before := mxSendingGroup(t, domainGet(t, h, cookie, "/?domain="+d.ID).Body.String(), d.ID)
	if strings.Contains(before, "Publish this DNS record") {
		t.Error("the DKIM record is shown before signing is enabled")
	}

	rr := domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/sending", url.Values{
		"_csrf": {csrf}, "provider": {"mx"}, "cfg_mx_helo": {"mail.example.com"},
		"cfg_mx_dkim_enabled": {"on"},
	})
	if rr.Code != 303 {
		t.Fatalf("save %d %s", rr.Code, rr.Body.String())
	}
	// The save returns to the dashboard, not a separate page.
	if loc := rr.Header().Get("Location"); strings.Contains(loc, "/sending/dkim") {
		t.Fatalf("saving DKIM navigated to a separate page: %q", loc)
	}

	group := mxSendingGroup(t, domainGet(t, h, cookie, "/?domain="+d.ID).Body.String(), d.ID)
	if !strings.Contains(group, "Publish this DNS record") {
		t.Fatal("the DKIM record is not shown after enabling signing")
	}
	if !strings.Contains(group, "TXT") {
		t.Error("the record does not state its type")
	}
	wantName := dkimsign.RecordName(dkimsign.DefaultSelector, d.Name)
	flat := html.UnescapeString(group)
	if !strings.Contains(flat, wantName) {
		t.Errorf("the record name %q is not shown", wantName)
	}
	if !strings.Contains(flat, "v=DKIM1; k=rsa; p=") {
		t.Error("the record value is not shown")
	}
	// The private key must not be on the page; it is behind an explicit reveal.
	if strings.Contains(group, "PRIVATE KEY") {
		t.Error("the private key is rendered into the sending dialog")
	}
	if !strings.Contains(group, "Show private key") {
		t.Error("the page does not offer the private key on demand")
	}
}

// TestDKIMSigningOffHidesTheRecord proves the panel is dependent: once a key
// exists, turning signing off leaves the panel in the markup but hidden, so the
// record is neither shown nor submitted.
func TestDKIMSigningOffHidesTheRecord(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)

	// Generate a key by enabling signing.
	domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/sending", url.Values{
		"_csrf": {csrf}, "provider": {"mx"}, "cfg_mx_helo": {"mail.example.com"},
		"cfg_mx_dkim_enabled": {"on"},
	})
	if !strings.Contains(mxSendingGroup(t, domainGet(t, h, cookie, "/?domain="+d.ID).Body.String(), d.ID), "data-dkim-panel") {
		t.Fatal("the DKIM panel is not in the markup after a key exists")
	}

	// Turning signing off removes the key, so there is no record to show.
	domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/sending", url.Values{
		"_csrf": {csrf}, "provider": {"mx"}, "cfg_mx_helo": {"mail.example.com"},
		"cfg_mx_dkim_enabled": {"off"},
	})
	group := mxSendingGroup(t, domainGet(t, h, cookie, "/?domain="+d.ID).Body.String(), d.ID)
	if strings.Contains(group, "Publish this DNS record") {
		t.Error("the record is shown with signing off")
	}
}

// TestDKIMRevealIsCSRFProtectedAndSingleUse proves the private key can be
// retrieved deliberately, but not by a bare navigation and not twice.
func TestDKIMRevealIsCSRFProtectedAndSingleUse(t *testing.T) {
	svc, h, u, d, _ := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)
	domainPost(t, h, cookie, "/ui/domains/"+d.ID+"/sending", url.Values{
		"_csrf": {csrf}, "provider": {"mx"}, "cfg_mx_helo": {"mail.example.com"},
		"cfg_mx_dkim_enabled": {"on"},
	})
	reveal := "/ui/domains/" + d.ID + "/sending/dkim/reveal"

	// Without CSRF the action is refused.
	if rr := domainPost(t, h, cookie, reveal, url.Values{}); rr.Code == 303 {
		t.Fatalf("reveal without CSRF was accepted: %d", rr.Code)
	}

	rr := domainPost(t, h, cookie, reveal, url.Values{"_csrf": {csrf}})
	if rr.Code != 303 {
		t.Fatalf("reveal %d %s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "_flash=") {
		t.Fatal("reveal did not carry a one-time flash")
	}
	first := domainGet(t, h, cookie, loc).Body.String()
	if !strings.Contains(first, "DKIM private key") || !strings.Contains(first, "PRIVATE KEY") {
		t.Error("the reveal did not show the private key pop-up")
	}
	// The flash is single-use.
	if again := domainGet(t, h, cookie, loc).Body.String(); strings.Contains(again, "PRIVATE KEY") {
		t.Error("the revealed private key survived a second read")
	}
}
