package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestAPIMessagesFolderFilter proves the account-wide message route honours the
// folder filter exactly as the inbox-scoped route does: a folder id narrows the
// result to that folder, an unknown folder id yields an empty list rather than
// the whole inbox, and no folder filter still lists everything.
func TestAPIMessagesFolderFilter(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	folder, err := svc.Store.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	filed := seedInbound(t, svc, box, "folder-filter-filed", "<folder-filter-filed@test>", "Filed", "body")
	if _, _, err := svc.Store.MoveMessageToFolder(ctx, p, filed.ID, folder.ID); err != nil {
		t.Fatal(err)
	}
	seedInbound(t, svc, box, "folder-filter-loose", "<folder-filter-loose@test>", "Loose", "body")
	key := adminKey(t, svc, u)

	ids := func(path string) []string {
		t.Helper()
		rr := apiGet(t, h, path, key)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
		var page struct {
			Items []struct{ ID string } `json:"items"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(page.Items))
		for _, item := range page.Items {
			out = append(out, item.ID)
		}
		return out
	}

	// The account-wide route narrows to the folder, exactly like the
	// inbox-scoped route.
	scoped := ids("/v1/inboxes/" + box.ID + "/messages?folder=" + url.QueryEscape(folder.ID))
	account := ids("/v1/messages?inbox=" + box.ID + "&folder=" + url.QueryEscape(folder.ID))
	if len(scoped) != 1 || scoped[0] != filed.ID {
		t.Fatalf("inbox-scoped folder filter = %v, want only %s", scoped, filed.ID)
	}
	if len(account) != 1 || account[0] != filed.ID {
		t.Fatalf("account-wide folder filter = %v, want only %s", account, filed.ID)
	}
	// An unknown folder id narrows to nothing; it never returns the whole inbox.
	if got := ids("/v1/messages?inbox=" + box.ID + "&folder=fld_missing"); len(got) != 0 {
		t.Fatalf("unknown folder filter = %v, want no rows", got)
	}
	// Without a folder filter the listing is unchanged.
	if got := ids("/v1/messages?inbox=" + box.ID); len(got) != 2 {
		t.Fatalf("unfiltered listing = %v, want both messages", got)
	}
}

// TestImplicitInboxFolderFilterAcrossAPIAndUI checks that the system Inbox
// folder id includes new, implicitly filed mail and explicitly filed mail, but
// never mail moved to a custom folder or mail in another inbox.
func TestImplicitInboxFolderFilterAcrossAPIAndUI(t *testing.T) {
	t.Parallel()
	svc, h, u, domain, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	inboxFolder, err := svc.Store.GetSystemFolder(ctx, u.AccountID, box.ID, model.FolderRoleInbox)
	if err != nil {
		t.Fatal(err)
	}
	custom, err := svc.Store.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	otherBox, err := svc.Store.CreateInbox(ctx, u.AccountID, domain.ID, "other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	implicit := seedInbound(t, svc, box, "implicit-inbox-uat", "<implicit-inbox-uat@test>", "Implicit Inbox UAT", "needle")
	explicit := seedInbound(t, svc, box, "explicit-inbox-uat", "<explicit-inbox-uat@test>", "Explicit Inbox UAT", "needle")
	moved := seedInbound(t, svc, box, "custom-inbox-uat", "<custom-inbox-uat@test>", "Custom Folder UAT", "needle")
	other := seedInbound(t, svc, otherBox, "other-inbox-uat", "<other-inbox-uat@test>", "Other Inbox UAT", "needle")
	if _, _, err := svc.Store.MoveMessageToFolder(ctx, p, explicit.ID, inboxFolder.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Store.MoveMessageToFolder(ctx, p, moved.ID, custom.ID); err != nil {
		t.Fatal(err)
	}
	key := adminKey(t, svc, u)
	param := "folder=" + url.QueryEscape(inboxFolder.ID)
	for _, path := range []string{
		"/v1/inboxes/" + box.ID + "/messages?" + param,
		"/v1/messages?inbox=" + box.ID + "&" + param,
		"/v1/messages?" + param,
		"/v1/inboxes/" + box.ID + "/search?q=needle&" + param,
		"/v1/search?inbox=" + box.ID + "&q=needle&" + param,
		"/v1/search?q=needle&" + param,
	} {
		rr := apiGet(t, h, path, key)
		var page struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &page) != nil {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
		seen := map[string]bool{}
		for _, item := range page.Items {
			seen[item.ID] = true
		}
		if len(page.Items) != 2 || !seen[implicit.ID] || !seen[explicit.ID] {
			t.Fatalf("%s: got %v, want implicit and explicit Inbox only (exclude %s and %s)", path, seen, moved.ID, other.ID)
		}
	}
	for _, path := range []string{
		"/v1/inboxes/" + box.ID + "/threads?" + param,
		"/v1/threads?inbox=" + box.ID + "&" + param,
		"/v1/threads?" + param,
	} {
		rr := apiGet(t, h, path, key)
		var page struct {
			Items []model.Thread `json:"items"`
		}
		if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &page) != nil {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
		seen := map[string]bool{}
		for _, th := range page.Items {
			if th.MessageCount != 1 {
				t.Fatalf("%s: thread %s has %d messages, want 1", path, th.ID, th.MessageCount)
			}
			seen[th.ID] = true
		}
		if len(page.Items) != 2 || !seen[implicit.ThreadID] || !seen[explicit.ThreadID] {
			t.Fatalf("%s: got threads %v, want implicit and explicit Inbox only", path, seen)
		}
	}
	// Thread pagination must use the same Inbox predicate to resolve before.
	first := apiGet(t, h, "/v1/inboxes/"+box.ID+"/threads?"+param+"&limit=1", key)
	var page1 struct {
		Items      []model.Thread `json:"items"`
		NextCursor string         `json:"next_cursor"`
	}
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &page1) != nil || len(page1.Items) != 1 || page1.NextCursor == "" {
		t.Fatalf("first Inbox thread page: %d %s", first.Code, first.Body.String())
	}
	second := apiGet(t, h, "/v1/inboxes/"+box.ID+"/threads?"+param+"&limit=1&before="+url.QueryEscape(page1.NextCursor), key)
	var page2 struct {
		Items []model.Thread `json:"items"`
	}
	if second.Code != http.StatusOK || json.Unmarshal(second.Body.Bytes(), &page2) != nil || len(page2.Items) != 1 || page2.Items[0].ID == page1.Items[0].ID {
		t.Fatalf("second Inbox thread page: %d %s", second.Code, second.Body.String())
	}

	cookie, csrf := uiSession(t, svc, u.ID)
	ui := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID)
	body := ui.Body.String()
	if ui.Code != http.StatusOK || !strings.Contains(body, "Implicit Inbox UAT") || !strings.Contains(body, "Explicit Inbox UAT") ||
		strings.Contains(body, "Custom Folder UAT") || strings.Contains(body, "Other Inbox UAT") ||
		!strings.Contains(body, `data-select-banner data-total="2"`) {
		t.Fatalf("local Inbox UI must list/count only Inbox mail: %d %s", ui.Code, body)
	}
	// Select-all in the Inbox must not mark archived/custom-folder mail read.
	bulk := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/bulk",
		url.Values{"_csrf": {csrf}, "folder": {"inbox"}, "scope": {"all"}, "action": {"read"}}.Encode())
	if bulk.Code != http.StatusSeeOther {
		t.Fatalf("Inbox bulk mark read: %d %s", bulk.Code, bulk.Body.String())
	}
	for _, tc := range []struct {
		msg  model.Message
		read bool
	}{
		{implicit, true}, {explicit, true}, {moved, false}, {other, false},
	} {
		got, err := svc.Store.GetMessage(ctx, p, tc.msg.ID)
		if err != nil || got.Read != tc.read {
			t.Fatalf("Inbox bulk filter updated %s: read=%v, want %v, err=%v", tc.msg.ID, got.Read, tc.read, err)
		}
	}
}
