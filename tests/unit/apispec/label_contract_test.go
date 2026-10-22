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
	// The location is a separate, nullable field: a folder id is never a label.
	folder, ok := message["properties"].(map[string]any)["folder_id"].(map[string]any)
	if !ok || folder["type"] != "string" || folder["nullable"] != true || !contains(message["required"].([]string), "folder_id") {
		t.Fatalf("Message.folder_id must be required and nullable: %#v", message)
	}
	// Every catalogue entry is an additive label: no exclusive membership and
	// no system classification survive the unified-label contract.
	label := schemas["MailboxLabel"].(map[string]any)["properties"].(map[string]any)
	for _, field := range []string{"membership", "can_add", "can_remove", "kind"} {
		if _, ok := label[field]; !ok {
			t.Fatalf("MailboxLabel.%s missing", field)
		}
	}
	for _, field := range []string{"role", "folder_id"} {
		if _, ok := label[field]; ok {
			t.Fatalf("MailboxLabel.%s must be gone: a folder is not a label", field)
		}
	}
	if kinds := label["kind"].(map[string]any)["enum"].([]string); len(kinds) != 1 || kinds[0] != "tag" {
		t.Fatalf("MailboxLabel.kind must be tag only, got %v", kinds)
	}
	if members := label["membership"].(map[string]any)["enum"].([]string); len(members) != 1 || members[0] != "multiple" {
		t.Fatalf("MailboxLabel.membership must be multiple only, got %v", members)
	}
	caps := schemas["MailboxLabelCapabilities"].(map[string]any)
	for _, field := range []string{"can_create", "create_kind", "create_membership", "can_rename", "can_delete"} {
		if _, ok := caps["properties"].(map[string]any)[field]; !ok {
			t.Fatalf("MailboxLabelCapabilities.%s missing", field)
		}
	}
	// The inbox capability surface states what the mailbox supports.
	mailbox := schemas["Capabilities"].(map[string]any)["properties"].(map[string]any)
	for _, field := range []string{"folders", "move", "create_folders", "rename_folders", "delete_folders", "labels", "create_labels", "rename_labels", "delete_labels"} {
		if _, ok := mailbox[field]; !ok {
			t.Fatalf("Capabilities.%s missing", field)
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
