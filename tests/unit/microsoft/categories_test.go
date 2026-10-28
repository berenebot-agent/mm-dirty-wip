package microsoft_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mammothmail/internal/transport/microsoft"
)

func TestNativeCategories(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/mail@example.test/outlook/masterCategories" || r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("unexpected category request: %s %s", r.Method, r.URL)
			http.Error(w, "bad request", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(map[string]any{"value": []microsoft.Category{{ID: "native", DisplayName: "Finance"}}})
			return
		}
		var input microsoft.Category
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&input) != nil || input.DisplayName != "Urgent" || input.Color != "preset0" {
			t.Errorf("invalid category creation: %+v", input)
			http.Error(w, "bad request", 400)
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(microsoft.Category{ID: "native-new", DisplayName: input.DisplayName, Color: input.Color})
	}))
	defer server.Close()
	client := microsoft.NewClient(server.Client())
	client.BaseURL = server.URL
	items, err := client.Categories(context.Background(), "token", "mail@example.test")
	if err != nil || len(items) != 1 || items[0].DisplayName != "Finance" {
		t.Fatalf("category list: %+v %v", items, err)
	}
	category, err := client.CreateCategory(context.Background(), "token", "mail@example.test", "Urgent")
	if err != nil || category.DisplayName != "Urgent" {
		t.Fatalf("category create: %+v %v", category, err)
	}
	if !strings.Contains(microsoft.Scope(false), "MailboxSettings.ReadWrite") {
		t.Fatal("native category permission not requested")
	}
}

func TestCategoriesRejectCrossOriginContinuation(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"value": []any{}, "@odata.nextLink": "https://other.example/users/mail@example.test/outlook/masterCategories"})
	}))
	defer server.Close()
	client := microsoft.NewClient(server.Client())
	client.BaseURL = server.URL
	if _, err := client.Categories(context.Background(), "token", "mail@example.test"); err == nil {
		t.Fatal("accepted cross-origin category continuation")
	}
}

// TestCategoryRenameAndDeleteResolveTheNativeID proves a rename and a delete
// address the category by the id Graph assigned it, not by the display name a
// client happens to know, and that an unknown name is not found rather than
// guessed at.
func TestCategoryRenameAndDeleteResolveTheNativeID(t *testing.T) {
	t.Parallel()
	var renamed, deleted string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/users/mail@example.test/outlook/masterCategories" && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"value": []microsoft.Category{{ID: "native-finance", DisplayName: "Finance"}, {ID: "native-urgent", DisplayName: "Urgent"}}})
		case r.URL.Path == "/users/mail@example.test/outlook/masterCategories/native-urgent" && r.Method == http.MethodPatch:
			var input microsoft.Category
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.DisplayName != "Critical" {
				t.Errorf("invalid category rename: %+v %v", input, err)
			}
			renamed = "native-urgent"
			json.NewEncoder(w).Encode(map[string]any{"id": "native-urgent", "displayName": "Critical", "color": "preset0"})
		case r.URL.Path == "/users/mail@example.test/outlook/masterCategories/native-finance" && r.Method == http.MethodDelete:
			deleted = "native-finance"
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected category request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "bad request", 400)
		}
	}))
	defer server.Close()
	client := microsoft.NewClient(server.Client())
	client.BaseURL = server.URL
	ctx := context.Background()
	// A case-insensitive rename of an existing category.
	category, err := client.RenameCategory(ctx, "token", "mail@example.test", "urgent", "Critical")
	if err != nil || category.DisplayName != "Critical" || renamed != "native-urgent" {
		t.Fatalf("category rename: %+v %v (patched %q)", category, err, renamed)
	}
	if err := client.DeleteCategory(ctx, "token", "mail@example.test", "Finance"); err != nil || deleted != "native-finance" {
		t.Fatalf("category delete: %v (deleted %q)", err, deleted)
	}
	// The provider does not have that category, so nothing is patched.
	if err := client.DeleteCategory(ctx, "token", "mail@example.test", "NotARealCategory"); err == nil {
		t.Fatal("deleted a category the provider does not have")
	}
}
