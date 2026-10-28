package httpapp_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestListingCursorsFailClosed(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	key := adminKey(t, svc, u)
	for i, subject := range []string{"cursor contract first", "cursor contract second", "cursor contract third"} {
		seedInbound(t, svc, box, "cursor-contract-"+subject, "<cursor-"+string(rune('a'+i))+"@test>", subject, "cursor contract")
	}
	// A real continuation must advance, not repeat page one.
	first := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages?limit=1", key)
	if first.Code != http.StatusOK {
		t.Fatalf("first page: %d %s", first.Code, first.Body.String())
	}
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("no cursor: %s", first.Body.String())
	}
	next := apiGet(t, h, "/v1/inboxes/"+box.ID+"/messages?limit=1&before="+url.QueryEscape(page.NextCursor), key)
	if next.Code != http.StatusOK {
		t.Fatalf("valid cursor: %d %s", next.Code, next.Body.String())
	}
	var second struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(next.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Items[0].ID == page.Items[0].ID {
		t.Fatalf("pagination did not advance: %s -> %s", first.Body.String(), next.Body.String())
	}
	paths := []string{
		"/v1/messages?inbox=" + box.ID,
		"/v1/inboxes/" + box.ID + "/messages",
		"/v1/threads?inbox=" + box.ID,
		"/v1/inboxes/" + box.ID + "/threads",
		"/v1/search?inbox=" + box.ID + "&q=cursor",
		"/v1/inboxes/" + box.ID + "/search?q=cursor",
	}
	for _, path := range paths {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		for _, bad := range []string{"abc", "msg_06zzzzzzzzzzzzzzzzzzzzzzzzzz"} {
			res := apiGet(t, h, path+sep+"before="+url.QueryEscape(bad), key)
			if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "invalid cursor") {
				t.Errorf("%s before=%s: got %d %s, want 400 invalid cursor", path, bad, res.Code, res.Body.String())
			}
		}
		legacy := apiGet(t, h, path+sep+"cursor=abc", key)
		if legacy.Code != http.StatusBadRequest || !strings.Contains(legacy.Body.String(), "use before") {
			t.Errorf("%s legacy cursor: got %d %s, want migration hint", path, legacy.Code, legacy.Body.String())
		}
	}
}

// TestAccountWideListingsBoundHugeLimit guards the merged listings against an
// unbounded limit sizing their up-front allocations: before the clamp,
// limit=1000000000 reserved gigabytes and took the process down.
func TestAccountWideListingsBoundHugeLimit(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	key := adminKey(t, svc, u)
	seedInbound(t, svc, box, "huge-limit", "<huge-limit@test>", "huge limit", "huge limit")
	for _, path := range []string{"/v1/messages", "/v1/threads", "/v1/search?q=huge"} {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		rr := apiGet(t, h, path+sep+"limit=1000000000", key)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
	}
}
