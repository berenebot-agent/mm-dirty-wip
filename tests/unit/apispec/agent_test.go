package apispec_test

import (
	"strings"
	"testing"

	"gatehouse-mail/internal/apispec"
)

// guideFixture exercises group headings, the role suffix and a plain route
// bullet without depending on the real table.
func guideFixture() []apispec.Route {
	return []apispec.Route{
		{Method: "GET", Path: "/v1/bootstrap", Summary: "Discover key capabilities and accessible inboxes", Group: "Bootstrap"},
		{Method: "GET", Path: "/v1/inboxes", Summary: "List inboxes", Group: "Inboxes"},
		{Method: "POST", Path: "/v1/drafts/{id}/approve", Summary: "Approve and send a pending draft", Role: "Owner", Group: "Draft approval"},
	}
}

func TestRenderAgentGuideHeadingsAndRoutes(t *testing.T) {
	guide := apispec.RenderAgentGuide(guideFixture())
	if !strings.HasPrefix(guide, "# Gatehouse Mail\n\n") {
		t.Fatalf("guide does not start with the header: %q", guide)
	}
	if !strings.Contains(guide, "Authenticate with `Authorization: Bearer <key>`.") {
		t.Fatal("missing authentication line")
	}
	if !strings.Contains(guide, "GET /v1/bootstrap") {
		t.Fatal("missing bootstrap pointer")
	}
	for _, heading := range []string{"## Bootstrap", "## Inboxes", "## Draft approval"} {
		if !strings.Contains(guide, heading) {
			t.Fatalf("missing group heading %q", heading)
		}
	}
	if !strings.Contains(guide, "- `GET /v1/bootstrap` — Discover key capabilities and accessible inboxes") {
		t.Fatal("missing bootstrap route line")
	}
	if !strings.Contains(guide, "- `POST /v1/drafts/{id}/approve` — Approve and send a pending draft (Owner)") {
		t.Fatal("missing role suffix on route line")
	}
}

func TestRenderAgentGuideRetainsNarrative(t *testing.T) {
	guide := apispec.RenderAgentGuide(guideFixture())
	for _, keyword := range []string{
		"pending_approval",
		"Idempotency-Key",
		"APPROVAL_EXPIRY_HOURS",
		"decision_method",
		"draft.approval_expired",
		"approver_email",
	} {
		if !strings.Contains(guide, keyword) {
			t.Fatalf("guide dropped narrative keyword %q", keyword)
		}
	}
}
