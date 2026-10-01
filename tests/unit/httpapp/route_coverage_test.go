package httpapp_test

import (
	"testing"

	"github.com/dellarb/mailmoose/internal/apispec"
	"github.com/dellarb/mailmoose/internal/httpapp"
)

// TestAPISpecCoversEveryRegisteredRoute is the drift guard for agent
// self-discovery: the documentation table in internal/apispec and the live
// registrations in internal/httpapp must describe exactly the same /v1 surface.
func TestAPISpecCoversEveryRegisteredRoute(t *testing.T) {
	svc, _, _, _, _ := httpFixture(t)
	srv := httpapp.New(svc, nil)
	undocumented, phantom := apispec.Missing(srv.RegisteredAPIRoutes())
	if len(undocumented) > 0 {
		t.Fatalf("registered routes missing from the spec table: %v", undocumented)
	}
	if len(phantom) > 0 {
		t.Fatalf("spec table entries with no registered route: %v", phantom)
	}
}

// TestAPISpecCoversEverySessionRoute is the drift guard for the
// session-authenticated installation surface: the spec's Auth "session" entries
// and the live session registrations must describe exactly the same routes.
func TestAPISpecCoversEverySessionRoute(t *testing.T) {
	svc, _, _, _, _ := httpFixture(t)
	srv := httpapp.New(svc, nil)
	undocumented, phantom := apispec.SessionMissing(srv.RegisteredSessionRoutes())
	if len(undocumented) > 0 {
		t.Fatalf("session registrations missing from the spec table: %v", undocumented)
	}
	if len(phantom) > 0 {
		t.Fatalf("session spec entries with no registered route: %v", phantom)
	}
	// The installation MX routes must be the session-authenticated ones.
	want := map[string]bool{
		"GET /v1/admin/mx":    true,
		"PUT /v1/admin/mx":    true,
		"DELETE /v1/admin/mx": true,
	}
	got := map[string]bool{}
	for _, key := range apispec.SessionRoutes() {
		got[key] = true
	}
	for key := range want {
		if !got[key] {
			t.Errorf("session route %q missing from the spec", key)
		}
	}
	for key := range got {
		if !want[key] {
			t.Errorf("unexpected session route %q in the spec", key)
		}
	}
}
