package httpapp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/httpapp"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

// TestAPIMessageTrashLifecycle exercises the message trash/restore/purge API:
// DELETE moves to Trash, the trashed filter lists it, restore returns it, and
// purge erases it.
func TestAPIMessageTrashLifecycle(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	m := seedInbound(t, svc, box, "api-trash-1", "<api-trash-1@test>", "Trash me", "body")
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	// DELETE trashes.
	if rr := do("DELETE", "/v1/messages/"+m.ID, ""); rr.Code != 204 {
		t.Fatalf("delete %d %s", rr.Code, rr.Body.String())
	}
	// Hidden from the ordinary list.
	if rr := do("GET", "/v1/messages?inbox="+box.ID, ""); strings.Contains(rr.Body.String(), m.ID) {
		t.Fatalf("trashed message in default list: %s", rr.Body.String())
	}
	// Visible via the trashed filter.
	rr := do("GET", "/v1/messages?inbox="+box.ID+"&trashed=true", "")
	var env struct {
		Items []model.Message `json:"items"`
	}
	if err = json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Items) != 1 || env.Items[0].ID != m.ID || env.Items[0].DeletedAt == nil {
		t.Fatalf("trashed list %#v", env.Items)
	}

	// Restore returns it.
	if rr = do("POST", "/v1/messages/"+m.ID+"/restore", ""); rr.Code != 200 {
		t.Fatalf("restore %d %s", rr.Code, rr.Body.String())
	}
	if rr = do("GET", "/v1/messages?inbox="+box.ID, ""); !strings.Contains(rr.Body.String(), m.ID) {
		t.Fatalf("restored message missing: %s", rr.Body.String())
	}

	// Purge requires trashed first.
	if rr = do("DELETE", "/v1/messages/"+m.ID+"/purge", ""); rr.Code != 409 {
		t.Fatalf("purge untrashed %d %s", rr.Code, rr.Body.String())
	}
	if rr = do("DELETE", "/v1/messages/"+m.ID, ""); rr.Code != 204 {
		t.Fatalf("re-trash %d", rr.Code)
	}
	if rr = do("DELETE", "/v1/messages/"+m.ID+"/purge", ""); rr.Code != 204 {
		t.Fatalf("purge %d %s", rr.Code, rr.Body.String())
	}
	if _, err := svc.Store.GetMessageByID(ctx, u.AccountID, m.ID); err == nil {
		t.Fatal("purged message still present")
	}
}

// TestAPIInboxTrashEmpty exercises the empty-trash route.
func TestAPIInboxTrashEmpty(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	a := seedInbound(t, svc, box, "api-et-1", "<api-et-1@test>", "a", "a")
	b := seedInbound(t, svc, box, "api-et-2", "<api-et-2@test>", "b", "b")
	keep := seedInbound(t, svc, box, "api-et-3", "<api-et-3@test>", "keep", "keep")
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	for _, id := range []string{a.ID, b.ID} {
		if _, _, err := svc.Store.TrashMessage(ctx, p, id); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest("POST", "/v1/inboxes/"+box.ID+"/trash/empty", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"purged":2`) {
		t.Fatalf("empty trash %d %s", rr.Code, rr.Body.String())
	}
	if _, err := svc.Store.GetMessageByID(ctx, u.AccountID, keep.ID); err != nil {
		t.Fatalf("untrashed message removed: %v", err)
	}
	if _, err := svc.Store.GetMessageByID(ctx, u.AccountID, a.ID); err == nil {
		t.Fatal("trashed message not purged")
	}
}

func TestAPIEmptyTrashSingleOperationOverBatchLimit(t *testing.T) {
	t.Parallel()
	svc, _, u, _, box := httpFixture(t)
	h := httpapp.New(svc, nil).Handler()
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	for i := 0; i < 501; i++ {
		id := fmt.Sprintf("bulk-trash-%d", i)
		m := seedInbound(t, svc, box, id, "<"+id+"@test>", "Trash", "body")
		if _, _, err := svc.Store.TrashMessage(ctx, p, m.ID); err != nil {
			t.Fatal(err)
		}
	}
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, nil)
		r.Header.Set("Authorization", "Bearer "+key)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, r)
		return response
	}
	path := "/v1/inboxes/" + box.ID + "/trash/empty"
	rr := request(path)
	if rr.Code != 202 || strings.Contains(rr.Body.String(), "truncated") {
		t.Fatalf("async operation: %d %s", rr.Code, rr.Body.String())
	}
	var result struct{ Action store.MessageAction }
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Action.Input.Kind != "empty_trash" || rr.Header().Get("Location") != "/v1/actions/"+result.Action.ID {
		t.Fatalf("missing parent handle: %s", rr.Body.String())
	}
	// wait=true must wait for the SAME complete operation, including >500 targets.
	rr = request(path + "?wait=true")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"purged":501`) {
		t.Fatalf("wait for whole operation: %d %s", rr.Code, rr.Body.String())
	}
	got, err := svc.WaitForInboxAction(ctx, p, result.Action.ID, time.Second)
	if err != nil || got.State != "done" || got.EmptyTrash.Purged != 501 {
		t.Fatalf("parent completion: %+v %v", got, err)
	}
	if count, err := svc.Store.CountTrash(ctx, p, box.ID); err != nil || count != 0 {
		t.Fatalf("trash remains: %d %v", count, err)
	}
}

// TestAPIAccountSettingsTrashRetention covers the account settings route.
func TestAPIAccountSettingsTrashRetention(t *testing.T) {
	t.Parallel()
	svc, h, u, _, _ := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/v1/account/settings", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	rr := do("GET", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"trash_retention_days":0`) {
		t.Fatalf("get settings %d %s", rr.Code, rr.Body.String())
	}
	if rr = do("PATCH", `{"trash_retention_days":7}`); rr.Code != 200 {
		t.Fatalf("patch settings %d %s", rr.Code, rr.Body.String())
	}
	if rr = do("GET", ""); !strings.Contains(rr.Body.String(), `"trash_retention_days":7`) {
		t.Fatalf("settings not updated: %s", rr.Body.String())
	}
}

// TestEmptyTrashParentPublishesNoEventWhilePending pins the queue-event budget of
// an Empty Trash: one event per purged message, plus one when the parent
// finishes. The parent re-runs a coordination turn every emptyTrashTurnDelay
// while its children are in flight, and a still-pending turn changes nothing the
// queue shows, so publishing there would make every open page refetch and
// re-render several times a second for the whole operation.
func TestEmptyTrashParentPublishesNoEventWhilePending(t *testing.T) {
	t.Parallel()
	svc, _, u, _, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	const targets = 3
	for i := 0; i < targets; i++ {
		m := seedInbound(t, svc, box, fmt.Sprintf("empty-events-%d", i), fmt.Sprintf("<empty-events-%d@test>", i), "Trash me", "body")
		if _, _, err := svc.Store.TrashMessage(ctx, p, m.ID); err != nil {
			t.Fatal(err)
		}
	}
	_, events, unsubscribe := svc.Hub.Subscribe(64)
	defer unsubscribe()
	a, err := svc.Store.EnqueueEmptyTrash(ctx, p, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	// httpFixture already runs the action worker; it drains on its one-second tick.
	got, err := svc.WaitForInboxAction(ctx, p, a.ID, 10*time.Second)
	if err != nil || got.State != "done" || got.EmptyTrash == nil || got.EmptyTrash.Purged != targets {
		t.Fatalf("empty trash: %+v %v", got, err)
	}
	// The parent settles before it publishes, so collect for a moment after the
	// state was observed rather than stopping at the first completion.
	queueEvents := 0
	for deadline := time.Now().Add(500 * time.Millisecond); time.Now().Before(deadline); {
		select {
		case ev := <-events:
			if ev.Type == "actions.changed" && ev.InboxID == box.ID {
				queueEvents++
			}
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if queueEvents > targets+1 {
		t.Fatalf("queue events = %d, want at most %d (one per purge plus completion): a pending coordination turn published", queueEvents, targets+1)
	}
}
