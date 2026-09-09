package httpapp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/mailparse"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

func uiSession(t *testing.T, svc *app.Service, userID string) (*http.Cookie, string) {
	t.Helper()
	tok, csrf, err := svc.Store.CreateSession(context.Background(), userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: "ghm_session", Value: tok}, csrf
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

func setActiveBrevo(t *testing.T, svc *app.Service, accountID string) {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"messageId":"<mock-out>"}`)
	}))
	t.Cleanup(api.Close)
	cred, err := svc.SaveOutboundCredential(context.Background(), accountID, "", "Brevo", "brevo", map[string]any{"api_key": "k", "api_base": api.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Store.SetActiveOutboundCredential(context.Background(), accountID, cred.ID); err != nil {
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
	svc, h, u, _, box := httpFixture(t)
	seedInbound(t, svc, box, "d1", "<m1@test>", "Subject", "body")
	// Two drafts.
	for i := 0; i < 2; i++ {
		if _, err := svc.Store.CreateDraft(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}, model.Draft{InboxID: box.ID, To: []string{"a@example.net"}, Subject: fmt.Sprintf("Draft %d", i), Text: "draft"}); err != nil {
			t.Fatal(err)
		}
	}
	// One pending outbound message.
	setActiveBrevo(t, svc, u.AccountID)
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
	if !strings.Contains(body, "Drafts (2)") {
		t.Fatalf("inbox view missing drafts badge: %s", body)
	}
	if !strings.Contains(body, "Outbox (1)") {
		t.Fatalf("inbox view missing outbox badge: %s", body)
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
	if !strings.Contains(rr.Body.String(), "Drafts (2)") {
		t.Fatalf("drafts view missing drafts badge: %s", rr.Body.String())
	}

	// Outbox view shows the outbox badge.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/ui/inboxes/"+box.ID+"/outbox", nil)
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("outbox view %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Outbox (1)") {
		t.Fatalf("outbox view missing outbox badge: %s", rr.Body.String())
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
	if strings.Contains(body, "Drafts (") || strings.Contains(body, "Outbox (") {
		t.Fatalf("empty inbox should not show count badges: %s", body)
	}
}

func TestUIInboxPagination(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	for i := 0; i < inboxPageSize+1; i++ {
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
	if strings.Count(rr.Body.String(), `class="mailrow"`)+strings.Count(rr.Body.String(), `class="mailrow unread"`) != inboxPageSize {
		t.Fatalf("expected %d messages on first page", inboxPageSize)
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

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ui/messages/"+m.ID+"/delete", strings.NewReader("_csrf="+csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	h.ServeHTTP(rr, req)
	if rr.Code != 303 {
		t.Fatalf("delete %d: %s", rr.Code, rr.Body.String())
	}
	if _, err := svc.Store.GetMessageByID(context.Background(), u.AccountID, m.ID); err == nil {
		t.Fatal("message should be deleted")
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
		HTML:    `<p>Hello <img src="cid:logo@test"></p>`,
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
	if !strings.Contains(body, `sandbox="allow-popups allow-popups-to-escape-sandbox"`) {
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
	if csp := rr.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "img-src https:") || strings.Contains(csp, "script-src") {
		t.Fatalf("html csp %q", csp)
	}
	if !strings.Contains(rr.Body.String(), `<base target="_blank">`) {
		t.Fatal("html should inject a base target")
	}
}

func TestUIComposeCSRFAndSend(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	setActiveBrevo(t, svc, u.AccountID)
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

func TestUIReplyAndForward(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	setActiveBrevo(t, svc, u.AccountID)
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

func TestRewriteCIDsOnlyRewritesImages(t *testing.T) {
	atts := []model.Attachment{
		{ID: "att_1", ContentID: "<logo@test>", ContentType: "image/png; name=logo"},
		{ID: "att_2", ContentID: "<page@test>", ContentType: "text/html"},
	}
	body := `<img src="cid:logo@test"><img src="cid:<page@test>">`
	got := rewriteCIDs(body, atts)
	if !strings.Contains(got, "/ui/attachments/att_1/inline") {
		t.Fatalf("image cid not rewritten: %q", got)
	}
	if strings.Contains(got, "att_2") {
		t.Fatalf("non-image cid must not be rewritten: %q", got)
	}
	if !strings.Contains(got, "cid:<page@test>") {
		t.Fatalf("non-image cid should be left untouched: %q", got)
	}
	if !isInlineImage(normalizeContentType("image/jpeg")) || isInlineImage(normalizeContentType("text/html")) {
		t.Fatal("inline image allowlist is wrong")
	}
}
