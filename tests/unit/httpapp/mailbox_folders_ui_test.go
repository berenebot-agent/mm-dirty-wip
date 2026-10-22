package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
)

// TestMessageMoveFilesThroughFolderAction proves the reader's move control
// files a message through the shared folder action. A domain mailbox files
// into its system folders and archives with a null destination, so an archived
// message can be moved back to the Inbox. A label never moves a message.
func TestMessageMoveFilesThroughFolderAction(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	msg := seedInbound(t, svc, box, "move-ui", "<move-ui@test>", "Move me", "body")
	cookie, csrf := uiSession(t, svc, u.ID)

	// The chooser offers the system views a message may be filed into, and the
	// archive action a domain mailbox supports. Sent, Drafts and Outbox are
	// provider-managed and are never destinations.
	inboxFolder, err := svc.Store.GetSystemFolder(ctx, p.AccountID, box.ID, model.FolderRoleInbox)
	if err != nil {
		t.Fatal(err)
	}
	spamFolder, err := svc.Store.GetSystemFolder(ctx, p.AccountID, box.ID, model.FolderRoleSpam)
	if err != nil {
		t.Fatal(err)
	}
	trashFolder, err := svc.Store.GetSystemFolder(ctx, p.AccountID, box.ID, model.FolderRoleTrash)
	if err != nil {
		t.Fatal(err)
	}
	page := uiGet(t, h, cookie, "/ui/messages/"+msg.ID).Body.String()
	for _, want := range []string{
		`<select name="folder_id"`,
		`<option value="` + inboxFolder.ID + `">Inbox</option>`,
		`<option value="` + spamFolder.ID + `">Spam</option>`,
		`<option value="` + trashFolder.ID + `">Trash</option>`,
		`<option value="">Archive</option>`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("the move chooser is missing %q", want)
		}
	}
	if strings.Contains(page, "moves message") {
		t.Fatal("a label must never offer a move")
	}

	// Wait until the queued move has been applied to the message.
	filed := func(want string) {
		t.Helper()
		var got model.Message
		var gerr error
		deadline := time.Now().Add(5 * time.Second)
		for {
			got, gerr = svc.Store.GetMessage(ctx, p, msg.ID)
			if gerr == nil && got.MailboxID == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("message was not filed in %s: %+v (%v)", want, got, gerr)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// An empty destination archives the message.
	rr := uiPost(t, h, cookie, "/ui/messages/"+msg.ID+"/move", "_csrf="+csrf+"&folder_id=")
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("archive move = %d body=%s", rr.Code, rr.Body.String())
	}
	archive, err := svc.Store.GetSystemFolder(ctx, p.AccountID, box.ID, model.FolderRoleArchive)
	if err != nil {
		t.Fatal(err)
	}
	filed(archive.ID)

	// An archived message is recoverable: it moves back to the Inbox.
	rr = uiPost(t, h, cookie, "/ui/messages/"+msg.ID+"/move", "_csrf="+csrf+"&folder_id="+inboxFolder.ID)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("inbox move = %d body=%s", rr.Code, rr.Body.String())
	}
	filed(inboxFolder.ID)
}

// TestIMAPReaderHasNoLabelControl proves a raw IMAP mailbox offers no label
// control in the reader: it has no additive labels, and its folders are the
// only way to file a message.
func TestIMAPReaderHasNoLabelControl(t *testing.T) {
	t.Parallel()
	svc, h, u, _, standalone, fake := standaloneFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)
	fake.add("INBOX", "From: remote@elsewhere.test\r\nSubject: Filed\r\nMessage-ID: <filed@remote>\r\n\r\nbody", "<filed@remote>", "Filed")
	rr := apiGet(t, h, "/v1/inboxes/"+standalone.ID+"/messages", adminKey(t, svc, u))
	var env struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil || len(env.Items) != 1 {
		t.Fatalf("list remote: %d %s", rr.Code, rr.Body.String())
	}
	page := uiGet(t, h, cookie, "/ui/messages/"+env.Items[0].ID).Body.String()
	if strings.Contains(page, `name="label" list="mail-label-suggestions"`) {
		t.Fatal("an IMAP mailbox must not offer an add-label control")
	}
	if strings.Contains(page, `<span>Labels</span>`) {
		t.Fatal("an IMAP mailbox has no labels to list")
	}
	// Its folders file the message instead.
	if !strings.Contains(page, `<select name="folder_id"`) {
		t.Fatal("an IMAP mailbox must offer its folders as move destinations")
	}
}
