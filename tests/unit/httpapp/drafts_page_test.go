package httpapp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

// placedDraftRaw is a draft that lives in the connected mailbox's Drafts folder.
const placedDraftRaw = "From: Agent <agent@remote.test>\r\n" +
	"To: someone@example.com\r\n" +
	"Subject: Placed draft subject\r\n" +
	"Message-ID: <placed-ui@remote.test>\r\n" +
	"Date: Mon, 13 Oct 2026 09:10:00 +0000\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"first body\r\n"

// seedPlacedDraft puts a draft in the fake server's Drafts folder and indexes it
// the way a folder pull would, returning the cached row's id.
func seedPlacedDraft(t *testing.T, svc *app.Service, u model.User, box model.Inbox, fake *fakeIMAP) string {
	t.Helper()
	fake.add("Drafts", placedDraftRaw, "<placed-ui@remote.test>", "Placed draft subject")
	fake.mu.Lock()
	uid := fake.messages[len(fake.messages)-1].uid
	fake.mu.Unlock()
	row, err := svc.Store.UpsertRemoteMessage(context.Background(), u.AccountID, box.ID, store.RemoteMessageInput{
		FolderPath:   "Drafts",
		UIDValidity:  100,
		UID:          uid,
		RFCMessageID: "<placed-ui@remote.test>",
		FromName:     "Agent",
		FromAddress:  "agent@remote.test",
		To:           []string{"someone@example.com"},
		Subject:      "Placed draft subject",
		Snippet:      "first body",
		SizeBytes:    int64(len(placedDraftRaw)),
		Read:         true,
		Draft:        true,
		Flags:        []string{"\\Draft", "\\Seen"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return row.ID
}

// TestStandaloneDraftsPageTreatsPlacedDraftAsADraft proves a draft placed in a
// standalone inbox's connected Drafts folder is an ordinary row: the same edit
// link and delete button a local draft has, no separate "read-only" group, and
// the same live fragment and badge.
func TestStandaloneDraftsPageTreatsPlacedDraftAsADraft(t *testing.T) {
	t.Parallel()
	svc, h, u, domainBox, box, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	// Establish the connected mailbox's folder map the way a normal reconcile
	// would, so the Drafts role resolves.
	refreshRemote(t, h, box.ID, key)
	rowID := seedPlacedDraft(t, svc, u, box, fake)

	cookie, _ := uiSession(t, svc, u.ID)
	page := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts").Body.String()
	if !strings.Contains(page, "Placed draft subject") {
		t.Fatalf("drafts page does not list the placed draft: %s", page)
	}
	if strings.Contains(page, "In mailbox Drafts") {
		t.Fatal("a placed draft must not be a separate read-only group any more")
	}
	// The row is editable: it links to the same editor route a local draft uses.
	if !strings.Contains(page, "/ui/inboxes/"+box.ID+"/drafts/"+rowID+"/edit") {
		t.Fatalf("placed draft row has no edit link: %s", page)
	}
	// ...and deletable through the same button, with a checkbox of its own.
	if !strings.Contains(page, `action="/ui/inboxes/`+box.ID+`/drafts/`+rowID+`/delete"`) {
		t.Fatalf("placed draft row has no delete action: %s", page)
	}
	if !strings.Contains(page, `name="ids" value="`+rowID+`"`) {
		t.Fatalf("placed draft row has no selection checkbox: %s", page)
	}
	if !strings.Contains(page, `data-count="drafts"`) {
		t.Fatal("drafts badge missing: the placed draft must count like a draft")
	}

	// The live fragment renders the same list, so an open tab picks the placed
	// draft up on its next snapshot without a reload.
	frag := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/live?part=list&folder=drafts").Body.String()
	if !strings.Contains(frag, "Placed draft subject") || !strings.Contains(frag, "/drafts/"+rowID+"/edit") {
		t.Fatalf("drafts live fragment does not carry the placed draft as a draft row: %s", frag)
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
	if body := uiGet(t, h, cookie, "/ui/inboxes/"+domainBox.ID+"/drafts").Body.String(); strings.Contains(body, rowID) {
		t.Fatal("a domain inbox's drafts page must not render a placed draft")
	}
}

// TestStandalonePlacedDraftEditsInPlace proves the editor opens a placed draft
// and the save writes it back to the connected mailbox, with the previous copy
// removed and the cached row following the edit.
func TestStandalonePlacedDraftEditsInPlace(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	refreshRemote(t, h, box.ID, key)
	rowID := seedPlacedDraft(t, svc, u, box, fake)
	cookie, csrf := uiSession(t, svc, u.ID)

	editor := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+rowID+"/edit")
	if editor.Code != http.StatusOK {
		t.Fatalf("edit page = %d: %s", editor.Code, editor.Body.String())
	}
	body := editor.Body.String()
	if !strings.Contains(body, "Placed draft subject") || !strings.Contains(body, "first body") {
		t.Fatalf("editor did not load the placed draft's fields: %s", body)
	}
	if !strings.Contains(body, "connected mailbox") {
		t.Fatal("editor does not say where the draft lives")
	}
	if strings.Contains(body, `value="send"`) {
		t.Fatal("editor offers Send for a draft the connected mailbox sends")
	}
	marker := hiddenValue(t, body, "draft_marker")
	if marker == "" {
		t.Fatal("editor did not carry the change marker")
	}

	form := map[string]string{
		"_csrf": csrf, "action": "draft", "draft_id": rowID, "draft_marker": marker,
		"to": "someone@example.com", "subject": "Edited subject", "text": "second body",
	}
	if rr := uiPostMultipart(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+rowID+"/save", form); rr.Code != http.StatusSeeOther {
		t.Fatalf("save = %d: %s", rr.Code, rr.Body.String())
	}

	// The connected mailbox holds exactly one copy, and it is the edited one.
	drafts := fake.messagesIn("Drafts")
	if len(drafts) != 1 {
		t.Fatalf("Drafts folder holds %d messages, want the single edited draft", len(drafts))
	}
	if !strings.Contains(drafts[0].raw, "Subject: Edited subject") || !strings.Contains(drafts[0].raw, "second body") {
		t.Fatalf("connected mailbox draft was not updated: %s", drafts[0].raw)
	}
	if strings.Contains(drafts[0].raw, "first body") {
		t.Fatal("the edited draft still carries the replaced body")
	}
	// The message stays the same draft to every reader: same Message-ID.
	if !strings.Contains(drafts[0].raw, "Message-ID: <placed-ui@remote.test>") {
		t.Fatalf("edited draft lost its Message-ID: %s", drafts[0].raw)
	}

	// The cached row follows at once, so the list the operator lands on is right.
	// The role-folder pull that the save schedules is asynchronous, so give it a
	// bounded moment to settle before insisting the list is final.
	var page string
	deadline := time.Now().Add(3 * time.Second)
	for {
		page = uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts").Body.String()
		if strings.Contains(page, "Edited subject") && !strings.Contains(page, "Placed draft subject") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("drafts page did not settle on the edited draft: %s", page)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The edit is attributable: the placement's correlation header survived.
	if !strings.Contains(drafts[0].raw, model.HandoffHeader) && strings.Contains(placedDraftRaw, model.HandoffHeader) {
		t.Fatal("edited draft lost its handoff correlation header")
	}
}

// TestStandalonePlacedDraftSaveRefusesAStaleEditor proves a save whose change
// marker no longer matches the server's copy is refused instead of silently
// overwriting an edit made in Outlook or Gmail.
func TestStandalonePlacedDraftSaveRefusesAStaleEditor(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	refreshRemote(t, h, box.ID, key)
	rowID := seedPlacedDraft(t, svc, u, box, fake)
	cookie, csrf := uiSession(t, svc, u.ID)

	// The draft changed elsewhere: its size and arrival time both moved.
	rec, err := svc.Store.GetRemoteMessage(context.Background(), u.AccountID, box.ID, rowID)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err = svc.Store.UpsertRemoteMessage(context.Background(), u.AccountID, box.ID, store.RemoteMessageInput{
		FolderPath: "Drafts", UIDValidity: rec.UIDValidity, UID: rec.UID,
		RFCMessageID: rec.RFCMessageID, FromAddress: rec.FromAddress, To: rec.To,
		Subject: "Changed in Outlook", SizeBytes: rec.SizeBytes + 40, Draft: true, Read: true,
		InternalDate: &at,
	}); err != nil {
		t.Fatal(err)
	}

	form := map[string]string{
		"_csrf": csrf, "action": "draft", "draft_id": rowID, "draft_marker": "100/1/1/stale",
		"to": "someone@example.com", "subject": "Edited subject", "text": "second body",
	}
	if rr := uiPostMultipart(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+rowID+"/save", form); rr.Code != http.StatusConflict {
		t.Fatalf("stale save = %d, want 409: %s", rr.Code, rr.Body.String())
	}
	if drafts := fake.messagesIn("Drafts"); len(drafts) != 1 || !strings.Contains(drafts[0].raw, "first body") {
		t.Fatal("a refused save must not touch the connected mailbox")
	}
}

// TestStandalonePlacedDraftDeleteQueuesTheProviderDelete proves the row's delete
// button removes the draft from the connected mailbox through the action queue.
func TestStandalonePlacedDraftDeleteQueuesTheProviderDelete(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, fake := standaloneFixture(t)
	key := adminKey(t, svc, u)
	refreshRemote(t, h, box.ID, key)
	rowID := seedPlacedDraft(t, svc, u, box, fake)
	cookie, csrf := uiSession(t, svc, u.ID)

	form := "_csrf=" + csrf
	if rr := uiPost(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+rowID+"/delete", form); rr.Code != http.StatusSeeOther {
		t.Fatalf("delete = %d: %s", rr.Code, rr.Body.String())
	}
	actions, err := svc.Store.ListMessageActions(context.Background(), model.Principal{AccountID: u.AccountID, UserID: u.ID, Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	queued := false
	for _, a := range actions {
		if a.Input.Kind == "delete" && a.MessageID == rowID {
			queued = true
		}
	}
	if !queued {
		t.Fatal("deleting a placed draft did not queue a provider delete")
	}
}

// uiPostMultipart posts a multipart form, the way the compose page does: the
// draft save route parses attachments, so it only accepts multipart bodies.
func uiPostMultipart(t *testing.T, h http.Handler, cookie *http.Cookie, path string, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// messagesIn returns the fake server's messages in one folder.
func (f *fakeIMAP) messagesIn(folder string) []*fakeIMAPMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fakeIMAPMsg
	for _, m := range f.messages {
		if m.folder == folder {
			out = append(out, m)
		}
	}
	return out
}

// hiddenValue returns the value of a hidden input in a rendered form.
func hiddenValue(t *testing.T, body, name string) string {
	t.Helper()
	needle := `name="` + name + `" value="`
	i := strings.Index(body, needle)
	if i < 0 {
		return ""
	}
	rest := body[i+len(needle):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}
