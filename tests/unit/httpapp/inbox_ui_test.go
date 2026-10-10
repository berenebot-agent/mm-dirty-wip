package httpapp_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/mailparse"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func uiSession(t *testing.T, svc *app.Service, userID string) (*http.Cookie, string) {
	t.Helper()
	tok, csrf, err := svc.Store.CreateSession(context.Background(), userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: "mmm_session", Value: tok}, csrf
}

func seedInbound(t *testing.T, svc *app.Service, box model.Inbox, delivery, rfc, subject, body string) model.Message {
	t.Helper()
	m, _, _, err := svc.Store.CommitInbound(context.Background(), store.InboundRecord{
		Inbox: box, Provider: "mailgun", ProviderDeliveryID: delivery, RFCMessageID: rfc,
		From: model.Address{Name: "Sender", Address: "sender@outside.test"}, To: []string{box.Address},
		EnvelopeTo: []string{box.Address}, Subject: subject, Text: body,
		RawPath: "messages/test.eml", SizeBytes: int64(len(body)), ReceivedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func setDomainBrevo(t *testing.T, svc *app.Service, accountID, domainID string) {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"messageId":"<mock-out>"}`)
	}))
	t.Cleanup(api.Close)
	if _, err := svc.SaveDomainSendingConfig(context.Background(), accountID, domainID, "brevo", map[string]any{"api_key": "k", "api_base": api.URL}); err != nil {
		t.Fatal(err)
	}
}

func multipartBody(t *testing.T, fields map[string]string, filename, content string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if filename != "" {
		fw, err := mw.CreateFormFile("attachments", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = io.WriteString(fw, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

func TestUIInboxViewListsMessages(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	seedInbound(t, svc, box, "d1", "<m1@test>", "First subject", "one")
	seedInbound(t, svc, box, "d2", "<m2@test>", "Second subject", "two")
	cookie, _ := uiSession(t, svc, u.ID)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ui/inboxes/"+box.ID, nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("inbox view %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"First subject", "Second subject", box.Address, "Compose"} {
		if !strings.Contains(body, want) {
			t.Fatalf("inbox view missing %q", want)
		}
	}
}

func TestUIDraftsOutboxCountsAndDate(t *testing.T) {
	svc, h, u, dom, box := httpFixture(t)
	seedInbound(t, svc, box, "d1", "<m1@test>", "Subject", "body")
	// Two drafts.
	for i := 0; i < 2; i++ {
		if _, err := svc.Store.CreateDraft(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}, model.Draft{InboxID: box.ID, To: []string{"a@example.net"}, Subject: fmt.Sprintf("Draft %d", i), Text: "draft"}); err != nil {
			t.Fatal(err)
		}
	}
	// One pending outbound message.
	setDomainBrevo(t, svc, u.AccountID, dom.ID)
	if _, err := svc.Send(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}, app.SendInput{InboxID: box.ID, To: []string{"b@example.net"}, Subject: "Queued", Text: "hi"}, ""); err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)

	// Inbox view shows both badges and the new date format.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ui/inboxes/"+box.ID, nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("inbox view %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !regexp.MustCompile(`data-folder="drafts"[^>]*>Drafts\s*<span class="count" data-count="drafts">2</span>`).MatchString(body) {
		t.Fatalf("inbox view missing drafts count: %s", body)
	}
	if !regexp.MustCompile(`data-folder="outbox"[^>]*>Outbox\s*<span class="count" data-count="outbox">1</span>`).MatchString(body) {
		t.Fatalf("inbox view missing outbox count: %s", body)
	}
	if !regexp.MustCompile(`\d{2}:\d{2} \d{1,2}-[A-Z][a-z]{2}-\d{2}`).MatchString(body) {
		t.Fatalf("inbox view missing new date format: %s", body)
	}

	// Drafts view shows the drafts badge.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/ui/inboxes/"+box.ID+"/drafts", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("drafts view %d: %s", rr.Code, rr.Body.String())
	}
	if !regexp.MustCompile(`data-folder="drafts"[^>]*>Drafts\s*<span class="count" data-count="drafts">2</span>`).MatchString(rr.Body.String()) {
		t.Fatalf("drafts view missing drafts count: %s", rr.Body.String())
	}

	// Outbox view shows the outbox badge.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/ui/inboxes/"+box.ID+"/outbox", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("outbox view %d: %s", rr.Code, rr.Body.String())
	}
	if !regexp.MustCompile(`data-folder="outbox"[^>]*>Outbox\s*<span class="count" data-count="outbox">1</span>`).MatchString(rr.Body.String()) {
		t.Fatalf("outbox view missing outbox count: %s", rr.Body.String())
	}
}

func TestUIDraftsOutboxCountsEmpty(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ui/inboxes/"+box.ID, nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("inbox view %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if regexp.MustCompile(`Drafts\s*<span class="count"`).MatchString(body) || regexp.MustCompile(`Outbox\s*<span class="count"`).MatchString(body) {
		t.Fatalf("empty inbox should not show draft/outbox counts: %s", body)
	}
}

// testInboxPageSize mirrors internal/httpapp's unexported inboxPageSize.
const testInboxPageSize = 50

func TestUIInboxPagination(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	for i := 0; i < testInboxPageSize+1; i++ {
		seedInbound(t, svc, box, fmt.Sprintf("d%d", i), fmt.Sprintf("<m%d@test>", i), fmt.Sprintf("Subject %02d", i), "body")
	}
	cookie, _ := uiSession(t, svc, u.ID)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ui/inboxes/"+box.ID, nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("page 1 %d", rr.Code)
	}
	if strings.Count(rr.Body.String(), `class="mailrow"`)+strings.Count(rr.Body.String(), `class="mailrow unread"`) != testInboxPageSize {
		t.Fatalf("expected %d messages on first page", testInboxPageSize)
	}
	if !strings.Contains(rr.Body.String(), "Load older") {
		t.Fatal("first page should offer Load older")
	}
	before := rr.Body.String()
	idx := strings.Index(before, "?before=")
	start := idx + len("?before=")
	end := strings.Index(before[start:], `"`)
	cursor := before[start : start+end]
	if cursor == "" {
		t.Fatal("missing cursor")
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/ui/inboxes/"+box.ID+"?before="+cursor, nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("page 2 %d", rr.Code)
	}
	if strings.Count(rr.Body.String(), `class="mailrow"`)+strings.Count(rr.Body.String(), `class="mailrow unread"`) != 1 {
		t.Fatalf("expected 1 message on second page")
	}
	if strings.Contains(rr.Body.String(), "Load older") {
		t.Fatal("second page should not offer Load older")
	}
}

func TestUIMessageAutoMarksRead(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	m := seedInbound(t, svc, box, "d1", "<m1@test>", "Unread message", "hello")
	cookie, _ := uiSession(t, svc, u.ID)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ui/messages/"+m.ID, nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("message view %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Unread message") {
		t.Fatal("message body missing")
	}
	got, err := svc.Store.GetMessageByID(context.Background(), u.AccountID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Read {
		t.Fatal("message should be marked read on open")
	}
}

func TestUIReadToggle(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	m := seedInbound(t, svc, box, "d1", "<m1@test>", "Toggle", "hello")
	cookie, csrf := uiSession(t, svc, u.ID)

	form := "read=1&_csrf=" + csrf
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ui/messages/"+m.ID+"/read", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("read toggle %d: %s", rr.Code, rr.Body.String())
	}
	got, _ := svc.Store.GetMessageByID(context.Background(), u.AccountID, m.ID)
	if !got.Read {
		t.Fatal("message should be read")
	}
}

func TestUIReadToggleRequiresCSRF(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	m := seedInbound(t, svc, box, "d1", "<m1@test>", "Toggle", "hello")
	cookie, _ := uiSession(t, svc, u.ID)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ui/messages/"+m.ID+"/read", strings.NewReader("read=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("missing csrf should be 403, got %d", rr.Code)
	}
}

func TestUIDeleteMessage(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	m := seedInbound(t, svc, box, "d1", "<m1@test>", "Delete me", "bye")
	cookie, csrf := uiSession(t, svc, u.ID)

	post := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("POST", path, strings.NewReader("_csrf="+csrf))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		h.ServeHTTP(rr, req)
		return rr
	}

	// Delete moves the message to Trash; it is retained, not erased.
	if rr := post("/ui/messages/" + m.ID + "/delete"); rr.Code != 303 {
		t.Fatalf("delete %d: %s", rr.Code, rr.Body.String())
	}
	got, err := svc.Store.GetMessageByID(context.Background(), u.AccountID, m.ID)
	if err != nil {
		t.Fatalf("trashed message should still exist: %v", err)
	}
	if got.DeletedAt == nil {
		t.Fatal("message should be trashed")
	}
	// It is hidden from the ordinary inbox list.
	list, err := svc.Store.ListMessages(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}, store.MessageFilter{InboxID: box.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, lm := range list {
		if lm.ID == m.ID {
			t.Fatal("trashed message should not appear in the inbox list")
		}
	}

	// Restore returns it to the mailbox.
	if rr := post("/ui/messages/" + m.ID + "/restore"); rr.Code != 303 {
		t.Fatalf("restore %d: %s", rr.Code, rr.Body.String())
	}
	if got, err = svc.Store.GetMessageByID(context.Background(), u.AccountID, m.ID); err != nil || got.DeletedAt != nil {
		t.Fatalf("restore should clear trashed state: %v %+v", err, got)
	}

	// Trash then purge erases it permanently.
	if rr := post("/ui/messages/" + m.ID + "/delete"); rr.Code != 303 {
		t.Fatalf("re-delete %d: %s", rr.Code, rr.Body.String())
	}
	if rr := post("/ui/messages/" + m.ID + "/purge"); rr.Code != 303 {
		t.Fatalf("purge %d: %s", rr.Code, rr.Body.String())
	}
	if _, err := svc.Store.GetMessageByID(context.Background(), u.AccountID, m.ID); err == nil {
		t.Fatal("purged message should be gone")
	}
}

func TestUITrashFolderAndEmpty(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	a := seedInbound(t, svc, box, "ui-t1", "<ui-t1@test>", "one", "one")
	b := seedInbound(t, svc, box, "ui-t2", "<ui-t2@test>", "two", "two")
	keep := seedInbound(t, svc, box, "ui-t3", "<ui-t3@test>", "keep", "keep")
	cookie, csrf := uiSession(t, svc, u.ID)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	for _, id := range []string{a.ID, b.ID} {
		if _, _, err := svc.Store.TrashMessage(ctx, p, id); err != nil {
			t.Fatal(err)
		}
	}

	// The Trash folder lists the trashed messages and hides live ones.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ui/inboxes/"+box.ID+"/trash", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("trash view %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, a.ID) || !strings.Contains(body, b.ID) {
		t.Fatalf("trash view missing messages: %s", body)
	}
	if strings.Contains(body, keep.ID) {
		t.Fatalf("trash view leaked live message")
	}

	// Empty trash purges them and returns to the Trash view.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/trash/empty", strings.NewReader("_csrf="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("empty trash %d: %s", rr.Code, rr.Body.String())
	}
	if _, err := svc.Store.GetMessageByID(ctx, u.AccountID, a.ID); err == nil {
		t.Fatal("trashed message not purged")
	}
	if _, err := svc.Store.GetMessageByID(ctx, u.AccountID, keep.ID); err != nil {
		t.Fatalf("live message removed: %v", err)
	}
}

func TestUIAttachmentDownloadAndInline(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	raw, err := mailparse.BuildMessage(
		mailparse.Address{Address: "alice@outside.test"}, []string{box.Address}, nil, nil,
		"With attachment", "see attached", "", "<a1@outside.test>", "", nil, time.Now().UTC(),
		[]mailparse.Attachment{{Filename: "note.png", ContentType: "image/png", Content: []byte("attached bytes")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join("messages", "att.eml")
	full := filepath.Join(svc.Config.DataDir, rel)
	if err = writeFile(full, raw); err != nil {
		t.Fatal(err)
	}
	m, _, _, err := svc.Store.CommitInbound(context.Background(), store.InboundRecord{
		Inbox: box, Provider: "mailgun", ProviderDeliveryID: "att-1", RFCMessageID: "<a1@outside.test>",
		From: model.Address{Address: "alice@outside.test"}, To: []string{box.Address},
		EnvelopeTo: []string{box.Address}, Subject: "With attachment", Text: "see attached",
		RawPath: rel, SizeBytes: int64(len(raw)), ReceivedAt: time.Now().UTC(),
		Attachments: []store.AttachmentInput{{Filename: "note.png", ContentType: "image/png", Size: int64(len("attached bytes")), PartIndex: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	atts, err := svc.Store.ListAttachments(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}, m.ID)
	if err != nil || len(atts) != 1 {
		t.Fatalf("attachments %v %#v", err, atts)
	}
	cookie, _ := uiSession(t, svc, u.ID)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ui/attachments/"+atts[0].ID, nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("download %d %q", rr.Code, rr.Header().Get("Content-Disposition"))
	}
	if rr.Body.String() != "attached bytes" {
		t.Fatalf("download bytes %q", rr.Body.String())
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/ui/attachments/"+atts[0].ID+"/inline", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Header().Get("Content-Disposition"), "inline") {
		t.Fatalf("inline %d %q", rr.Code, rr.Header().Get("Content-Disposition"))
	}
	if !strings.Contains(rr.Header().Get("Content-Type"), "image/png") {
		t.Fatalf("inline content type %q", rr.Header().Get("Content-Type"))
	}
	if csp := rr.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Fatalf("inline csp %q", csp)
	}
}

func TestUIMessageHTMLIsFramableAndSandboxed(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	m, _, _, err := svc.Store.CommitInbound(context.Background(), store.InboundRecord{
		Inbox: box, Provider: "mailgun", ProviderDeliveryID: "html-1", RFCMessageID: "<html@test>",
		From: model.Address{Address: "sender@outside.test"}, To: []string{box.Address},
		EnvelopeTo: []string{box.Address}, Subject: "HTML", Text: "plain",
		HTML:    `<p>Hello <img src="cid:logo@test"><img src="https://tracker.example/pixel.png"></p>`,
		RawPath: "messages/test.eml", SizeBytes: 10, ReceivedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie, _ := uiSession(t, svc, u.ID)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ui/messages/"+m.ID, nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, `sandbox="allow-same-origin allow-popups allow-popups-to-escape-sandbox"`) {
		t.Fatal("message view should sandbox HTML iframe")
	}
	if !strings.Contains(body, "/ui/messages/"+m.ID+"/html") {
		t.Fatal("message view should reference the html route")
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/ui/messages/"+m.ID+"/html", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("html route %d", rr.Code)
	}
	if rr.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Fatalf("html frame options %q", rr.Header().Get("X-Frame-Options"))
	}
	// Remote images are blocked by default so opening mail cannot track the
	// reader; the banner offers an explicit opt-in.
	if csp := rr.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self' data:") || strings.Contains(csp, "script-src") {
		t.Fatalf("default html csp %q", csp)
	}
	if strings.Contains(rr.Body.String(), `<base target="_blank">`) {
		t.Fatal("html should not inject a base target")
	}
	if strings.Contains(rr.Body.String(), "tracker.example") {
		t.Fatal("remote image should be stripped by default")
	}
	if !strings.Contains(body, "data-remote-img-show") {
		t.Fatal("message view should offer a show-images control when remote images exist")
	}

	// Opting in relaxes the CSP and keeps the remote image.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/ui/messages/"+m.ID+"/html?remote=1", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if csp := rr.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self' https: http: data:") {
		t.Fatalf("opt-in html csp %q", csp)
	}
	if !strings.Contains(rr.Body.String(), "tracker.example") {
		t.Fatal("remote image should load after opt-in")
	}
}

func TestUIComposeCSRFAndSend(t *testing.T) {
	svc, h, u, dom, box := httpFixture(t)
	setDomainBrevo(t, svc, u.AccountID, dom.ID)
	cookie, csrf := uiSession(t, svc, u.ID)

	// Missing CSRF (multipart body token is not parsed by ParseForm) must be rejected.
	body, ctype := multipartBody(t, map[string]string{"to": "friend@example.net", "subject": "No token", "text": "hi"}, "", "")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/send", body)
	req.Header.Set("Content-Type", ctype)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("compose without csrf = %d", rr.Code)
	}

	body, ctype = multipartBody(t, map[string]string{"to": "friend@example.net", "subject": "Composed", "text": "hi there"}, "upload.txt", "file contents")
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/send?_csrf="+csrf, body)
	req.Header.Set("Content-Type", ctype)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("compose send %d: %s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	if loc != "/ui/inboxes/"+box.ID+"/sent" {
		t.Fatalf("redirect %q", loc)
	}
	// The sent message was created.
	msgs, err := svc.Store.ListMessages(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}, store.MessageFilter{InboxID: box.ID, Direction: "outbound", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Subject != "Composed" {
		t.Fatalf("sent %#v", msgs)
	}
	sent := msgs[0]
	atts, _ := svc.Store.ListAttachments(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}, sent.ID)
	if len(atts) != 1 || atts[0].Filename != "upload.txt" {
		t.Fatalf("sent attachments %#v", atts)
	}
}

func TestUIComposeAttachmentDropUI(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	cookie, _ := uiSession(t, svc, u.ID)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ui/inboxes/"+box.ID+"/compose", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("compose get %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`id="attach-drop"`,
		`id="attach-overlay"`,
		`id="attach-list"`,
		`for="attachments"`,
		`name="attachments" multiple`,
		`enctype="multipart/form-data"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("compose page missing %q", want)
		}
	}
}

func TestUIReplyAndForward(t *testing.T) {
	svc, h, u, dom, box := httpFixture(t)
	setDomainBrevo(t, svc, u.AccountID, dom.ID)
	original := seedInbound(t, svc, box, "d1", "<orig@test>", "Original", "original body")
	cookie, csrf := uiSession(t, svc, u.ID)

	// Reply.
	body, ctype := multipartBody(t, map[string]string{"to": "sender@outside.test", "subject": "Re: Original", "text": "my reply"}, "", "")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ui/messages/"+original.ID+"/reply?_csrf="+csrf, body)
	req.Header.Set("Content-Type", ctype)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("reply %d: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/ui/inboxes/"+box.ID+"/sent" {
		t.Fatalf("reply redirect %q", loc)
	}
	threadMsgs, err := svc.Store.ListMessages(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}, store.MessageFilter{InboxID: box.ID, ThreadID: original.ThreadID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var reply model.Message
	for _, m := range threadMsgs {
		if m.Direction == "outbound" {
			reply = m
		}
	}
	if reply.ID == "" {
		t.Fatalf("no reply in thread %#v", threadMsgs)
	}
	if reply.ThreadID != original.ThreadID {
		t.Fatal("reply should stay in the same thread")
	}
	if reply.InReplyTo != original.RFCMessageID {
		t.Fatalf("reply in-reply-to %q", reply.InReplyTo)
	}

	// Forward carries the body quote and creates a new conversation.
	body, ctype = multipartBody(t, map[string]string{"to": "elsewhere@example.net", "subject": "Fwd: Original", "text": "fyi"}, "", "")
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/ui/messages/"+original.ID+"/forward?_csrf="+csrf, body)
	req.Header.Set("Content-Type", ctype)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("forward %d: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/ui/inboxes/"+box.ID+"/sent" {
		t.Fatalf("forward redirect %q", loc)
	}
	fwdMsgs, err := svc.Store.ListMessages(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}, store.MessageFilter{InboxID: box.ID, Direction: "outbound", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var fwd model.Message
	for _, m := range fwdMsgs {
		if m.Subject == "Fwd: Original" {
			fwd = m
		}
	}
	if fwd.ID == "" {
		t.Fatalf("no forward %#v", fwdMsgs)
	}
	if !strings.Contains(fwd.Text, "---------- Forwarded message ----------") || !strings.Contains(fwd.Text, "original body") {
		t.Fatalf("forward body %q", fwd.Text)
	}
	if fwd.ThreadID == original.ThreadID {
		t.Fatal("forward should start a new conversation")
	}
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func TestUIMessageHTMLRewritesOnlyImageCIDs(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	rel := filepath.Join("messages", "cid.eml")
	if err := writeFile(filepath.Join(svc.Config.DataDir, rel), []byte("From: alice@outside.test\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	m, _, _, err := svc.Store.CommitInbound(context.Background(), store.InboundRecord{
		Inbox: box, Provider: "mailgun", ProviderDeliveryID: "cid-1", RFCMessageID: "<cid@outside.test>",
		From: model.Address{Address: "alice@outside.test"}, To: []string{box.Address},
		EnvelopeTo: []string{box.Address}, Subject: "CIDs", Text: "see attached",
		HTML:    `<img src="cid:logo@test"><img src="cid:<page@test>">`,
		RawPath: rel, SizeBytes: 32, ReceivedAt: time.Now().UTC(),
		Attachments: []store.AttachmentInput{
			{Filename: "logo.png", ContentType: "image/png", ContentID: "<logo@test>", Size: 3, PartIndex: 1},
			{Filename: "page.html", ContentType: "text/html", ContentID: "<page@test>", Size: 12, PartIndex: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	atts, err := svc.Store.ListAttachments(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, a := range atts {
		byName[a.Filename] = a.ID
	}
	cookie, _ := uiSession(t, svc, u.ID)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ui/messages/"+m.ID+"/html", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("html %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "/ui/attachments/"+byName["logo.png"]+"/inline") {
		t.Fatalf("image cid not rewritten: %q", body)
	}
	if strings.Contains(body, byName["page.html"]) {
		t.Fatalf("non-image cid must not be rewritten: %q", body)
	}
}

func TestUIDraftSendDeletesDraft(t *testing.T) {
	svc, h, u, dom, box := httpFixture(t)
	setDomainBrevo(t, svc, u.AccountID, dom.ID)
	cookie, csrf := uiSession(t, svc, u.ID)
	ctx := context.Background()
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}

	// Create a draft with an attachment.
	d, err := svc.Store.CreateDraft(ctx, p, model.Draft{InboxID: box.ID, To: []string{"friend@example.net"}, Subject: "Draft", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	rawPath := filepath.Join(svc.Config.DataDir, "drafts", "test.bin")
	if err = writeFile(rawPath, []byte("draft attachment")); err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(svc.Config.DataDir, rawPath)
	if _, err = svc.Store.AddDraftAttachment(ctx, p, d.ID, model.DraftAttachment{Filename: "d.txt", ContentType: "text/plain", Size: 16, RawPath: filepath.ToSlash(rel)}); err != nil {
		t.Fatal(err)
	}

	// Send the draft via the UI form (action=send).
	body, ctype := multipartBody(t, map[string]string{
		"to":        "friend@example.net",
		"subject":   "Draft",
		"text":      "body",
		"action":    "send",
		"draft_id":  d.ID,
		"return_to": "/ui/inboxes/" + box.ID + "/drafts",
	}, "", "")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/drafts/"+d.ID+"/save?_csrf="+csrf, body)
	req.Header.Set("Content-Type", ctype)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("draft send %d: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/ui/inboxes/"+box.ID+"/drafts" {
		t.Fatalf("redirect %q", loc)
	}

	// The draft and its attachments must be gone.
	if _, err = svc.Store.GetDraft(ctx, p, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("draft still present after send: %v", err)
	}
	if _, err = svc.Store.ListDraftAttachments(ctx, p, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("draft attachments after send err=%v", err)
	}
	if _, err = os.Stat(rawPath); !os.IsNotExist(err) {
		t.Fatalf("attachment file still present: %v", err)
	}

	// The message was enqueued.
	msgs, err := svc.Store.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID, Direction: "outbound", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Subject != "Draft" {
		t.Fatalf("sent %#v", msgs)
	}
}

// TestUIDeleteInboxConfirmDialog locks in that the dashboard renders the
// type-to-confirm inbox delete modal, that every delete entry point carries the
// inbox address so the dialog can display it, and that the existing delete
// route still works.
func TestUIDeleteInboxConfirmDialog(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	cookie, csrf := uiSession(t, svc, u.ID)

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`id="inbox-delete-dialog"`,
		`id="inbox-delete-form"`,
		`id="inbox-delete-input"`,
		`id="inbox-delete-submit"`,
		`id="inbox-delete-address"`,
		`class="secondary icon-btn danger open-delete-inbox" data-id="` + box.ID + `" data-address="` + box.Address + `"`,
		`class="secondary danger open-delete-inbox" id="inbox-edit-delete"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, `/ui/inboxes/`+box.ID+`/delete" data-confirm=`) {
		t.Fatal("inbox delete should route through the confirm dialog, not data-confirm")
	}

	// The delete route is unchanged, but now requires the server-side typed
	// confirmation (the inbox address).
	post := httptest.NewRecorder()
	delReq := httptest.NewRequest("POST", "/ui/inboxes/"+box.ID+"/delete", strings.NewReader("_csrf="+csrf+"&confirm="+url.QueryEscape(box.Address)))
	delReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	delReq.AddCookie(cookie)
	h.ServeHTTP(post, delReq)
	if post.Code != http.StatusSeeOther {
		t.Fatalf("delete inbox %d: %s", post.Code, post.Body.String())
	}
	if _, err := svc.Store.GetInbox(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}, box.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("inbox still present after delete: %v", err)
	}
}
