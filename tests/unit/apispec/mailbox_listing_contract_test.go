package apispec_test

import (
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/apispec"
)

func TestMailboxListingQueryParity(t *testing.T) {
	t.Parallel()
	spec := apispec.RenderOpenAPI("https://example.test", apispec.Routes())
	paths := spec["paths"].(map[string]any)
	for _, resource := range []string{"messages", "threads", "search"} {
		parameters := func(path string) map[string]bool {
			out := map[string]bool{}
			operation := paths[path].(map[string]any)["get"].(map[string]any)
			for _, param := range operation["parameters"].([]map[string]any) {
				if param["in"] == "query" {
					out[param["name"].(string)] = true
				}
			}
			return out
		}
		account := parameters("/v1/" + resource)
		inbox := parameters("/v1/inboxes/{id}/" + resource)
		for name := range inbox {
			if !account[name] {
				t.Errorf("%s: inbox-only query %s", resource, name)
			}
		}
		for name := range account {
			if name != "inbox" && name != "address" && name != "thread" && !inbox[name] {
				t.Errorf("%s: missing inbox query %s", resource, name)
			}
		}
		if !inbox["before"] || inbox["cursor"] {
			t.Errorf("%s: incorrect pagination contract: %v", resource, inbox)
		}
	}
	guide := apispec.RenderAgentGuide(apispec.Routes())
	for _, text := range []string{"before=<next_cursor>", "label-catalog", "membership: exclusive", "handoff.publication", "source coverage"} {
		if !strings.Contains(guide, text) {
			t.Errorf("agent guide missing %q", text)
		}
	}
	if strings.Contains(guide, "body is fetched separately by the content route") {
		t.Fatal("agent guide still describes metadata-only remote details")
	}
}
