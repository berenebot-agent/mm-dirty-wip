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
