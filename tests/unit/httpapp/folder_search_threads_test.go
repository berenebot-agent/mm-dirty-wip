package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func TestFolderFiltersSearchAndThreads(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	folder, err := svc.Store.CreateFolder(ctx, p, box.ID, store.FolderCreate{Path: "Projects"})
	if err != nil {
		t.Fatal(err)
	}
	filed := seedInbound(t, svc, box, "folder-search-filed", "<folder-search-filed@test>", "Project match", "needle")
	if _, _, err := svc.Store.MoveMessageToFolder(ctx, p, filed.ID, folder.ID); err != nil {
		t.Fatal(err)
	}
	loose := seedInbound(t, svc, box, "folder-search-loose", "<folder-search-loose@test>", "Loose match", "needle")
	key := adminKey(t, svc, u)
	folderParam := "folder=" + url.QueryEscape(folder.ID)

	assertOnlyFiled := func(path string) {
		t.Helper()
		rr := apiGet(t, h, path, key)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
		var page struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.Items[0].ID != filed.ID {
			t.Fatalf("%s returned ids %+v, want only %s (exclude %s)", path, page.Items, filed.ID, loose.ID)
		}
	}

	assertOnlyFiled("/v1/search?q=needle&inbox=" + box.ID + "&" + folderParam)
	assertOnlyFiled("/v1/search?q=needle&" + folderParam)
	assertOnlyFiled("/v1/inboxes/" + box.ID + "/search?q=needle&" + folderParam)
	assertOnlyFiled("/v1/messages?" + folderParam)

	for _, path := range []string{
		"/v1/inboxes/" + box.ID + "/threads?" + folderParam,
		"/v1/threads?inbox=" + box.ID + "&" + folderParam,
		"/v1/threads?" + folderParam,
	} {
		rr := apiGet(t, h, path, key)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
		var page struct {
			Items []model.Thread `json:"items"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.Items[0].ID != filed.ThreadID || page.Items[0].MessageCount != 1 {
			t.Fatalf("%s returned threads %+v, want only thread %s with one matching message", path, page.Items, filed.ThreadID)
		}
	}
}
