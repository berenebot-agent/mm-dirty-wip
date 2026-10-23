package apispec_test

import (
	"testing"

	"github.com/dellarb/mailmoose/internal/apispec"
)

func TestCommonSendContract(t *testing.T) {
	t.Parallel()
	spec := apispec.RenderOpenAPI("https://example.test", apispec.Routes())
	paths := spec["paths"].(map[string]any)
	for _, path := range []string{"/v1/send", "/v1/messages/{id}/reply", "/v1/messages/{id}/forward", "/v1/drafts/{id}/send", "/v1/drafts/{id}/approve", "/v1/drafts"} {
		op := paths[path].(map[string]any)["post"].(map[string]any)
		responses := op["responses"].(map[string]any)
		response, ok := responses["200"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no 200 response: %+v", path, responses)
		}
		schema := response["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
		if schema["$ref"] != "#/components/schemas/SendQueued" {
			t.Errorf("%s response: %+v", path, schema)
		}
	}
	schemas := apispec.Schemas()
	for _, name := range []string{"SendBody", "ReplyBody", "ForwardBody"} {
		properties := schemas[name].(map[string]any)["properties"].(map[string]any)
		for _, field := range []string{"to", "cc", "bcc", "sender", "subject", "text", "html", "attachments"} {
			if _, ok := properties[field]; !ok {
				t.Errorf("%s missing %s", name, field)
			}
		}
	}
}
