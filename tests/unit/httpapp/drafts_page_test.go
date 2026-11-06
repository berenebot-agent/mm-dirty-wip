package httpapp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dellarb/mammothmail/internal/store"
)

// TestStandaloneDraftsPageShowsPlacedRemoteDraft proves a draft placed in a
// standalone inbox's connected Drafts folder is visible in the local Drafts view,
// read-only, without opening or polling the server: the page reads the cached
// remote index. The count badge and the live fragment must agree with the page,
// because the badge is what tells the operator the placed draft is there.
func TestStandaloneDraftsPageShowsPlacedRemoteDraft(t *testing.T) {
	t.Parallel()
	svc, h, u, domainBox, box, _ := standaloneFixture(t)
	ctx := context.Background()
	key := adminKey(t, svc, u)
	// Establish the connected mailbox's folder map (INBOX/Drafts/Sent) the way a
	// normal reconcile would, then seed the index with a placed draft.
	refreshRemote(t, h, box.ID, key)
	if _, err := svc.Store.UpsertRemoteMessage(ctx, u.AccountID, box.ID, store.RemoteMessageInput{
		FolderPath:   "Drafts",
		UIDValidity:  100,
		UID:          77,
		RFCMessageID: "<placed-ui@remote.test>",
		FromName:     "Agent",
		FromAddress:  "agent@remote.test",
		To:           []string{"someone@example.com"},
		Subject:      "Placed draft subject",
		Snippet:      "placed snippet",
		SizeBytes:    240,
		Read:         true,
		Draft:        true,
		Flags:        []string{"\\Draft", "\\Seen"},
	}); err != nil {
		t.Fatal(err)
	}

	cookie, _ := uiSession(t, svc, u.ID)
	page := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts").Body.String()
	if !strings.Contains(page, "Placed draft subject") {
		t.Fatalf("drafts page does not list the placed remote draft: %s", page)
	}
	if !strings.Contains(page, "In mailbox Drafts") {
		t.Fatal("drafts page does not label the placed rows as the connected mailbox's")
	}
	if !strings.Contains(page, `data-count="drafts"`) {
		t.Fatal("drafts badge missing: the placed draft must count like a draft")
	}

	// The live fragment must render the same list, so an open tab picks the placed
	// draft up on its next snapshot without a reload.
	frag := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/live?part=list&folder=drafts").Body.String()
	if !strings.Contains(frag, "Placed draft subject") {
		t.Fatalf("drafts live fragment does not carry the placed draft: %s", frag)
	}

	// The live state snapshot backs the badge; it must count the placed draft too.
	state := uiGet(t, h, cookie, "/ui/state?page=inbox&inbox="+box.ID).Body.String()
	var snap struct {
		Drafts int `json:"drafts"`
	}
	if err := json.Unmarshal([]byte(state), &snap); err != nil {
		t.Fatalf("decode state: %v (%s)", err, state)
	}
	if snap.Drafts != 1 {
		t.Fatalf("state drafts = %d, want 1 for the placed draft", snap.Drafts)
	}

	// A domain inbox has no connected Drafts folder, so its page must never claim
	// placed rows.
	if body := uiGet(t, h, cookie, "/ui/inboxes/"+domainBox.ID+"/drafts").Body.String(); strings.Contains(body, "In mailbox Drafts") {
		t.Fatal("a domain inbox's drafts page must not render the remote-drafts group")
	}
}
