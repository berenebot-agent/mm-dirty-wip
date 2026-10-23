package microsoft_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/transport/microsoft"
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
