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

// TestInvalidLimitRejected asserts a non-integer limit is a 400 rather than a
// silently ignored default.
func TestInvalidLimitRejected(t *testing.T) {
	svc, h, u, _, _ := httpFixture(t)
	_, token, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "test", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/messages?limit=abc", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("limit=abc = %d body=%s, want 400", rr.Code, rr.Body.String())
	}
}
