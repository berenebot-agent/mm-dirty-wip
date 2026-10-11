package httpapp_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
)

// Existing lifecycle tests assert settled mutations. Exercise the explicit
// wait mode now that submission defaults to asynchronous. Dedicated action API
// tests use the raw handler to verify pending responses and polling instead.
func settledActionFixture(t *testing.T, svc *app.Service, rm *app.RemoteMailboxService, h http.Handler, account string) http.Handler {
	t.Helper()
	w := app.NewActionWorker(svc, rm)
	svc.ActionWaker = w
	w.Start()
	t.Cleanup(w.Stop)
	return http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		mutation := r.Method == "PATCH" || r.Method == "DELETE" || r.Method == "POST"
		if mutation && (strings.HasPrefix(r.URL.Path, "/v1/messages/") || strings.HasSuffix(r.URL.Path, "/trash/empty")) {
			q := r.URL.Query()
			q.Set("wait", "true")
			r.URL.RawQuery = q.Encode()
		}
		h.ServeHTTP(out, r)
		if mutation && (strings.HasPrefix(r.URL.Path, "/ui/messages/") || strings.HasSuffix(r.URL.Path, "/trash/empty")) {
			waitUIActions(t, svc, account)
		}
	})
}

func waitUIActions(t *testing.T, svc *app.Service, account string) {
	t.Helper()
	p := model.Principal{AccountID: account, Admin: true}
	actions, err := svc.Store.ListMessageActions(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range actions {
		if a.State == "pending" || a.State == "running" {
			if _, err := svc.WaitForInboxAction(context.Background(), p, a.ID, 3*time.Second); err != nil {
				t.Fatal(err)
			}
		}
	}
}
