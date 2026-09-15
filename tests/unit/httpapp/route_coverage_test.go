package httpapp_test

import (
	"testing"

	"gatehouse-mail/internal/apispec"
	"gatehouse-mail/internal/httpapp"
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
