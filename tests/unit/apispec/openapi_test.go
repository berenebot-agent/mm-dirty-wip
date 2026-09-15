package apispec_test

import (
	"strconv"
	"strings"
	"testing"

	"gatehouse-mail/internal/apispec"
)

// openAPIFixture covers a 200 GET, a 201 POST and a 204 DELETE so the success
// status handling can be exercised without depending on the real table.
func openAPIFixture() []apispec.Route {
	return []apispec.Route{
		{Method: "GET", Path: "/v1/inboxes", Summary: "List inboxes", Group: "Inboxes"},
		{Method: "POST", Path: "/v1/inboxes", Summary: "Create an inbox", Role: "admin", Group: "Inboxes", Success: 201},
		{Method: "DELETE", Path: "/v1/inboxes/{id}", Summary: "Delete an inbox", Role: "admin", Group: "Inboxes", Success: 204},
	}
}

func TestRenderOpenAPITopLevel(t *testing.T) {
	doc := apispec.RenderOpenAPI("https://mail.example.test", openAPIFixture())
	if got := doc["openapi"]; got != "3.0.3" {
		t.Fatalf("openapi = %v, want 3.0.3", got)
	}
	servers, ok := doc["servers"].([]map[string]string)
	if !ok || len(servers) != 1 || servers[0]["url"] != "https://mail.example.test" {
		t.Fatalf("servers = %#v", doc["servers"])
	}
	security, ok := doc["security"].([]map[string]any)
	if !ok || len(security) != 1 {
		t.Fatalf("global security = %#v", doc["security"])
	}
	if _, ok := security[0]["bearerAuth"]; !ok {
		t.Fatalf("global security missing bearerAuth: %#v", security[0])
	}
	components, ok := doc["components"].(map[string]any)
	if !ok {
		t.Fatalf("components = %#v", doc["components"])
	}
	schemes, ok := components["securitySchemes"].(map[string]any)
	if !ok {
		t.Fatalf("securitySchemes = %#v", components["securitySchemes"])
	}
	bearer, ok := schemes["bearerAuth"].(map[string]any)
	if !ok || bearer["type"] != "http" || bearer["scheme"] != "bearer" {
		t.Fatalf("bearerAuth = %#v", schemes["bearerAuth"])
	}
	info, ok := doc["info"].(map[string]any)
	if !ok || info["title"] != "Gatehouse Mail" || info["version"] != "v1" {
		t.Fatalf("info = %#v", doc["info"])
	}
	tags, ok := doc["tags"].([]map[string]any)
	if !ok || len(tags) != 1 || tags[0]["name"] != "Inboxes" {
		t.Fatalf("tags = %#v", doc["tags"])
	}
}

func TestRenderOpenAPIResponses(t *testing.T) {
	routes := openAPIFixture()
	doc := apispec.RenderOpenAPI("https://mail.example.test", routes)
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatalf("paths = %#v", doc["paths"])
	}
	for _, r := range routes {
		item, ok := paths[r.Path].(map[string]any)
		if !ok {
			t.Fatalf("path %q missing", r.Path)
		}
		op, ok := item[strings.ToLower(r.Method)].(map[string]any)
		if !ok {
			t.Fatalf("%s %s operation missing", r.Method, r.Path)
		}
		if op["summary"] != r.Summary {
			t.Fatalf("%s %s summary = %#v, want %q", r.Method, r.Path, op["summary"], r.Summary)
		}
		responses, ok := op["responses"].(map[string]any)
		if !ok || len(responses) == 0 {
			t.Fatalf("%s %s responses = %#v, must be non-empty", r.Method, r.Path, op["responses"])
		}
		code := strconv.Itoa(r.Status())
		if _, ok := responses[code]; !ok {
			t.Fatalf("%s %s missing success response %s (%#v)", r.Method, r.Path, code, responses)
		}
		if _, ok := responses["401"]; !ok {
			t.Fatalf("%s %s missing 401 response", r.Method, r.Path)
		}
		if _, ok := responses["default"]; !ok {
			t.Fatalf("%s %s missing default response", r.Method, r.Path)
		}
	}
}

// TestRenderOpenAPIPathParameters asserts every {placeholder} in a path is
// declared as a required path parameter, which OpenAPI 3.0.3 requires.
func TestRenderOpenAPIPathParameters(t *testing.T) {
	routes := []apispec.Route{
		{Method: "GET", Path: "/v1/messages/{id}", Summary: "Get a message", Group: "Messages"},
		{Method: "GET", Path: "/v1/admin/inboxes/{id}/external-aliases/{aliasID}", Summary: "List aliases", Group: "Admin: external aliases"},
	}
	doc := apispec.RenderOpenAPI("https://mail.example.test", routes)
	paths := doc["paths"].(map[string]any)
	for _, r := range routes {
		item := paths[r.Path].(map[string]any)
		op := item[strings.ToLower(r.Method)].(map[string]any)
		raw, ok := op["parameters"].([]map[string]any)
		if !ok {
			t.Fatalf("%s %s: parameters missing (%#v)", r.Method, r.Path, op["parameters"])
		}
		want := map[string]bool{}
		for i := 0; i < len(r.Path); i++ {
			if r.Path[i] == '{' {
				if j := strings.IndexByte(r.Path[i:], '}'); j >= 0 {
					want[r.Path[i+1:i+j]] = true
				}
			}
		}
		if len(raw) != len(want) {
			t.Fatalf("%s %s: got %d parameters, want %d (%#v)", r.Method, r.Path, len(raw), len(want), raw)
		}
		for _, p := range raw {
			name, _ := p["name"].(string)
			if !want[name] {
				t.Errorf("%s %s: unexpected parameter %q", r.Method, r.Path, name)
			}
			if p["in"] != "path" || p["required"] != true {
				t.Errorf("%s %s: parameter %q must be in:path required:true (%#v)", r.Method, r.Path, name, p)
			}
		}
	}
}

func TestRenderOpenAPINo200On204Route(t *testing.T) {
	doc := apispec.RenderOpenAPI("https://mail.example.test", openAPIFixture())
	paths := doc["paths"].(map[string]any)
	item := paths["/v1/inboxes/{id}"].(map[string]any)
	op := item["delete"].(map[string]any)
	responses := op["responses"].(map[string]any)
	if _, ok := responses["204"]; !ok {
		t.Fatalf("204 route missing 204 response: %#v", responses)
	}
	if _, ok := responses["200"]; ok {
		t.Fatalf("204 route must not carry a 200 response: %#v", responses)
	}
}
