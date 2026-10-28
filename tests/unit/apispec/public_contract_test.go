package apispec_test

import (
	"testing"

	"github.com/dellarb/mammothmail/internal/apispec"
)

func TestPublicContractDiscoveryAndActions(t *testing.T) {
	t.Parallel()
	schemas := apispec.Schemas()
	for schema, fields := range map[string][]string{
		"Inbox":             {"kind", "remote", "namespace", "remote_configured", "storage_used_bytes"},
		"Capabilities":      {"archive", "purge"},
		"Attachment":        {"download_url"},
		"ThreadDetail":      {"messages_url", "completeness"},
		"MessageAction":     {"input", "state", "created_at", "inbox_name"},
		"MessageActionList": {"items", "syncs", "connections", "csrf"},
		"Error":             {"error", "code", "action"},
	} {
		props := schemas[schema].(map[string]any)["properties"].(map[string]any)
		for _, field := range fields {
			if props[field] == nil {
				t.Errorf("%s missing %s", schema, field)
			}
		}
	}
	doc := apispec.RenderOpenAPI("https://example.test", apispec.Routes())
	paths := doc["paths"].(map[string]any)
	for _, path := range []string{"/v1/messages/{id}", "/v1/messages/{id}/purge"} {
		op := paths[path].(map[string]any)["delete"].(map[string]any)
		responses := op["responses"].(map[string]any)
		if responses["202"] == nil || responses["204"] == nil {
			t.Errorf("%s missing queued/settled responses: %+v", path, responses)
		}
	}
	for _, path := range []string{"/v1/messages/{id}/seen", "/v1/drafts/{id}/send", "/v1/drafts/{id}/request-send", "/v1/drafts/{id}/approve", "/v1/drafts/{id}/reject"} {
		op := paths[path].(map[string]any)["post"].(map[string]any)
		if op["requestBody"].(map[string]any)["required"] != false {
			t.Errorf("%s body must be optional", path)
		}
	}
	op := paths["/v1/messages"].(map[string]any)["get"].(map[string]any)
	for _, param := range op["parameters"].([]map[string]any) {
		if param["name"] != "label_id" {
			continue
		}
		schema := param["schema"].(map[string]any)
		if param["style"] != "form" || param["explode"] != true || schema["style"] != nil || schema["explode"] != nil {
			t.Fatalf("invalid repeated-query encoding: %+v", param)
		}
	}
	for _, r := range apispec.Routes() {
		if r.Method == "GET" && r.Path == "/v1/inboxes/{id}/labels" && r.Response != "LabelEnvelope" {
			t.Errorf("legacy inbox labels schema = %s", r.Response)
		}
	}
}
