package httpapp_test

import (
	"strings"
	"testing"
)

// These tests lock the server contract that the dashboard's URL-driven dialogs
// depend on. The client strips these query params on dismiss so a refresh does
// not re-open a cancelled dialog; these assertions guard the rendering and
// validation the client relies on.
func TestUIDashboardDialogURLState(t *testing.T) {
	svc, h, u, d, box := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)

	// A valid inbox param opens that inbox's edit dialog on the requested tab.
	rr := domainGet(t, h, cookie, "/?inbox="+box.ID+"&inbox_tab=connectors")
	if rr.Code != 200 {
		t.Fatalf("dashboard %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `data-open-inbox="`+box.ID+`"`) {
		t.Fatalf("missing data-open-inbox for %s", box.ID)
	}
	if !strings.Contains(body, `data-open-inbox-tab="connectors"`) {
		t.Fatalf("missing data-open-inbox-tab=connectors")
	}

	// The inbox settings button omits inbox_tab and should open Identity.
	rr = domainGet(t, h, cookie, "/?inbox="+box.ID)
	if body = rr.Body.String(); !strings.Contains(body, `data-open-inbox-tab="basic"`) {
		t.Fatalf("missing default data-open-inbox-tab=basic")
	}

	// An unknown tab is coerced to the default Identity tab.
	rr = domainGet(t, h, cookie, "/?inbox="+box.ID+"&inbox_tab=bogus")
	if body = rr.Body.String(); !strings.Contains(body, `data-open-inbox-tab="basic"`) {
		t.Fatalf("unknown inbox_tab not coerced to basic")
	}

	// A stale or foreign inbox id is dropped so nothing re-opens.
	rr = domainGet(t, h, cookie, "/?inbox=does-not-exist")
	if body = rr.Body.String(); !strings.Contains(body, `data-open-inbox=""`) {
		t.Fatalf("stale inbox id not dropped")
	}

	// The removed connector param must not resurface as a data attribute.
	rr = domainGet(t, h, cookie, "/?inbox="+box.ID+"&inbox_tab=connectors&connector=xyz")
	if strings.Contains(rr.Body.String(), "data-open-connector") {
		t.Fatalf("data-open-connector should no longer be rendered")
	}

	// A domain dialog opens only for a matching domain id and kind. The Direct MX
	// sending dialog also carries the wider layout class, so match the class set
	// rather than one exact string.
	rr = domainGet(t, h, cookie, "/?domain="+d.ID+"&kind=sending")
	body = rr.Body.String()
	sendingTag := `id="domain-sending-dialog-` + d.ID + `" class="domain-dialog`
	if !strings.Contains(body, sendingTag) {
		t.Fatalf("sending dialog missing for %s", d.ID)
	}
	if !strings.Contains(body, sendingTag+`" data-open="1"`) &&
		!strings.Contains(body, sendingTag+` domain-dialog--wide" data-open="1"`) {
		t.Fatalf("sending dialog not marked data-open for %s", d.ID)
	}
	recvOpen := `id="domain-receiving-dialog-` + d.ID + `" class="domain-dialog" data-open="1"`
	if strings.Contains(body, recvOpen) {
		t.Fatalf("receiving dialog wrongly marked data-open")
	}

	// An unknown domain id is dropped.
	rr = domainGet(t, h, cookie, "/?domain=does-not-exist&kind=sending")
	if strings.Contains(rr.Body.String(), `class="domain-dialog" data-open="1"`) {
		t.Fatalf("stale domain id should not open any dialog")
	}
}
