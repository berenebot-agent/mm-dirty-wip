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
		`dialog[id^="domain-receiving-dialog-"]`,
		"height:auto", "max-height:calc(100dvh - 2 * var(--setup-dialog-gutter))",
		"max-width:var(--setup-form-width)",
	} {
		if !strings.Contains(styles, want) {
			t.Errorf("shared modal sizing missing %q", want)
		}
	}
	// The inbox settings dialog is the tabbed shell: it keeps a bounded,
	// content-independent height so its panel area stays the single scrolling
	// region and the left navigation rail never stretches with a tall tab. It
	// must share the width rule but not the content-height rule.
	ruleStart := strings.Index(styles, "dialog.domain-wizard,dialog[id^=\"domain-sending-dialog-\"],dialog[id^=\"domain-receiving-dialog-\"]{")
	if ruleStart < 0 {
		t.Fatal("shared content-height rule missing")
	}
	rule := styles[ruleStart : strings.Index(styles[ruleStart:], "}")+ruleStart+1]
	if strings.Contains(rule, "dialog.inbox-settings[open]") {
		t.Error("content-height rule must not apply to dialog.inbox-settings[open]")
	}
	inboxRuleStart := strings.Index(styles, "dialog.inbox-settings[open]{")
	if inboxRuleStart < 0 {
		t.Fatal("inbox settings sizing rule missing")
	}
	inboxRule := styles[inboxRuleStart : strings.Index(styles[inboxRuleStart:], "}")+inboxRuleStart+1]
	if !strings.Contains(inboxRule, "height:min(90dvh,880px)") {
		t.Errorf("inbox settings dialog must keep a bounded height, got %q", inboxRule)
	}
	if !strings.Contains(inboxRule, "--setup-dialog-width") {
		t.Errorf("inbox settings dialog must share the shell width, got %q", inboxRule)
	}
	// Do not enlarge unrelated confirmations or one-time credential popups.
	for _, unwanted := range []string{"dialog.domain-dialog{", "domain-delete-dialog", "domain-credential-dialog", "dkim-key-dialog"} {
		if strings.Contains(styles, unwanted) {
			t.Errorf("shared modal sizing includes %q", unwanted)
		}
	}
}
