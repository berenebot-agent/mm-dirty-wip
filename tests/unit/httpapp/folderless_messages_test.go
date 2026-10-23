package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func TestStableFolderIDScopesAccountWideRoutes(t *testing.T) {
	t.Parallel()
	svc, handler, user, _, inbox := httpFixture(t)
	key := adminKey(t, svc, user)
	message := seedInbound(t, svc, inbox, "stable-folder", "<stable-folder@test>", "Located", "body")
	folder, err := svc.Store.GetSystemFolder(context.Background(), user.AccountID, inbox.ID, model.FolderRoleInbox)
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/v1/messages", "/v1/search", "/v1/threads"} {
		result := apiGet(t, handler, route+"?folder="+folder.ID, key)
		var page struct {
			Items []struct {
				ID       string  `json:"id"`
				FolderID *string `json:"folder_id"`
			} `json:"items"`
		}
		if result.Code != http.StatusOK || json.Unmarshal(result.Body.Bytes(), &page) != nil || len(page.Items) != 1 {
			t.Fatalf("folder scope %s: %d %s", route, result.Code, result.Body.String())
		}
		if route == "/v1/threads" {
			if page.Items[0].ID != message.ThreadID {
				t.Fatalf("wrong folder thread: %s", result.Body.String())
			}
		} else if page.Items[0].ID != message.ID || page.Items[0].FolderID == nil || *page.Items[0].FolderID != folder.ID {
			t.Fatalf("wrong canonical location: %s", result.Body.String())
		}
	}
}

func TestLegacyLifecycleClearsSystemFolderPlacement(t *testing.T) {
	t.Parallel()
	svc, handler, user, _, inbox := httpFixture(t)
	ctx := context.Background()
	principal := model.Principal{AccountID: user.AccountID, Admin: true}
	key := adminKey(t, svc, user)
	inboxFolder, err := svc.Store.GetSystemFolder(ctx, user.AccountID, inbox.ID, model.FolderRoleInbox)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{model.FolderRoleSpam, model.FolderRoleTrash} {
		message := seedInbound(t, svc, inbox, "clear-"+role, "<clear-"+role+"@test>", "Restore", "body")
		folder, err := svc.Store.GetSystemFolder(ctx, user.AccountID, inbox.ID, role)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/inboxes/"+inbox.ID+"/messages/"+message.ID+"/move?wait=true", strings.NewReader(`{"folder_id":"`+folder.ID+`"}`))
		request.Header.Set("Authorization", "Bearer "+key)
		request.Header.Set("Content-Type", "application/json")
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, request)
		if result.Code != http.StatusOK {
			t.Fatalf("move to %s: %d %s", role, result.Code, result.Body.String())
		}
		if role == model.FolderRoleSpam {
			_, _, err = svc.Store.SetMessageSpam(ctx, principal, message.ID, false)
		} else {
			_, _, err = svc.Store.RestoreMessage(ctx, principal, message.ID)
		}
		if err != nil {
			t.Fatal(err)
		}
		detail := apiGet(t, handler, "/v1/messages/"+message.ID, key)
		var projection struct {
			FolderID *string `json:"folder_id"`
		}
		if detail.Code != http.StatusOK || json.Unmarshal(detail.Body.Bytes(), &projection) != nil || projection.FolderID == nil || *projection.FolderID != inboxFolder.ID {
			t.Fatalf("%s clear left a contradictory location: %d %s", role, detail.Code, detail.Body.String())
		}
	}
}

func TestFolderlessUnlabelledDomainMessageStaysAccessible(t *testing.T) {
	t.Parallel()
	svc, handler, user, _, inbox := httpFixture(t)
	key := adminKey(t, svc, user)
	message := seedInbound(t, svc, inbox, "folderless", "<folderless@test>", "Unclassified", "body")
	request := httptest.NewRequest(http.MethodPost, "/v1/inboxes/"+inbox.ID+"/messages/"+message.ID+"/move?wait=true", strings.NewReader(`{"folder_id":null}`))
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, request)
	if result.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", result.Code, result.Body.String())
	}
	assertMembership := func(raw json.RawMessage) {
		t.Helper()
		var projection map[string]json.RawMessage
		if err := json.Unmarshal(raw, &projection); err != nil {
			t.Fatal(err)
		}
		if string(projection["folder_id"]) != "null" || string(projection["label_ids"]) != "[]" {
			t.Fatalf("expected accessible folderless, unlabelled message: %s", raw)
		}
		if len(projection["mailbox_id"]) != 0 || len(projection["folder_path"]) != 0 {
			t.Fatalf("legacy location contradicts canonical null: %s", raw)
		}
	}
	assertMembership(result.Body.Bytes())
	for _, path := range []string{
		"/v1/inboxes/" + inbox.ID + "/messages",
		"/v1/messages?inbox=" + inbox.ID,
		"/v1/messages",
		"/v1/inboxes/" + inbox.ID + "/search",
		"/v1/search?inbox=" + inbox.ID,
		"/v1/search",
	} {
		response := apiGet(t, handler, path, key)
		var page struct {
			Items []json.RawMessage `json:"items"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Items) != 1 {
			t.Fatalf("folderless list %s: %d %s", path, response.Code, response.Body.String())
		}
		assertMembership(page.Items[0])
	}
	detail := apiGet(t, handler, "/v1/messages/"+message.ID, key)
	if detail.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", detail.Code, detail.Body.String())
	}
	assertMembership(detail.Body.Bytes())
	thread := apiGet(t, handler, "/v1/threads/"+message.ThreadID+"/messages", key)
	var messages []json.RawMessage
	if thread.Code != http.StatusOK || json.Unmarshal(thread.Body.Bytes(), &messages) != nil || len(messages) != 1 {
		t.Fatalf("folderless thread: %d %s", thread.Code, thread.Body.String())
	}
	assertMembership(messages[0])
}

// TestSentFolderFilesCompletedSend proves a completed send is filed in the Sent
// folder that the message reports, so folder=<Sent> lists it. The system folder
// set is seeded on demand by the send path, because an inbox can complete its
// first send before anything has listed its folder tree.
func TestSentFolderFilesCompletedSend(t *testing.T) {
	t.Parallel()
	svc, handler, user, _, inbox := httpFixture(t)
	ctx := context.Background()
	key := adminKey(t, svc, user)
	before, err := svc.Store.ListFolders(ctx, user.AccountID, inbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Fatalf("fixture expected an unseeded folder tree, got %d folders", len(before))
	}
	message, _, err := svc.Store.CommitOutbound(ctx, store.OutboundRecord{
		Inbox: inbox, Provider: "smtp", RFCMessageID: "<sent-folder@test>",
		From: model.Address{Address: inbox.Address}, To: []string{"recipient@example.test"},
		Subject: "Sent folder", Text: "body", RawPath: "messages/sent-folder.eml", SizeBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Store.MarkSent(ctx, user.AccountID, message.ID, "<wire-sent@test>", "smtp", ""); err != nil {
		t.Fatal(err)
	}
	sent, err := svc.Store.GetSystemFolder(ctx, user.AccountID, inbox.ID, model.FolderRoleSent)
	if err != nil {
		t.Fatal(err)
	}
	filed, err := svc.Store.GetMessage(ctx, model.Principal{AccountID: user.AccountID, Admin: true}, message.ID)
	if err != nil || filed.MailboxID != sent.ID {
		t.Fatalf("completed send mailbox = %q, want the Sent folder %q (err %v)", filed.MailboxID, sent.ID, err)
	}
	for _, route := range []string{
		"/v1/inboxes/" + inbox.ID + "/messages?folder=" + sent.ID,
		"/v1/messages?inbox=" + inbox.ID + "&folder=" + sent.ID,
	} {
		var page struct {
			Items []struct{ ID string } `json:"items"`
		}
		result := apiGet(t, handler, route, key)
		if result.Code != http.StatusOK || json.Unmarshal(result.Body.Bytes(), &page) != nil {
			t.Fatalf("%s: %d %s", route, result.Code, result.Body.String())
		}
		if len(page.Items) != 1 || page.Items[0].ID != message.ID {
			t.Fatalf("%s returned %s, want only %s", route, result.Body.String(), message.ID)
		}
	}
}
