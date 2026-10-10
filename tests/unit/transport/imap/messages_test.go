package imap_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	imapadapter "github.com/dellarb/mailmoose/internal/transport/imap"
)

const sampleMessage = "MIME-Version: 1.0\r\n" +
	"Message-ID: <handoff-123@example.com>\r\n" +
	"From: Alice <alice@example.com>\r\n" +
	"To: Bob <bob@example.com>\r\n" +
	"Subject: Hello there\r\n" +
	"Date: Mon, 02 Jan 2026 15:04:05 -0700\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"This is the body of the message.\r\n"

const multipartMessage = "MIME-Version: 1.0\r\n" +
	"Message-ID: <attach-456@example.com>\r\n" +
	"From: Alice <alice@example.com>\r\n" +
	"To: Bob <bob@example.com>\r\n" +
	"Subject: With attachment\r\n" +
	"Content-Type: multipart/mixed; boundary=\"BOUNDARY\"\r\n" +
	"\r\n" +
	"--BOUNDARY\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"See the attached file.\r\n" +
	"--BOUNDARY\r\n" +
	"Content-Type: application/octet-stream; name=\"data.bin\"\r\n" +
	"Content-Disposition: attachment; filename=\"data.bin\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"AAECAwQFBgc=\r\n" +
	"--BOUNDARY--\r\n"

func seedMessage(t *testing.T, adapter *imapadapter.Adapter, folder, raw string, flags []string) imapadapter.Locator {
	t.Helper()
	res, err := adapter.AppendBytes(testContext(t), folder, []byte(raw), flags, time.Time{})
	if err != nil {
		t.Fatalf("AppendBytes: %v", err)
	}
	if !res.Confirmed {
		t.Fatalf("seed append was not confirmed (no APPENDUID)")
	}
	return imapadapter.Locator{FolderPath: folder, UIDValidity: res.UIDValidity, UID: res.DestinationUID}
}

func TestListHeadersFetchesEnvelopeAndFlags(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	seedMessage(t, adapter, "INBOX", sampleMessage, nil)

	headers, uidValidity, err := adapter.ListHeaders(testContext(t), "INBOX", nil, 100)
	if err != nil {
		t.Fatalf("ListHeaders: %v", err)
	}
	if uidValidity == 0 {
		t.Error("expected a non-zero UIDVALIDITY")
	}
	if len(headers) != 1 {
		t.Fatalf("expected 1 header, got %d", len(headers))
	}
	h := headers[0]
	if h.Subject != "Hello there" {
		t.Errorf("subject = %q", h.Subject)
	}
	if h.MessageID != "handoff-123@example.com" {
		t.Errorf("message-id = %q", h.MessageID)
	}
	if h.From.Address != "alice@example.com" {
		t.Errorf("from = %q", h.From.Address)
	}
	if len(h.To) != 1 || h.To[0] != "bob@example.com" {
		t.Errorf("to = %v", h.To)
	}
	if h.Read {
		t.Error("message must not be seen after a header listing")
	}
}

func TestListHeadersDoesNotMarkSeen(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	loc := seedMessage(t, adapter, "INBOX", sampleMessage, nil)

	if _, _, err := adapter.ListHeaders(testContext(t), "INBOX", nil, 100); err != nil {
		t.Fatalf("ListHeaders: %v", err)
	}
	h, err := adapter.FetchHeader(testContext(t), loc)
	if err != nil {
		t.Fatalf("FetchHeader: %v", err)
	}
	if h.Read {
		t.Error("listing/fetching headers must not set \\Seen")
	}
}

func TestFetchHeaderStaleUIDValidity(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	loc := seedMessage(t, adapter, "INBOX", sampleMessage, nil)

	stale := loc
	stale.UIDValidity = loc.UIDValidity + 999
	_, err := adapter.FetchHeader(testContext(t), stale)
	if err == nil {
		t.Fatal("expected a conflict for stale UIDVALIDITY")
	}
	var mb *model.MailboxError
	if !errors.As(err, &mb) || mb.Kind != model.ErrKindConflict {
		t.Fatalf("error = %v, want kind conflict", err)
	}
}

func TestFetchHeaderMissingUIDIsNotFound(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	loc := seedMessage(t, adapter, "INBOX", sampleMessage, nil)
	loc.UID = 999999

	_, err := adapter.FetchHeader(testContext(t), loc)
	if err == nil {
		t.Fatal("expected not_found for a missing UID")
	}
	var mb *model.MailboxError
	if !errors.As(err, &mb) || mb.Kind != model.ErrKindNotFound {
		t.Fatalf("error = %v, want kind not_found", err)
	}
}

func TestFetchRawMIMEStreamsWholeMessage(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	loc := seedMessage(t, adapter, "INBOX", sampleMessage, nil)

	var buf bytes.Buffer
	if err := adapter.FetchRawMIME(testContext(t), loc, &buf); err != nil {
		t.Fatalf("FetchRawMIME: %v", err)
	}
	if !strings.Contains(buf.String(), "This is the body of the message.") {
		t.Errorf("raw MIME missing body: %q", buf.String())
	}
	// Fetching the body must not set \Seen because it uses BODY.PEEK.
	h, err := adapter.FetchHeader(testContext(t), loc)
	if err != nil {
		t.Fatalf("FetchHeader: %v", err)
	}
	if h.Read {
		t.Error("BODY.PEEK fetch must not set \\Seen")
	}
}

func TestFetchBodyPartStreamsAttachment(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	loc := seedMessage(t, adapter, "INBOX", multipartMessage, nil)

	// Part 2 is the attachment.
	var buf bytes.Buffer
	if err := adapter.FetchBodyPart(testContext(t), loc, []int{2}, &buf); err != nil {
		t.Fatalf("FetchBodyPart: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("expected attachment bytes")
	}
}

func TestBodyStructureDetectsAttachment(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	loc := seedMessage(t, adapter, "INBOX", multipartMessage, nil)

	h, err := adapter.FetchHeader(testContext(t), loc)
	if err != nil {
		t.Fatalf("FetchHeader: %v", err)
	}
	if !h.HasAttach {
		t.Error("expected HasAttach to be true for a multipart message")
	}
	if h.BodyStructure == nil {
		t.Fatal("expected a body structure")
	}
	if !strings.HasPrefix(h.BodyStructure.MediaType, "multipart/") {
		t.Errorf("media type = %q", h.BodyStructure.MediaType)
	}
}

func TestSearchByMessageID(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	seedMessage(t, adapter, "INBOX", sampleMessage, nil)
	seedMessage(t, adapter, "INBOX", multipartMessage, nil)

	res, err := adapter.Search(testContext(t), "INBOX", imapadapter.SearchQuery{MessageID: "handoff-123@example.com"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.UIDs) != 1 {
		t.Fatalf("expected exactly one match, got %d", len(res.UIDs))
	}
}

func TestSearchBySubjectAndUnseen(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	seedMessage(t, adapter, "INBOX", sampleMessage, nil)

	unseen := false
	res, err := adapter.Search(testContext(t), "INBOX", imapadapter.SearchQuery{
		Subject: "Hello", Seen: &unseen,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.UIDs) != 1 {
		t.Fatalf("expected one unseen match, got %d", len(res.UIDs))
	}
}

func TestSearchLimitReportsPartial(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	for i := 0; i < 5; i++ {
		seedMessage(t, adapter, "INBOX", sampleMessage, nil)
	}
	res, err := adapter.Search(testContext(t), "INBOX", imapadapter.SearchQuery{Limit: 3})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.UIDs) != 3 {
		t.Fatalf("expected 3 UIDs, got %d", len(res.UIDs))
	}
	if res.Completeness != imapadapter.CompletenessPartial {
		t.Errorf("completeness = %q, want partial", res.Completeness)
	}
	if res.NextCursor == 0 {
		t.Error("expected a next cursor")
	}
}

// TestSearchNewestFirstPagination proves a newest-first search returns the most
// recent matches and pages with an exact resume cursor, so a bounded search never
// silently truncates to the oldest rows.
func TestSearchNewestFirstPagination(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	for i := 0; i < 5; i++ {
		seedMessage(t, adapter, "INBOX", sampleMessage, nil)
	}
	first, err := adapter.Search(testContext(t), "INBOX", imapadapter.SearchQuery{Limit: 2, NewestFirst: true})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(first.UIDs) != 2 {
		t.Fatalf("page 1 = %v, want 2 newest", first.UIDs)
	}
	// Newest-first: descending UID.
	if first.UIDs[0] <= first.UIDs[1] {
		t.Fatalf("page 1 not newest-first: %v", first.UIDs)
	}
	if first.NextCursor == 0 {
		t.Fatal("expected a resume cursor")
	}
	second, err := adapter.Search(testContext(t), "INBOX", imapadapter.SearchQuery{Limit: 2, NewestFirst: true, BeforeUID: first.NextCursor})
	if err != nil {
		t.Fatalf("Search page 2: %v", err)
	}
	if len(second.UIDs) != 2 {
		t.Fatalf("page 2 = %v, want 2", second.UIDs)
	}
	// The pages are disjoint and strictly older.
	for _, a := range first.UIDs {
		for _, b := range second.UIDs {
			if a == b {
				t.Fatalf("pages overlap at UID %d", a)
			}
		}
	}
	if second.UIDs[0] >= first.NextCursor {
		t.Fatalf("page 2 not older than cursor %d: %v", first.NextCursor, second.UIDs)
	}
}

func TestFindByMessageIDAmbiguous(t *testing.T) {
	fs := newFakeServer(t, serverConfig{})
	fs.AddMailbox("INBOX")
	adapter := dialFake(t, fs, "user@example.com", "secret")
	seedMessage(t, adapter, "INBOX", sampleMessage, nil)
	seedMessage(t, adapter, "INBOX", sampleMessage, nil)

	_, err := adapter.FindByMessageID(testContext(t), "INBOX", "handoff-123@example.com")
	if err == nil {
		t.Fatal("expected ambiguity for a duplicated Message-ID")
	}
	if !errors.Is(err, imapadapter.ErrAmbiguous) {
		t.Fatalf("error = %v, want ErrAmbiguous", err)
	}
}
