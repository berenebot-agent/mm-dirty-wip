package apispec_test

import (
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/apispec"
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
	if !strings.HasPrefix(guide, "# MailMoose\n\n") {
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
		"GET /openapi.json",
		"Quick start",
		"can_send",
		"sender_not_allowed",
		`{"error":"sender not allowed","code":"sender_not_allowed"}`,
		"Appendix: command security notes",
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

func TestRenderAgentGuidePutsQuickstartBeforeSecurityNotes(t *testing.T) {
	guide := apispec.RenderAgentGuide(guideFixture())
	quickstart := strings.Index(guide, "## Quick start")
	security := strings.Index(guide, "## Appendix: command security notes")
	if quickstart < 0 || security < 0 || quickstart > security {
		t.Fatalf("quickstart/security order is wrong: quickstart=%d security=%d", quickstart, security)
	}
	if strings.Index(guide, "## Quick start") > strings.Index(guide, "## Bootstrap") {
		t.Fatal("quickstart should precede the generated route reference")
	}
}

// TestRenderAgentGuideRecommendsStdlibClient guards the jq-less onboarding
// path: the Python client must be recommended as stdlib-only, the Bash client
// must state its jq prerequisite, and the canonical inline curl example must
// not end in a jq pipe.
func TestRenderAgentGuideRecommendsStdlibClient(t *testing.T) {
	guide := apispec.RenderAgentGuide(guideFixture())
	for _, want := range []string{
		"`GET /examples/python`",
		"standard library only",
		"requires both `curl` and `jq`",
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("guide missing jq-less guidance %q", want)
		}
	}
	// The canonical single-command example must contain no pipe: a pipe is the
	// chained-execution shape a host scanner flags. The prose may still *name*
	// the forbidden `| jq .` shape while telling agents not to use it.
	found := false
	for _, line := range strings.Split(guide, "\n") {
		if !strings.Contains(line, "$BASE/v1/send?wait=true") {
			continue
		}
		found = true
		if strings.Contains(line, "|") {
			t.Errorf("canonical inline example pipes its output: %q", line)
		}
	}
	if !found {
		t.Fatal("guide has no inline curl send example")
	}
}
