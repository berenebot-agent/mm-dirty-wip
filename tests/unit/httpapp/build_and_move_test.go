package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestHealthAndDiscoveryReportBuild proves a deployed container can be
// identified without shell access: the running build is reported by /healthz
// and by the discovery document an agent already fetches.
func TestHealthAndDiscoveryReportBuild(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := httpFixture(t)
	svc.Config.Build = "9d7d1ce"
	h := httpapp.New(svc, nil).Handler()
	for _, path := range []string{"/healthz", "/health", "/v1/health", "/.well-known/mailmoose", "/"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Accept", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		var doc map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
			t.Fatalf("%s is not JSON: %d %s", path, rr.Code, rr.Body.String())
		}
		if doc["build"] != "9d7d1ce" {
			t.Errorf("%s build = %v, want the running build", path, doc["build"])
		}
	}
}

// TestReissuedMoveWhileInFlightIsNotRequeued proves a client that re-issues the
// same move while it is in flight does not queue a second one. The action is
// queued, the first submission's answer is discarded by the client, and the
// retry must meet the same row.
func TestReissuedMoveWhileInFlightIsNotRequeued(t *testing.T) {
	// Serial: it parks an action claim for the duration of the test through the
	// package-wide claim hook.
	svc, _, u, _, box := httpFixture(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	m := seedInbound(t, svc, box, "move-dup", "<move-dup@test>", "Move", "body")
	folder, err := svc.Store.GetSystemFolder(context.Background(), u.AccountID, box.ID, model.FolderRoleArchive)
	if err != nil {
		t.Fatal(err)
	}
	in := store.MessageActionInput{Kind: "classify", Destination: folder.ID}
	first, err := svc.EnqueueInboxAction(context.Background(), p, box.ID, m.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.EnqueueInboxAction(context.Background(), p, box.ID, m.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.ID != second.ID {
		t.Fatalf("re-issued move actions = %q then %q, want one queued action", first.ID, second.ID)
	}
	actions, err := svc.Store.ListMessageActionsForMessage(context.Background(), u.AccountID, box.ID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 {
		t.Fatalf("queued moves = %d, want one", len(actions))
	}
}
