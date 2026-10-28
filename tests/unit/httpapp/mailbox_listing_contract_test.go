package httpapp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

func TestMailboxListingPaginationBothURLForms(t *testing.T) {
	t.Parallel()
	svc, h, u, local, remote, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("<pagination-%d@test>", i)
		at := base // tied dates must not reorder a provider's native continuation
		seedInboundAt(t, svc, local, id, id, "needle invoice", "needle", at)
		fake.addAt("INBOX", "From: remote@elsewhere.test\r\nTo: agent@remote.test\r\nSubject: needle invoice\r\nMessage-ID: "+id+"\r\n\r\nneedle", id, "needle invoice", at)
	}
	refreshRemote(t, h, remote.ID, key)
	for _, box := range []model.Inbox{local, remote} {
		for _, resource := range []string{"messages", "threads", "search"} {
			var first []string
			for _, scoped := range []bool{true, false} {
				path := "/v1/" + resource
				q := url.Values{"limit": {"2"}, "inbox": {box.ID}}
				if scoped {
					path = "/v1/inboxes/" + box.ID + "/" + resource
					// Neither a query inbox nor compatibility address may override the path.
					q.Set("inbox", "not-the-path-inbox")
					q.Set("address", "not-the-path@example.test")
				}
				if resource == "search" {
					q.Set("q", "needle")
					q.Set("subject", "invoice")
				}
				var ids []string
				seen := map[string]bool{}
				for page := 0; ; page++ {
					if page > 5 {
						t.Fatalf("%s pagination did not terminate", path)
					}
					rr := apiGet(t, h, path+"?"+q.Encode(), key)
					if rr.Code != http.StatusOK {
						t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
					}
					var env struct {
						Items []struct {
							ID      string `json:"id"`
							InboxID string `json:"inbox_id"`
						} `json:"items"`
						Next string `json:"next_cursor"`
					}
					if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
						t.Fatal(err)
					}
					for _, item := range env.Items {
						if seen[item.ID] || item.InboxID != box.ID {
							t.Fatalf("duplicate or cross-inbox item in %s: %+v", path, item)
						}
						seen[item.ID] = true
						ids = append(ids, item.ID)
					}
					if env.Next == "" {
						break
					}
					q.Set("before", env.Next)
				}
				if len(ids) != 5 {
					t.Fatalf("%s (%s) got %d items: %v", path, box.Kind, len(ids), ids)
				}
				if first != nil && strings.Join(first, ",") != strings.Join(ids, ",") {
					t.Fatalf("URL forms disagree for %s: %v vs %v", resource, first, ids)
				}
				first = ids
			}
		}
	}
}

// TestMailboxFiltersAndSearchFolderBothKinds proves a folder filter narrows
// the listing to that folder and composes with the other filters, for both
// inbox kinds. A domain mailbox's canonical folders are its system folders,
// so the filter names the system Spam folder; a standalone mailbox keeps its
// physical folders.
func TestMailboxFiltersAndSearchFolderBothKinds(t *testing.T) {
	t.Parallel()
	svc, h, u, local, remote, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	ctx := context.Background()
	// A legacy custom folder: a domain mailbox organises with labels now, so
	// it is not a canonical location.
	legacy, err := svc.Store.CreateFolder(ctx, p, local.ID, store.FolderCreate{Path: "Legacy"})
	if err != nil {
		t.Fatal(err)
	}
	spam, err := svc.Store.GetSystemFolder(ctx, u.AccountID, local.ID, model.FolderRoleSpam)
	if err != nil {
		t.Fatal(err)
	}
	want := seedInbound(t, svc, local, "wanted-filter", "<wanted-filter@test>", "needle invoice", "needle")
	if _, _, err := svc.Store.SetMessageSpam(ctx, p, want.ID, true); err != nil {
		t.Fatal(err)
	}
	seedInbound(t, svc, local, "unwanted-filter", "<unwanted-filter@test>", "needle other", "needle")
	fake.mu.Lock()
	fake.folders["Projects"] = 100
	fake.mu.Unlock()
	fake.add("Projects", "From: remote@elsewhere.test\r\nSubject: needle invoice\r\nMessage-ID: <wanted-remote@test>\r\n\r\nneedle", "<wanted-remote@test>", "needle invoice")
	fake.add("INBOX", "From: remote@elsewhere.test\r\nSubject: needle other\r\nMessage-ID: <other-remote@test>\r\n\r\nneedle", "<other-remote@test>", "needle other")
	refreshRemote(t, h, remote.ID, key)
	for _, tc := range []struct{ inbox, folder, from string }{
		{local.ID, spam.ID, "sender@outside.test"},
		{remote.ID, "Projects", "remote@elsewhere.test"},
	} {
		for _, resource := range []string{"messages", "search"} {
			for _, scoped := range []bool{true, false} {
				path := "/v1/" + resource
				q := url.Values{"inbox": {tc.inbox}, "folder": {tc.folder}, "from": {tc.from}, "unread": {"true"}, "has_attachment": {"false"}}
				if scoped {
					path = "/v1/inboxes/" + tc.inbox + "/" + resource
				}
				if resource == "search" {
					q.Set("q", "needle")
					q.Set("subject", "invoice")
				}
				for _, match := range []bool{true, false} {
					if !match {
						q.Set("from", "absent@example.test")
					}
					rr := apiGet(t, h, path+"?"+q.Encode(), key)
					var env struct {
						Items []json.RawMessage `json:"items"`
					}
					if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &env) != nil {
						t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
					}
					count := 0
					if match {
						count = 1
					}
					if len(env.Items) != count {
						t.Fatalf("%s?%s: want %d: %s", path, q.Encode(), count, rr.Body.String())
					}
				}
			}
		}
	}
	// A legacy custom folder is not a canonical location: it matches nothing.
	for _, resource := range []string{"messages", "search"} {
		path := "/v1/inboxes/" + local.ID + "/" + resource
		q := url.Values{"folder": {legacy.ID}}
		if resource == "search" {
			q.Set("q", "needle")
		}
		rr := apiGet(t, h, path+"?"+q.Encode(), key)
		var env struct {
			Items []json.RawMessage `json:"items"`
		}
		if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &env) != nil {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
		if len(env.Items) != 0 {
			t.Fatalf("%s?%s returned %d items, want none: a legacy custom folder is not a canonical location", path, q.Encode(), len(env.Items))
		}
	}
}

func TestSearchRejectsObsoleteCursorParameter(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, _ := standaloneFixture(t)
	rr := apiGet(t, h, "/v1/inboxes/"+box.ID+"/search?q=needle&cursor=123", adminKey(t, svc, u))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "before") {
		t.Fatalf("obsolete cursor: %d %s", rr.Code, rr.Body.String())
	}
}

func TestPartialRemoteIndexReportedAcrossListingRoutes(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, _ := standaloneFixture(t)
	key := adminKey(t, svc, u)
	refreshRemote(t, h, box.ID, key)
	if err := svc.Store.SetRemoteIndexStatus(context.Background(), u.AccountID, box.ID, store.RemoteIndexPartial, ""); err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{"messages", "threads"} {
		for _, path := range []string{
			"/v1/inboxes/" + box.ID + "/" + resource,
			"/v1/" + resource + "?inbox=" + box.ID,
			"/v1/" + resource,
		} {
			rr := apiGet(t, h, path, key)
			var env struct {
				Completeness string `json:"completeness"`
			}
			if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &env) != nil || env.Completeness != "partial" {
				t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
			}
		}
	}
}

func TestLocalListingMaximumPageAndTimestampTies(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	key := adminKey(t, svc, u)
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 205; i++ {
		id := fmt.Sprintf("<max-page-%d@test>", i)
		seedInboundAt(t, svc, box, id, id, "needle", "needle", at)
	}
	for _, resource := range []string{"messages", "threads", "search"} {
		q := url.Values{"limit": {"200"}, "q": {"needle"}}
		seen := map[string]bool{}
		for page := 0; ; page++ {
			if page > 2 {
				t.Fatalf("%s did not terminate", resource)
			}
			rr := apiGet(t, h, "/v1/inboxes/"+box.ID+"/"+resource+"?"+q.Encode(), key)
			var env struct {
				Items []struct {
					ID string `json:"id"`
				} `json:"items"`
				Next string `json:"next_cursor"`
			}
			if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &env) != nil {
				t.Fatalf("%s: %d %s", resource, rr.Code, rr.Body.String())
			}
			if page == 0 && len(env.Items) != 200 {
				t.Fatalf("%s reset maximum limit: %d", resource, len(env.Items))
			}
			for _, item := range env.Items {
				if seen[item.ID] {
					t.Fatalf("%s duplicated %s", resource, item.ID)
				}
				seen[item.ID] = true
			}
			if env.Next == "" {
				break
			}
			q.Set("before", env.Next)
		}
		if len(seen) != 205 {
			t.Fatalf("%s lost timestamp-tied items: %d", resource, len(seen))
		}
	}
}

func TestRemoteRoleFiltersAcrossListingRoutes(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	fake.add("INBOX", "From: remote@elsewhere.test\r\nMessage-ID: <ordinary@test>\r\n\r\nbody", "<ordinary@test>", "ordinary")
	refreshRemote(t, h, box.ID, key)
	check := func(filter, subject string, count int) {
		t.Helper()
		for _, path := range []string{
			"/v1/inboxes/" + box.ID + "/messages?" + filter,
			"/v1/messages?inbox=" + box.ID + "&" + filter,
			"/v1/messages?" + filter,
		} {
			rr := apiGet(t, h, path, key)
			var env struct {
				Items []struct {
					Subject string `json:"subject"`
				} `json:"items"`
			}
			if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &env) != nil || len(env.Items) != count {
				t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
			}
			if count > 0 && env.Items[0].Subject != subject {
				t.Fatalf("%s ignored role filter: %s", path, rr.Body.String())
			}
		}
	}
	check("trashed=true", "", 0) // no mapped Trash must not fall back to Inbox
	fake.mu.Lock()
	fake.folders["Trash"], fake.folders["Spam"] = 100, 100
	fake.mu.Unlock()
	fake.add("Trash", "From: remote@elsewhere.test\r\nMessage-ID: <trash@test>\r\n\r\nbody", "<trash@test>", "trashed")
	fake.add("Spam", "From: remote@elsewhere.test\r\nMessage-ID: <spam@test>\r\n\r\nbody", "<spam@test>", "spam")
	refreshRemote(t, h, box.ID, key)
	check("trashed=true", "trashed", 1)
	check("spam=true", "spam", 1)
}
