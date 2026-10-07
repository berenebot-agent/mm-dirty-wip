package apispec_test

import (
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/apispec"
)

// TestRoutesCount pins the size of the canonical table to the live /v1
// registration count so a new transport route cannot be added without a
// matching table entry.
func TestRoutesCount(t *testing.T) {
	if got := len(apispec.Routes()); got != 98 {
		t.Fatalf("len(Routes()) = %d, want 98", got)
	}
}

// TestRoutesUnique rejects a method+path pair listed twice.
func TestRoutesUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range apispec.Routes() {
		key := r.Method + " " + r.Path
		if seen[key] {
			t.Errorf("duplicate route %q", key)
		}
		seen[key] = true
	}
}

// TestRouteFieldsPopulated checks that every entry is a usable /v1 operation.
// GET /v1/bootstrap is the one operation open to any authenticated principal,
// so its empty role is intentional rather than a missing value.
func TestRouteFieldsPopulated(t *testing.T) {
	validMethods := map[string]bool{
		"GET":    true,
		"POST":   true,
		"PUT":    true,
		"PATCH":  true,
		"DELETE": true,
	}
	for _, r := range apispec.Routes() {
		key := r.Method + " " + r.Path
		if r.Method == "" {
			t.Errorf("%s: empty method", key)
		} else if !validMethods[r.Method] {
			t.Errorf("%s: unexpected method %q", key, r.Method)
		}
		if r.Path == "" || !strings.HasPrefix(r.Path, "/v1/") {
			t.Errorf("%s: path must start with /v1/", key)
		}
		if r.Summary == "" {
			t.Errorf("%s: empty summary", key)
		}
		if r.Group == "" {
			t.Errorf("%s: empty group", key)
		}
		if r.Role == "" && r.Path != "/v1/bootstrap" {
			t.Errorf("%s: empty role", key)
		}
	}
}

// TestRoleValues keeps the advertised role within the set the auth model
// actually assigns, so an agent choosing operations from its bootstrap roles is
// not told "read" for an operation that requires Assistant or Owner.
func TestRoleValues(t *testing.T) {
	valid := map[string]bool{"": true, "read": true, "assistant": true, "owner": true, "admin": true}
	for _, r := range apispec.Routes() {
		if !valid[r.Role] {
			t.Errorf("%s %s: invalid role %q", r.Method, r.Path, r.Role)
		}
	}
}

// TestRestoredProse pins the operational descriptions that the route table is
// the single source of truth for. Without these the OpenAPI document and /agent
// lose semantics the API previously documented.
func TestRestoredProse(t *testing.T) {
	hasDescription := map[string]bool{}
	for _, r := range apispec.Routes() {
		if r.Description != "" {
			hasDescription[r.Method+" "+r.Path] = true
		}
	}
	for _, key := range []string{
		"PATCH /v1/inboxes/{id}",
		"POST /v1/send",
		"POST /v1/drafts",
		"PATCH /v1/drafts/{id}",
		"POST /v1/drafts/{id}/request-send",
		"GET /v1/messages",
		"PATCH /v1/messages/{id}",
		"GET /v1/search",
		"GET /v1/labels",
		"GET /v1/events",
		"GET /v1/events/wait",
		"GET /v1/events/stream",
		"PATCH /v1/admin/domains/{id}",
		"PUT /v1/admin/domains/{id}/receiving",
		"POST /v1/admin/hermes/enroll",
		"PUT /v1/admin/hermes/{id}",
	} {
		if !hasDescription[key] {
			t.Errorf("%s: missing restored description", key)
		}
	}
}

// TestRouteSuccessStatus keeps the primary success status within the small set
// the renderers know how to describe.
func TestRouteSuccessStatus(t *testing.T) {
	for _, r := range apispec.Routes() {
		switch r.Status() {
		case 200, 201, 204:
		default:
			t.Errorf("%s %s: Status() = %d, want 200, 201 or 204", r.Method, r.Path, r.Status())
		}
	}
}

// TestGroups pins the group headings and their documentation order.
func TestGroups(t *testing.T) {
	want := []string{
		"Discovery",
		"Inboxes",
		"Identities",
		"Messages",
		"Attachments",
		"Threads",
		"Search",
		"Labels",
		"Events",
		"Send",
		"Drafts",
		"Send requests",
		"Outbox",
		"Admin: domains",
		"Admin: keys",
		"Admin: external aliases",
		"Admin: Hermes",
		"Admin: OpenClaw",
		"Admin: MX",
		"Admin: clients",
	}
	got := apispec.Groups()
	if len(got) != len(want) {
		t.Fatalf("Groups() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Groups()[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestMissingWithRegisteredRoutes verifies the table self-consistently covers
// its own registrations: feeding Routes() back into Missing() reports no drift.
func TestMissingWithRegisteredRoutes(t *testing.T) {
	registered := make([]string, 0, len(apispec.Routes()))
	for _, r := range apispec.Routes() {
		if r.Auth == apispec.AuthSession {
			continue
		}
		registered = append(registered, r.Method+" "+r.Path)
	}
	undocumented, phantom := apispec.Missing(registered)
	if len(undocumented) != 0 {
		t.Errorf("undocumented = %v, want none", undocumented)
	}
	if len(phantom) != 0 {
		t.Errorf("phantom = %v, want none", phantom)
	}
}
