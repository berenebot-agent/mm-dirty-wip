package httpapp_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestSetupDialogsShareWidthAndUseContentHeight(t *testing.T) {
	t.Parallel()
	svc, h, u, _, _ := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	rr := domainGet(t, h, cookie, "/")
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard = %d", rr.Code)
	}
	body := rr.Body.String()
	// Anchored on the rule itself: html/template strips CSS comments from a
	// <style> block, so the source comment is not in the served page.
	start := strings.Index(body, ":root{--setup-dialog-width")
	if start < 0 {
		t.Fatal("shared setup dialog styles missing")
	}
	end := strings.Index(body[start:], ".provider-choice{")
	if end < 0 {
		t.Fatal("shared setup dialog styles have no boundary")
	}
	styles := body[start : start+end]
	for _, want := range []string{
		"--setup-dialog-width:1080px", "--setup-form-width:640px",
		"dialog.domain-wizard", `dialog[id^="domain-sending-dialog-"]`,
		`dialog[id^="domain-receiving-dialog-"]`, "dialog.inbox-settings[open]",
		"height:auto", "max-height:calc(100dvh - 2 * var(--setup-dialog-gutter))",
		"max-width:var(--setup-form-width)",
	} {
		if !strings.Contains(styles, want) {
			t.Errorf("shared modal sizing missing %q", want)
		}
	}
	// Do not enlarge unrelated confirmations or one-time credential popups.
	for _, unwanted := range []string{"dialog.domain-dialog{", "domain-delete-dialog", "domain-credential-dialog", "dkim-key-dialog", "height:880px"} {
		if strings.Contains(styles, unwanted) {
			t.Errorf("shared modal sizing includes %q", unwanted)
		}
	}
}
