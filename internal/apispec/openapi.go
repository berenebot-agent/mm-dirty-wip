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
			"description": "Agent-first REST API for Gatehouse Mail. See /agent for the working guide and GET /v1/bootstrap for key capabilities and accessible inboxes.",
		},
		"servers":  []map[string]string{{"url": baseURL}},
		"security": []map[string]any{{"bearerAuth": []string{}}},
		"tags":     tags,
		"paths":    paths,
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{"type": "http", "scheme": "bearer"},
			},
		},
	}
}

// openAPIOperation builds one Operation Object. Every operation carries a
// non-empty responses map (the primary success response plus 401 and default),
// and every {placeholder} in the path is declared as a required path parameter,
// as OpenAPI 3.0.3 requires.
func openAPIOperation(r Route) map[string]any {
	status := r.Status()
	op := map[string]any{
		"summary": r.Summary,
		"tags":    []string{r.Group},
		"responses": map[string]any{
			strconv.Itoa(status): map[string]any{"description": successDescription(status)},
			"401":                map[string]any{"description": "Unauthorized"},
			"default":            map[string]any{"description": "Unexpected error"},
		},
	}
	if r.Description != "" {
		op["description"] = r.Description
	}
	if params := pathParameters(r.Path); len(params) > 0 {
		op["parameters"] = params
	}
	return op
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
