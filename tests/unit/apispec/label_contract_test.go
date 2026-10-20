package apispec_test

import (
	"testing"

	"github.com/dellarb/mailmoose/internal/apispec"
)

// TestLabelRoutesDocumentActualResponses proves the labels and message-mutation
// operations document the queued-action status an async-default route actually
// returns, and that the label catalogue advertises capabilities.
func TestLabelRoutesDocumentActualResponses(t *testing.T) {
	t.Parallel()
	spec := apispec.RenderOpenAPI("https://example.test", apispec.Routes())
	paths := spec["paths"].(map[string]any)

	labelAction := paths["/v1/inboxes/{id}/messages/{messageId}/labels"].(map[string]any)["post"].(map[string]any)
	responses := labelAction["responses"].(map[string]any)
	for _, status := range []string{"200", "202"} {
		if _, ok := responses[status]; !ok {
			t.Fatalf("label action must document %s, got %v", status, keys(responses))
		}
	}
	if responses["200"].(map[string]any)["content"] == nil || responses["202"].(map[string]any)["content"] == nil {
		t.Fatal("both label action success bodies must carry a schema")
	}

	patch := paths["/v1/messages/{id}"].(map[string]any)["patch"].(map[string]any)
	if _, ok := patch["responses"].(map[string]any)["202"]; !ok {
		t.Fatal("PATCH /v1/messages/{id} must document its queued-action response")
	}

	catalog := paths["/v1/inboxes/{id}/label-catalog"].(map[string]any)["get"].(map[string]any)
	ok200 := catalog["responses"].(map[string]any)["200"].(map[string]any)
	schema := ok200["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	if schema["$ref"] != "#/components/schemas/MailboxLabelCatalog" {
		t.Fatalf("catalogue must reference MailboxLabelCatalog, got %v", schema)
	}

	// The message and catalogue schemas must require the new fields.
	schemas := spec["components"].(map[string]any)["schemas"].(map[string]any)
	message := schemas["Message"].(map[string]any)
	if _, ok := message["properties"].(map[string]any)["label_ids"]; !ok {
		t.Fatal("Message.label_ids missing")
	}
	if !contains(message["required"].([]string), "label_ids") {
		t.Fatal("Message.label_ids must be required")
	}
	caps := schemas["MailboxLabelCapabilities"].(map[string]any)
	for _, field := range []string{"can_create", "create_kind", "create_membership", "can_rename", "can_delete"} {
		if _, ok := caps["properties"].(map[string]any)[field]; !ok {
			t.Fatalf("MailboxLabelCapabilities.%s missing", field)
		}
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
