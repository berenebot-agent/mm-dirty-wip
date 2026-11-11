package httpapp_test

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

// seedRawDraft places an arbitrary raw draft in the fake Drafts folder and
// indexes it the way a folder pull would, returning the cached row's id.
func seedRawDraft(t *testing.T, svc *app.Service, u model.User, box model.Inbox, fake *fakeIMAP, raw, messageID string) string {
	t.Helper()
	fake.add("Drafts", raw, messageID, "Seeded")
	fake.mu.Lock()
	uid := fake.messages[len(fake.messages)-1].uid
	fake.mu.Unlock()
	row, err := svc.Store.UpsertRemoteMessage(context.Background(), u.AccountID, box.ID, store.RemoteMessageInput{
		FolderPath: "Drafts", UIDValidity: 100, UID: uid, RFCMessageID: messageID,
		FromName: "Agent", FromAddress: "agent@remote.test", To: []string{"someone@example.com"},
		Subject: "Seeded", SizeBytes: int64(len(raw)), Read: true, Draft: true,
		Flags: []string{"\\Draft", "\\Seen"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return row.ID
}

const bccDraftRaw = "From: Agent <agent@remote.test>\r\n" +
	"To: someone@example.com\r\n" +
	"Bcc: hidden@example.com\r\n" +
	"Subject: Has bcc\r\n" +
	"Message-ID: <bcc@remote.test>\r\n" +
	"Date: Mon, 13 Oct 2026 09:10:00 +0000\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\nbcc body\r\n"

const htmlDraftRaw = "From: Agent <agent@remote.test>\r\n" +
	"To: someone@example.com\r\n" +
	"Subject: Has html\r\n" +
	"Message-ID: <html@remote.test>\r\n" +
	"Date: Mon, 13 Oct 2026 09:10:00 +0000\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/alternative; boundary=\"b1\"\r\n" +
	"\r\n--b1\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nold text\r\n" +
	"--b1\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>old html body</p>\r\n--b1--\r\n"

func saveForm(csrf, rowID, marker string, extra map[string]string) map[string]string {
	f := map[string]string{
		"_csrf": csrf, "action": "draft", "draft_id": rowID, "draft_marker": marker,
		"to": "someone@example.com", "subject": "Edited", "text": "new text",
	}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

// TestPlacedDraftSaveRefusesAChangeTheCacheHasNotSeen proves the concurrent-edit
// check reads the provider's live copy. The draft is edited on the server while
// the cached index still describes the old copy, which is exactly when an
// operator's edit in their own client would otherwise be overwritten.
func TestPlacedDraftSaveRefusesAChangeTheCacheHasNotSeen(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, fake := standaloneFixture(t)
	refreshRemote(t, h, box.ID, adminKey(t, svc, u))
	rowID := seedPlacedDraft(t, svc, u, box, fake)
	cookie, csrf := uiSession(t, svc, u.ID)

	marker := hiddenValue(t, uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+rowID+"/edit").Body.String(), "draft_marker")
	if marker == "" {
		t.Fatal("editor carried no marker")
	}
	// Edited elsewhere; the cached row is untouched.
	msg := fake.messagesIn("Drafts")[0]
	fake.mu.Lock()
	msg.raw = strings.Replace(msg.raw, "first body", "changed in Outlook", 1)
	fake.mu.Unlock()

	rr := uiPostMultipart(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+rowID+"/save", saveForm(csrf, rowID, marker, nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("save over a server-side change = %d, want 409: %s", rr.Code, rr.Body.String())
	}
	if d := fake.messagesIn("Drafts"); len(d) != 1 || !strings.Contains(d[0].raw, "changed in Outlook") {
		t.Fatal("a refused save must leave the other client's edit in place")
	}
}

// TestPlacedDraftSaveRequiresAMarker proves an empty marker cannot bypass the
// concurrent-edit check.
func TestPlacedDraftSaveRequiresAMarker(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, fake := standaloneFixture(t)
	refreshRemote(t, h, box.ID, adminKey(t, svc, u))
	rowID := seedPlacedDraft(t, svc, u, box, fake)
	cookie, csrf := uiSession(t, svc, u.ID)

	rr := uiPostMultipart(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+rowID+"/save", saveForm(csrf, rowID, "", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("markerless save = %d, want 409: %s", rr.Code, rr.Body.String())
	}
	if d := fake.messagesIn("Drafts"); len(d) != 1 || !strings.Contains(d[0].raw, "first body") {
		t.Fatal("a markerless save must not touch the connected mailbox")
	}
}

// TestPlacedDraftReadOnlyUserCannotEdit proves a Read-scoped user can neither
// open the editor nor save, and the draft is untouched.
func TestPlacedDraftReadOnlyUserCannotEdit(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, fake := standaloneFixture(t)
	refreshRemote(t, h, box.ID, adminKey(t, svc, u))
	rowID := seedPlacedDraft(t, svc, u, box, fake)

	_, key, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "reader", false, map[string]string{box.ID: "read"})
	if err != nil {
		t.Fatal(err)
	}
	session := keyLogin(t, h, key)
	if session == nil {
		t.Fatal("read key login failed")
	}
	if rr := uiGet(t, h, session, "/ui/inboxes/"+box.ID+"/drafts/"+rowID+"/edit"); rr.Code != http.StatusForbidden {
		t.Fatalf("read-only editor = %d, want 403", rr.Code)
	}
	// Even with the right marker (taken by an owner) the save is refused.
	ownerCookie, _ := uiSession(t, svc, u.ID)
	marker := hiddenValue(t, uiGet(t, h, ownerCookie, "/ui/inboxes/"+box.ID+"/drafts/"+rowID+"/edit").Body.String(), "draft_marker")
	rr := uiPostMultipart(t, h, session, "/ui/inboxes/"+box.ID+"/drafts/"+rowID+"/save", saveForm(csrfFromInbox(t, h, session, box.ID), rowID, marker, nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("read-only save = %d, want 403: %s", rr.Code, rr.Body.String())
	}
	if d := fake.messagesIn("Drafts"); len(d) != 1 || !strings.Contains(d[0].raw, "first body") {
		t.Fatal("a read-only save must not touch the connected mailbox")
	}
}

// TestPlacedDraftKeepsBcc proves a draft's Bcc recipients are shown in the
// editor and survive a save, rather than being saved as an empty list.
func TestPlacedDraftKeepsBcc(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, fake := standaloneFixture(t)
	refreshRemote(t, h, box.ID, adminKey(t, svc, u))
	rowID := seedRawDraft(t, svc, u, box, fake, bccDraftRaw, "<bcc@remote.test>")
	cookie, csrf := uiSession(t, svc, u.ID)

	body := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+rowID+"/edit").Body.String()
	if !strings.Contains(body, `name="bcc" value="hidden@example.com"`) {
		t.Fatalf("editor did not show the draft's Bcc: %s", body)
	}
	// Submit the form the way the browser would, Bcc field included.
	form := saveForm(csrf, rowID, hiddenValue(t, body, "draft_marker"), map[string]string{"bcc": "hidden@example.com"})
	if rr := uiPostMultipart(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+rowID+"/save", form); rr.Code != http.StatusSeeOther {
		t.Fatalf("save = %d: %s", rr.Code, rr.Body.String())
	}
	d := fake.messagesIn("Drafts")
	if len(d) != 1 || !strings.Contains(d[0].raw, "Bcc: hidden@example.com") {
		t.Fatalf("saved draft lost its Bcc: %v", d)
	}
}

// TestPlacedDraftHTMLFollowsTheText proves the HTML alternative never outlives
// the text it was written for: an edited body drops it, an untouched body keeps it.
func TestPlacedDraftHTMLFollowsTheText(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, fake := standaloneFixture(t)
	refreshRemote(t, h, box.ID, adminKey(t, svc, u))
	rowID := seedRawDraft(t, svc, u, box, fake, htmlDraftRaw, "<html@remote.test>")
	cookie, csrf := uiSession(t, svc, u.ID)
	path := "/ui/inboxes/" + box.ID + "/drafts/" + rowID

	marker := hiddenValue(t, uiGet(t, h, cookie, path+"/edit").Body.String(), "draft_marker")
	// A subject-only edit leaves the text alone: the HTML stays.
	form := saveForm(csrf, rowID, marker, map[string]string{"text": "old text", "subject": "Subject only"})
	if rr := uiPostMultipart(t, h, cookie, path+"/save", form); rr.Code != http.StatusSeeOther {
		t.Fatalf("subject-only save = %d: %s", rr.Code, rr.Body.String())
	}
	d := fake.messagesIn("Drafts")
	if len(d) != 1 || !strings.Contains(d[0].raw, "old html body") {
		t.Fatalf("an untouched body lost its HTML: %v", d)
	}

	// Editing the text must not leave the old HTML behind.
	rowID = d[0].messageIDRow(t, svc, u, box)
	marker = hiddenValue(t, uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+rowID+"/edit").Body.String(), "draft_marker")
	form = saveForm(csrf, rowID, marker, map[string]string{"text": "brand new text"})
	if rr := uiPostMultipart(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+rowID+"/save", form); rr.Code != http.StatusSeeOther {
		t.Fatalf("text save = %d: %s", rr.Code, rr.Body.String())
	}
	d = fake.messagesIn("Drafts")
	if len(d) != 1 || !strings.Contains(d[0].raw, "brand new text") || strings.Contains(d[0].raw, "old html body") {
		t.Fatalf("edited body still carries the old HTML: %v", d)
	}
}

// messageIDRow finds the cached row now holding this message (an IMAP save
// appends a new copy, so the row id moves).
func (m *fakeIMAPMsg) messageIDRow(t *testing.T, svc *app.Service, u model.User, box model.Inbox) string {
	t.Helper()
	rows, err := svc.Store.ListRemoteMessages(context.Background(), u.AccountID, box.ID, "Drafts")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.UID == m.uid {
			return r.ID
		}
	}
	t.Fatalf("no cached row for uid %d", m.uid)
	return ""
}

// TestPlacedDraftEditorOffersNoUploads proves the editor does not show file
// controls the save path cannot honour, and that a posted file is refused
// instead of being silently dropped.
func TestPlacedDraftEditorOffersNoUploads(t *testing.T) {
	t.Parallel()
	svc, h, u, _, box, fake := standaloneFixture(t)
	refreshRemote(t, h, box.ID, adminKey(t, svc, u))
	rowID := seedPlacedDraft(t, svc, u, box, fake)
	cookie, csrf := uiSession(t, svc, u.ID)

	body := uiGet(t, h, cookie, "/ui/inboxes/"+box.ID+"/drafts/"+rowID+"/edit").Body.String()
	if strings.Contains(body, `type="file"`) || strings.Contains(body, "attach-drop") {
		t.Fatalf("placed draft editor still offers uploads: %s", body)
	}
	marker := hiddenValue(t, body, "draft_marker")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range saveForm(csrf, rowID, marker, nil) {
		_ = mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("attachments", "note.txt")
	_, _ = fw.Write([]byte("hello"))
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/drafts/"+rowID+"/save", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("save with an upload = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	if d := fake.messagesIn("Drafts"); len(d) != 1 || !strings.Contains(d[0].raw, "first body") {
		t.Fatal("a refused upload must not change the draft")
	}
}
