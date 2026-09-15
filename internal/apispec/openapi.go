package apispec

import (
	"strconv"
	"strings"
)

// RenderOpenAPI renders the authenticated /v1 surface as an OpenAPI 3.0.3
// document. Every operation carries a responses object, which OpenAPI 3.0.3
// requires.
func RenderOpenAPI(baseURL string, routes []Route) map[string]any {
	paths := map[string]any{}
	tags := make([]map[string]any, 0, len(routes))
	seen := map[string]bool{}
	for _, r := range routes {
		if r.Group != "" && !seen[r.Group] {
			seen[r.Group] = true
			tags = append(tags, map[string]any{"name": r.Group})
		}
		method := strings.ToLower(strings.TrimSpace(r.Method))
		item, ok := paths[r.Path].(map[string]any)
		if !ok {
			item = map[string]any{}
			paths[r.Path] = item
		}
		item[method] = openAPIOperation(r)
	}
	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "Gatehouse Mail",
			"version":     "v1",
			"description": "Agent-first REST API for Gatehouse Mail. See /agent for the working guide and GET /v1/bootstrap for key capabilities and accessible inboxes. Every operation carries x-required-role: one of read, assistant, owner or admin (absent means any authenticated principal).",
		},
		"servers":  []map[string]string{{"url": baseURL}},
		"security": []map[string]any{{"bearerAuth": []string{}}},
		"tags":     tags,
		"paths":    paths,
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{"type": "http", "scheme": "bearer"},
			},
			"schemas": Schemas(),
		},
	}
}

// openAPIOperation builds one Operation Object. Every operation carries a
// non-empty responses map (the primary success response plus 401 and default),
// and every {placeholder} in the path is declared as a required path parameter,
// as OpenAPI 3.0.3 requires. When the route names a request or response schema,
// the operation carries the matching requestBody and success content; any
// documented query parameters are emitted after the path parameters.
func openAPIOperation(r Route) map[string]any {
	status := r.Status()
	op := map[string]any{
		"summary": r.Summary,
		"tags":    []string{r.Group},
		"responses": map[string]any{
			strconv.Itoa(status): successResponse(r, status),
			"401":                errorResponse("Unauthorized"),
			"default":            errorResponse("Unexpected error"),
		},
	}
	if r.Description != "" {
		op["description"] = r.Description
	}
	// x-required-role makes the mailbox/admin role machine-readable so a client
	// can pre-filter operations against the roles returned by /v1/bootstrap.
	if r.Role != "" {
		op["x-required-role"] = r.Role
	}
	params := pathParameters(r.Path)
	params = append(params, queryParameters(r.Query)...)
	if len(params) > 0 {
		op["parameters"] = params
	}
	if r.Request != "" {
		contentType := r.RequestContentType
		if contentType == "" {
			contentType = "application/json"
		}
		op["requestBody"] = map[string]any{
			"required": true,
			"content":  map[string]any{contentType: map[string]any{"schema": Ref(r.Request)}},
		}
	}
	return op
}

// successResponse builds the primary success response, attaching the JSON body
// schema when the route names one.
func successResponse(r Route, status int) map[string]any {
	resp := map[string]any{"description": successDescription(status)}
	if r.Response != "" {
		resp["content"] = map[string]any{
			"application/json": map[string]any{"schema": Ref(r.Response)},
		}
	}
	return resp
}

// errorResponse builds an error response referencing the shared Error schema.
func errorResponse(desc string) map[string]any {
	return map[string]any{
		"description": desc,
		"content": map[string]any{
			"application/json": map[string]any{"schema": Ref("Error")},
		},
	}
}

// queryParameters renders the route's documented query parameters. Array
// parameters are encoded as repeated string parameters (form/explode), matching
// how the handlers read r.URL.Query()[name].
func queryParameters(params []Param) []map[string]any {
	if len(params) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(params))
	for _, p := range params {
		schema := map[string]any{"type": p.Type}
		if p.Type == "array" {
			schema["items"] = map[string]any{"type": "string"}
			schema["style"] = "form"
			schema["explode"] = true
		}
		param := map[string]any{
			"name":     p.Name,
			"in":       "query",
			"required": p.Required,
			"schema":   schema,
		}
		if p.Description != "" {
			param["description"] = p.Description
		}
		out = append(out, param)
	}
	return out
}

// pathParameters returns one required string path parameter for each
// {placeholder} in a ServeMux-style path. OpenAPI 3.0.3 requires each template
// expression in a path to be declared as a path parameter.
func pathParameters(path string) []map[string]any {
	var out []map[string]any
	for i := 0; i < len(path); i++ {
		if path[i] != '{' {
			continue
		}
		rel := strings.IndexByte(path[i:], '}')
		if rel < 0 {
			break
		}
		name := path[i+1 : i+rel]
		if name != "" {
			out = append(out, map[string]any{
				"name":     name,
				"in":       "path",
				"required": true,
				"schema":   map[string]any{"type": "string"},
			})
		}
		i += rel
	}
	return out
}

// successDescription maps a primary success status to its standard reason
// phrase. Unknown codes fall back to "OK".
func successDescription(status int) string {
	switch status {
	case 201:
		return "Created"
	case 204:
		return "No content"
	default:
		return "OK"
	}
}
