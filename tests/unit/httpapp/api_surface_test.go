package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gatehouse-mail/internal/apispec"
)

// TestOpenAPISurfaceIsValidAndComplete asserts the served document is valid
// OpenAPI 3.0.3 (every Operation Object carries a non-empty responses map) and
// covers every operation in the spec table.
func TestOpenAPISurfaceIsValidAndComplete(t *testing.T) {
	_, h, _, _, _ := httpFixture(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json = %d", rr.Code)
	}

	var doc struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatalf("openapi.json is not valid JSON: %v", err)
	}
	if doc.OpenAPI != "3.0.3" {
		t.Fatalf("openapi version = %q, want 3.0.3", doc.OpenAPI)
	}

	type opKey struct{ method, path string }
	want := map[opKey]bool{}
	for _, r := range apispec.Routes() {
		want[opKey{strings.ToUpper(r.Method), r.Path}] = true
	}

	for path, ops := range doc.Paths {
		for method, raw := range ops {
			op, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("%s %s: operation is not an object", method, path)
			}
			responses, ok := op["responses"].(map[string]any)
			if !ok || len(responses) == 0 {
				t.Fatalf("%s %s: OpenAPI 3.0.3 requires a non-empty responses object", method, path)
			}
			hasSuccess := false
			for code := range responses {
				if strings.HasPrefix(code, "2") {
					hasSuccess = true
					break
				}
			}
			if !hasSuccess {
				t.Fatalf("%s %s: responses has no 2xx success entry", method, path)
			}
			// OpenAPI 3.0.3 requires each {placeholder} to be declared as a
			// required path parameter.
			declared := map[string]bool{}
			if params, ok := op["parameters"].([]any); ok {
				for _, p := range params {
					pm, _ := p.(map[string]any)
					if pm["in"] != "path" {
						continue
					}
					if name, _ := pm["name"].(string); name != "" {
						declared[name] = true
					}
				}
			}
			for i := 0; i < len(path); i++ {
				if path[i] != '{' {
					continue
				}
				rel := strings.IndexByte(path[i:], '}')
				if rel < 0 {
					break
				}
				name := path[i+1 : i+rel]
				if !declared[name] {
					t.Fatalf("%s %s: path parameter %q is not declared", method, path, name)
				}
				i += rel
			}
			delete(want, opKey{strings.ToUpper(method), path})
		}
	}
	if len(want) > 0 {
		t.Fatalf("spec operations missing from /openapi.json: %v", want)
	}
}

// TestDiscoverySurfacesServeRealContent asserts every advertised discovery
// artifact exists and is non-trivial.
func TestDiscoverySurfacesServeRealContent(t *testing.T) {
	_, h, _, _, _ := httpFixture(t)
	get := func(path string) string {
		t.Helper()
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		return rr.Body.String()
	}

	guide := get("/agent")
	quickstart := strings.Index(guide, "## Quick start")
	discoverySection := strings.Index(guide, "## Discovery")
	if quickstart < 0 || discoverySection < 0 || quickstart > discoverySection {
		t.Error("/agent quickstart should precede the generated route reference")
	}
	if !strings.Contains(guide, "GET /openapi.json") {
		t.Error("/agent does not link to /openapi.json")
	}
	for _, group := range apispec.Groups() {
		if !strings.Contains(guide, "## "+group) {
			t.Errorf("/agent missing group %q", group)
		}
	}
	if len(guide) < 2000 {
		t.Errorf("/agent unexpectedly short (%d bytes)", len(guide))
	}

	discovery := get("/.well-known/gatehouse")
	for _, want := range []string{"/agent", "/openapi.json", "/examples/python", "/examples/bash", "/examples/curl", "/v1/bootstrap"} {
		if !strings.Contains(discovery, want) {
			t.Errorf("/.well-known/gatehouse does not advertise %q", want)
		}
	}

	if body := get("/examples/python"); len(body) < 2000 || !strings.Contains(body, "urllib") {
		t.Errorf("/examples/python does not look like the Python client (%d bytes)", len(body))
	}
	if body := get("/examples/bash"); len(body) < 2000 || !strings.Contains(body, "usage") {
		t.Errorf("/examples/bash does not look like the Bash client (%d bytes)", len(body))
	}
	if body := get("/examples/curl"); len(body) < 500 || !strings.Contains(body, "/v1/") {
		t.Errorf("/examples/curl does not look like the curl cookbook (%d bytes)", len(body))
	}
}

// TestRootServesDiscoveryToAgents asserts GET / is useful to an agent that has
// no browser session: an API client (does not ask for HTML) gets the discovery
// document, while a browser is sent through the login flow.
func TestRootServesDiscoveryToAgents(t *testing.T) {
	_, h, _, _, _ := httpFixture(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("agent GET / = %d, want 200", rr.Code)
	}
	for _, want := range []string{"/agent", "/openapi.json", "/examples/python", "/examples/bash"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("agent GET / body does not advertise %q", want)
		}
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("browser GET / = %d, want 303 redirect", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/login" {
		t.Fatalf("browser GET / redirected to %q, want /login", loc)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /login = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "/openapi.json") {
		t.Errorf("/login page does not point agents at the discovery surfaces")
	}
}

// TestOpenAPIDiscoversRequestOrigin asserts the served document advertises the
// host the caller actually reached instead of a configured BASE_URL.
func TestOpenAPIDiscoversRequestOrigin(t *testing.T) {
	_, h, _, _, _ := httpFixture(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	req.Host = "mail.example.org"
	h.ServeHTTP(rr, req)
	var doc struct {
		Servers []struct {
			URL string `json:"url"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Servers) != 1 || doc.Servers[0].URL != "http://mail.example.org" {
		t.Fatalf("servers = %#v, want the request origin", doc.Servers)
	}
}

// TestDiscoveryAdvertisesLimits asserts the discovery surfaces publish the
// pagination, size and rate bounds an agent needs to page safely.
func TestDiscoveryAdvertisesLimits(t *testing.T) {
	_, h, _, _, _ := httpFixture(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/.well-known/gatehouse", nil))
	var doc struct {
		Limits map[string]any `json:"limits"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Limits == nil {
		t.Fatalf("discovery has no limits block: %s", rr.Body.String())
	}
	for _, key := range []string{"page_size_default", "page_size_max_list", "page_size_max_events", "message_bytes_max", "send_per_minute"} {
		if _, ok := doc.Limits[key]; !ok {
			t.Errorf("limits missing %q: %#v", key, doc.Limits)
		}
	}
}

// TestDiscoveryAdvertisesAuth asserts the discovery document reveals the auth
// scheme, header and key prefix so an agent can authenticate from the
// well-known document without reading the full /agent guide first.
func TestDiscoveryAdvertisesAuth(t *testing.T) {
	_, h, _, _, _ := httpFixture(t)
	for _, path := range []string{"/.well-known/gatehouse", "/"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Accept", "application/json")
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
		var doc struct {
			Auth map[string]string `json:"auth"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
			t.Fatalf("%s is not valid JSON: %v", path, err)
		}
		if doc.Auth["scheme"] != "bearer" || doc.Auth["header"] != "Authorization" || doc.Auth["key_prefix"] != "ghm_" {
			t.Errorf("%s auth block = %#v, want bearer/Authorization/ghm_", path, doc.Auth)
		}
	}
}

func TestBootstrapAdvertisesEffectivePermissions(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		admin bool
		role  string
		want  map[string]bool
	}{
		{name: "read", role: "read", want: map[string]bool{"can_read": true}},
		{name: "assistant", role: "assistant", want: map[string]bool{"can_read": true, "can_draft": true}},
		{name: "owner", role: "owner", want: map[string]bool{"can_read": true, "can_draft": true, "can_send": true, "can_approve": true}},
		{name: "admin", admin: true, want: map[string]bool{"can_read": true, "can_draft": true, "can_send": true, "can_approve": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			roles := map[string]string(nil)
			if !tc.admin {
				roles = map[string]string{box.ID: tc.role}
			}
			_, token, err := svc.Store.CreateAPIKey(ctx, u.AccountID, tc.name, tc.admin, roles)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/v1/bootstrap", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("bootstrap = %d body=%s", rr.Code, rr.Body.String())
			}
			var body struct {
				Permissions map[string]map[string]any `json:"permissions"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			got, ok := body.Permissions[box.ID]
			if !ok {
				t.Fatalf("permissions missing inbox %q: %#v", box.ID, body.Permissions)
			}
			for field, want := range tc.want {
				if got[field] != want {
					t.Errorf("%s = %#v, want %t", field, got[field], want)
				}
			}
		})
	}
}

func TestBearerAuthChallengeAndErrorCode(t *testing.T) {
	_, h, _, _, _ := httpFixture(t)
	for _, tc := range []struct {
		name string
		auth string
	}{
		{name: "missing", auth: ""},
		{name: "invalid", auth: "Bearer ghm_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/bootstrap", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rr.Code)
			}
			if got := rr.Header().Get("WWW-Authenticate"); got != `Bearer realm="gatehouse-api"` {
				t.Fatalf("WWW-Authenticate = %q", got)
			}
			var body map[string]string
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["code"] != "unauthorized" {
				t.Fatalf("error code = %q, want unauthorized", body["code"])
			}
		})
	}
}

// TestInvalidLimitRejected asserts a non-integer limit is a 400 rather than a
// silently ignored default.
func TestInvalidLimitRejected(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	_, token, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "test", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"limit=abc", "limit=0", "limit=-5"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/messages?"+q, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d body=%s, want 400", q, rr.Code, rr.Body.String())
		}
	}
}
