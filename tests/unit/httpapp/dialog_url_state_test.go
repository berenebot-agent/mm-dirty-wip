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

	// An unknown tab is coerced to the default aliases tab.
	rr = domainGet(t, h, cookie, "/?inbox="+box.ID+"&inbox_tab=bogus")
	if body = rr.Body.String(); !strings.Contains(body, `data-open-inbox-tab="aliases"`) {
		t.Fatalf("unknown inbox_tab not coerced to aliases")
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

	// A domain dialog opens only for a matching domain id and kind.
	rr = domainGet(t, h, cookie, "/?domain="+d.ID+"&kind=sending")
	if body = rr.Body.String(); !strings.Contains(body, `id="domain-sending-dialog-`+d.ID+`" class="domain-dialog" data-open="1"`) {
		t.Fatalf("sending dialog not marked data-open for %s", d.ID)
	}
	if strings.Contains(body, `id="domain-receiving-dialog-`+d.ID+`" class="domain-dialog" data-open="1"`) {
		t.Fatalf("receiving dialog wrongly marked data-open")
	}

	// An unknown domain id is dropped.
	rr = domainGet(t, h, cookie, "/?domain=does-not-exist&kind=sending")
	if strings.Contains(rr.Body.String(), `class="domain-dialog" data-open="1"`) {
		t.Fatalf("stale domain id should not open any dialog")
	}
}
