package apispec_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/apispec"
)

// collectRefs walks an arbitrary OpenAPI value and returns every
// #/components/schemas/<name> reference it contains.
func collectRefs(v any, out map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if k == "$ref" {
				if s, ok := val.(string); ok {
					out[strings.TrimPrefix(s, "#/components/schemas/")] = true
				}
				continue
			}
			collectRefs(val, out)
		}
	case []any:
		for _, val := range t {
			collectRefs(val, out)
		}
	case []map[string]any:
		for _, val := range t {
			collectRefs(val, out)
		}
	}
}

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
	if !ok || info["title"] != "MailMoose" || info["version"] != "v1" {
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

// TestRenderOpenAPIRoleExtension asserts the machine-readable role map: a
// role-scoped operation carries x-required-role, and an open operation does not.
func TestRenderOpenAPIRoleExtension(t *testing.T) {
	doc := apispec.RenderOpenAPI("https://mail.example.test", []apispec.Route{
		{Method: "POST", Path: "/v1/inboxes", Summary: "Create", Role: "admin", Group: "Inboxes", Success: 201},
		{Method: "GET", Path: "/v1/bootstrap", Summary: "Discover", Group: "Discovery"},
	})
	paths := doc["paths"].(map[string]any)
	op := paths["/v1/inboxes"].(map[string]any)["post"].(map[string]any)
	if op["x-required-role"] != "admin" {
		t.Fatalf("x-required-role = %#v, want admin", op["x-required-role"])
	}
	boot := paths["/v1/bootstrap"].(map[string]any)["get"].(map[string]any)
	if _, ok := boot["x-required-role"]; ok {
		t.Fatalf("open operation must not carry x-required-role: %#v", boot)
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

// TestContractKeysMatchRoutes guards the wire-contract table against a typo in
// a "METHOD /path" key that would silently drop a request or response schema.
func TestContractKeysMatchRoutes(t *testing.T) {
	known := map[string]bool{}
	for _, r := range apispec.Routes() {
		known[r.Method+" "+r.Path] = true
	}
	for _, key := range apispec.ContractKeys() {
		if !known[key] {
			t.Errorf("route contract key %q does not name a route", key)
		}
	}
}

// TestOpenAPISessionAuthentication asserts the installation-management routes
// advertise cookie-session authentication (with CSRF on writes) instead of the
// global bearer requirement, and that the session security scheme is declared.
func TestOpenAPISessionAuthentication(t *testing.T) {
	doc := apispec.RenderOpenAPI("https://mail.example.test", apispec.Routes())
	components := doc["components"].(map[string]any)
	schemes := components["securitySchemes"].(map[string]any)
	if _, ok := schemes["sessionAuth"]; !ok {
		t.Fatalf("sessionAuth security scheme not declared: %#v", schemes)
	}
	paths := doc["paths"].(map[string]any)
	item := paths["/v1/admin/mx"].(map[string]any)
	for _, method := range []string{"get", "put", "delete"} {
		op := item[method].(map[string]any)
		if op["x-authentication"] != apispec.AuthSession {
			t.Errorf("%s /v1/admin/mx x-authentication = %#v, want session", method, op["x-authentication"])
		}
		sec, ok := op["security"].([]map[string]any)
		if !ok || len(sec) != 1 {
			t.Fatalf("%s /v1/admin/mx security = %#v", method, op["security"])
		}
		if _, ok := sec[0]["sessionAuth"]; !ok {
			t.Errorf("%s /v1/admin/mx must require sessionAuth: %#v", method, sec)
		}
		responses := op["responses"].(map[string]any)
		if _, ok := responses["403"]; !ok {
			t.Errorf("%s /v1/admin/mx must document 403: %#v", method, responses)
		}
	}
	// A bearer route keeps the global bearer requirement and no session marker.
	inc := paths["/v1/inboxes"].(map[string]any)["get"].(map[string]any)
	if _, ok := inc["x-authentication"]; ok {
		t.Errorf("bearer route must not carry x-authentication: %#v", inc["x-authentication"])
	}
}

// TestSessionRoutesAreInstallationOnly pins that only installation-management
// operations are session-authenticated; the bearer surface stays unchanged.
func TestSessionRoutesAreInstallationOnly(t *testing.T) {
	for _, key := range apispec.SessionRoutes() {
		if key != "GET /v1/admin/mx" && key != "PUT /v1/admin/mx" && key != "DELETE /v1/admin/mx" {
			t.Errorf("unexpected session-authenticated route %q", key)
		}
	}
}

// TestOpenAPISchemasAndReferencesResolve renders the real document and checks
// every $ref resolves to a declared component schema, and that every route's
// named request/response schema exists.
func TestOpenAPISchemasAndReferencesResolve(t *testing.T) {
	doc := apispec.RenderOpenAPI("https://mail.example.test", apispec.Routes())
	components, ok := doc["components"].(map[string]any)
	if !ok {
		t.Fatalf("components = %#v", doc["components"])
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok || len(schemas) == 0 {
		t.Fatalf("components.schemas = %#v", components["schemas"])
	}
	refs := map[string]bool{}
	collectRefs(doc, refs)
	for name := range refs {
		if _, ok := schemas[name]; !ok {
			t.Errorf("reference to undeclared schema %q", name)
		}
	}
	for _, r := range apispec.Routes() {
		if r.Request != "" {
			if _, ok := schemas[r.Request]; !ok {
				t.Errorf("%s %s: request schema %q not declared", r.Method, r.Path, r.Request)
			}
		}
		if r.Response != "" {
			if _, ok := schemas[r.Response]; !ok {
				t.Errorf("%s %s: response schema %q not declared", r.Method, r.Path, r.Response)
			}
		}
	}
}

// TestOpenAPISendBodyMatchesRuntime keeps the spec honest: it requires the
// fields the server enforces (to, subject) while accepting a bare string or a
// list for recipients.
func TestOpenAPISendBodyMatchesRuntime(t *testing.T) {
	doc := apispec.RenderOpenAPI("https://mail.example.test", apispec.Routes())
	components := doc["components"].(map[string]any)
	schemas := components["schemas"].(map[string]any)
	send, ok := schemas["SendBody"].(map[string]any)
	if !ok {
		t.Fatal("SendBody schema missing")
	}
	required, ok := send["required"].([]string)
	if !ok {
		t.Fatalf("SendBody should require subject and to: %#v", send["required"])
	}
	want := map[string]bool{"to": true, "subject": true}
	if len(required) != len(want) {
		t.Fatalf("SendBody.required = %#v, want to and subject", required)
	}
	for _, name := range required {
		if !want[name] {
			t.Fatalf("SendBody.required has unexpected %q", name)
		}
	}
	props := send["properties"].(map[string]any)
	to, ok := props["to"].(map[string]any)
	if !ok {
		t.Fatal("SendBody.to missing")
	}
	if _, ok := to["oneOf"]; !ok {
		t.Fatalf("SendBody.to must accept a bare string or a list: %#v", to)
	}
	if desc, _ := props["text"].(map[string]any)["description"].(string); !strings.Contains(desc, "html") {
		t.Fatalf("SendBody.text does not explain the body requirement: %q", desc)
	}
	if desc, _ := props["subject"].(map[string]any)["description"].(string); !strings.Contains(desc, "non-empty") {
		t.Fatalf("SendBody.subject does not explain the non-empty requirement: %q", desc)
	}
}

func TestOpenAPIBootstrapIncludesEffectivePermissions(t *testing.T) {
	doc := apispec.RenderOpenAPI("https://mail.example.test", apispec.Routes())
	components := doc["components"].(map[string]any)
	schemas := components["schemas"].(map[string]any)
	bootstrap := schemas["Bootstrap"].(map[string]any)
	props := bootstrap["properties"].(map[string]any)
	permissions := props["permissions"].(map[string]any)
	if permissions["type"] != "object" {
		t.Fatalf("Bootstrap.permissions type = %#v", permissions["type"])
	}
	additional, ok := permissions["additionalProperties"].(map[string]any)
	if !ok || additional["$ref"] != "#/components/schemas/MailboxPermissions" {
		t.Fatalf("Bootstrap.permissions schema = %#v", permissions)
	}
}

// TestOpenAPIRequestBodyAndQueryParameters asserts a JSON write carries a
// requestBody, the multipart upload uses its content type, and query
// parameters are emitted as in:query.
func TestOpenAPIRequestBodyAndQueryParameters(t *testing.T) {
	doc := apispec.RenderOpenAPI("https://mail.example.test", apispec.Routes())
	paths := doc["paths"].(map[string]any)

	send := paths["/v1/send"].(map[string]any)["post"].(map[string]any)
	body, ok := send["requestBody"].(map[string]any)
	if !ok {
		t.Fatalf("POST /v1/send has no requestBody")
	}
	content := body["content"].(map[string]any)
	schema := content["application/json"].(map[string]any)["schema"].(map[string]any)
	if schema["$ref"] != "#/components/schemas/SendBody" {
		t.Fatalf("POST /v1/send request schema = %#v", schema)
	}
	hasWait := false
	for _, raw := range send["parameters"].([]map[string]any) {
		if raw["name"] == "wait" && raw["in"] == "query" {
			hasWait = true
		}
	}
	if !hasWait {
		t.Fatalf("POST /v1/send missing wait query parameter: %#v", send["parameters"])
	}

	upload := paths["/v1/drafts/{id}/attachments"].(map[string]any)["post"].(map[string]any)
	upBody := upload["requestBody"].(map[string]any)
	if _, ok := upBody["content"].(map[string]any)["multipart/form-data"]; !ok {
		t.Fatalf("draft upload must use multipart/form-data: %#v", upBody["content"])
	}

	list := paths["/v1/messages"].(map[string]any)["get"].(map[string]any)
	params, ok := list["parameters"].([]map[string]any)
	if !ok || len(params) == 0 {
		t.Fatalf("GET /v1/messages missing parameters: %#v", list["parameters"])
	}
	names := map[string]bool{}
	for _, raw := range params {
		names[raw["name"].(string)] = true
	}
	for _, want := range []string{"inbox", "limit", "label", "before"} {
		if !names[want] {
			t.Errorf("GET /v1/messages missing query parameter %q", want)
		}
	}
}
