package app_test

import (
	"context"
	"errors"
	"io"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/app"
	"github.com/dellarb/mammothmail/internal/mailparse"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/internal/transport"
)

func TestPublishedHandoffFilesRetiredByRunningWorker(t *testing.T) {
	t.Parallel()
	svc, u, _, _ := testService(t)
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "cleanup@remote.example")
	draft := standaloneDraft(t, svc, u.AccountID, box)
	p := assistantPrincipal(u.AccountID, box.ID)
	attachmentPath := filepath.Join(svc.Config.DataDir, "draft-attachment.bin")
	if err := os.WriteFile(attachmentPath, []byte("attachment"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.AddDraftAttachment(ctx, p, draft.ID, model.DraftAttachment{Filename: "file.bin", RawPath: "draft-attachment.bin", Size: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestSend(ctx, p, draft.ID, false); err != nil {
		t.Fatal(err)
	}
	handoff, err := svc.Store.LatestAssistantHandlingForDraft(ctx, u.AccountID, draft.ID)
	if err != nil || handoff == nil {
		t.Fatalf("handoff: %v", err)
	}
	frozen := filepath.Join(svc.Config.DataDir, handoff.RawPath)
	svc.SetHandoffPublisher(&fakeHandoff{confirmed: true, remoteUID: 42})
	svc.PublishHandoffs(ctx)
	// The store stays open; only the normal maintenance loop can retire the files.
	worker := app.NewOutboxWorker(svc, nil)
	worker.SetPeriod(5 * time.Millisecond)
	worker.Start()
	defer worker.Stop()
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, attachmentErr := os.Stat(attachmentPath)
		_, frozenErr := os.Stat(frozen)
		if errors.Is(attachmentErr, os.ErrNotExist) && errors.Is(frozenErr, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("published files retained: attachment=%v frozen=%v", attachmentErr, frozenErr)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := svc.Store.GetDraft(ctx, p, draft.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("published draft not consumed: %v", err)
	}
	stored, err := svc.Store.GetAssistantHandlingInternal(ctx, u.AccountID, handoff.ID)
	if err != nil || stored.Publication != model.HandoffPublished {
		t.Fatalf("handoff history lost: %+v %v", stored, err)
	}
}

// fakeHandoff is a deterministic HandoffPublisher. It records appends and can be
// configured to report a confirmed append (APPENDUID), an unconfirmed append that
// a lookup then verifies, an unconfirmed append whose lookup also fails
// (ambiguous), or a terminal failure.
type fakeHandoff struct {
	confirmed       bool
	remoteUID       uint32
	appendErr       error
	lookupErr       error
	lookupErrOnce   bool
	lookupHit       bool
	lookupAmbiguous bool
	appends         []fakeAppend
	lookups         int
	failAppend      bool
	failInbox       string
	seenHeader      string
}

type fakeAppend struct {
	inboxID   string
	raw       []byte
	messageID string
	handoffID string
}

func (f *fakeHandoff) Append(_ context.Context, inboxID string, raw []byte, messageID, handoffID string) (app.HandoffOutcome, error) {
	if f.failAppend && (f.failInbox == "" || f.failInbox == inboxID) {
		return app.HandoffOutcome{}, f.appendErr
	}
	f.appends = append(f.appends, fakeAppend{inboxID: inboxID, raw: append([]byte(nil), raw...), messageID: messageID, handoffID: handoffID})
	for _, line := range splitHeaderLines(raw) {
		if len(line) >= len(model.HandoffHeader)+2 && line[:len(model.HandoffHeader)] == model.HandoffHeader {
			f.seenHeader = line
		}
	}
	if f.confirmed {
		return app.HandoffOutcome{Confirmed: true, RemoteUID: f.remoteUID, RemoteFolder: "Drafts"}, nil
	}
	return app.HandoffOutcome{Confirmed: false}, nil
}

func (f *fakeHandoff) Lookup(_ context.Context, inboxID, handoffID, messageID string) (app.HandoffOutcome, error) {
	f.lookups++
	if f.lookupErr != nil && (!f.lookupErrOnce || f.lookups == 1) {
		return app.HandoffOutcome{}, f.lookupErr
	}
	if f.lookupHit {
		return app.HandoffOutcome{Confirmed: true, Found: true, RemoteUID: f.remoteUID, RemoteFolder: "Drafts"}, nil
	}
	if f.lookupAmbiguous {
		return app.HandoffOutcome{Found: true, Ambiguous: true, RemoteFolder: "Drafts"}, nil
	}
	return app.HandoffOutcome{Confirmed: false}, nil
}

func splitHeaderLines(raw []byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\n' {
			out = append(out, string(raw[start:i]))
			start = i + 1
		}
	}
	return out
}

// standaloneInbox creates a standalone inbox with a remote binding and seeds its
// system folders (including Drafts), as the setup flow would.
func standaloneInbox(t *testing.T, svc *app.Service, accountID, address string) model.Inbox {
	t.Helper()
	ctx := context.Background()
	box, err := svc.Store.CreateStandaloneInbox(ctx, accountID, store.StandaloneCreate{
		DisplayName: "Agent",
		Address:     address,
		Namespace:   model.NamespaceDefault,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.EnsureSystemFolders(ctx, accountID, box.ID); err != nil {
		t.Fatal(err)
	}
	return box
}

// standaloneDraft creates a real draft in a standalone inbox.
func standaloneDraft(t *testing.T, svc *app.Service, accountID string, box model.Inbox) model.Draft {
	t.Helper()
	asst := assistantPrincipal(accountID, box.ID)
	d, err := svc.Store.CreateDraft(context.Background(), asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "handoff", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestRequestSendStandaloneDefaultsToRemoteDraft proves a standalone inbox's
// default authoring mode is a one-way remote handoff: RequestSend freezes the
// draft, creates a handling record, and never creates a
// Mammoth Mail approval request.
func TestRequestSendStandaloneDefaultsToRemoteDraft(t *testing.T) {
	t.Parallel()
	svc, u, _, _ := testService(t)
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "agent@remote.example")
	asst := assistantPrincipal(u.AccountID, box.ID)
	d := standaloneDraft(t, svc, u.AccountID, box)

	got, err := svc.RequestSend(ctx, asst, d.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.DraftStatusPendingApproval {
		t.Fatalf("draft not frozen: %q", got.Status)
	}
	// No Mammoth Mail approval request was created.
	if srs, err := svc.Store.ListSendRequests(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, box.ID, false, 10); err != nil || len(srs) != 0 {
		t.Fatalf("standalone handoff created an approval request: %+v err=%v", srs, err)
	}
	// A handoff handling record exists with the frozen content hash and a stable
	// handoff id and Message-ID.
	h, err := svc.Store.LatestAssistantHandlingForDraft(ctx, u.AccountID, d.ID)
	if err != nil || h == nil {
		t.Fatalf("no handoff record: %v", err)
	}
	if h.Mode != model.AuthoringRemoteDraft || h.HandoffID == "" || h.MessageID == "" || h.ContentHash == "" {
		t.Fatalf("handoff record incomplete: %+v", h)
	}
	if h.RemoteFolder != "Drafts" {
		t.Fatalf("handoff target folder=%q want Drafts", h.RemoteFolder)
	}
	if h.Publication != model.HandoffPending {
		t.Fatalf("handoff publication=%q want pending", h.Publication)
	}
}

func TestRemoteDraftHandoffPreservesDraftBody(t *testing.T) {
	t.Parallel()
	svc, u, _, _ := testService(t)
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "content@remote.example")
	assistant := assistantPrincipal(u.AccountID, box.ID)
	want := "\tExact draft, verbatim.\r\nNo assistant preamble.\r\nUnicode: café — 🚀\r\n  trailing spaces  "
	d, err := svc.Store.CreateDraft(ctx, assistant, model.Draft{
		InboxID: box.ID,
		To:      []string{"x@y.test"},
		Subject: "Verbatim draft",
		Text:    want,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestSend(ctx, assistant, d.ID, false); err != nil {
		t.Fatal(err)
	}
	fake := &fakeHandoff{confirmed: true}
	svc.SetHandoffPublisher(fake)
	svc.PublishHandoffs(ctx)
	if len(fake.appends) != 1 {
		t.Fatalf("appends=%d want 1", len(fake.appends))
	}
	message, err := mail.ReadMessage(strings.NewReader(string(fake.appends[0].raw)))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(quotedprintable.NewReader(message.Body))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(decoded); got != want {
		t.Fatalf("remote draft body differs from API-supplied content\n got: %q\nwant: %q", got, want)
	}
}

// TestRemoteDraftHandoffEndToEnd proves the frozen draft is published to the
// remote Drafts folder through the injected publisher, carries the handoff
// header, and cleans up the local draft while retaining the handoff state.
func TestRemoteDraftHandoffEndToEnd(t *testing.T) {
	t.Parallel()
	svc, u, _, _ := testService(t)
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "handoff@remote.example")
	asst := assistantPrincipal(u.AccountID, box.ID)
	d := standaloneDraft(t, svc, u.AccountID, box)

	if _, err := svc.RequestSend(ctx, asst, d.ID, false); err != nil {
		t.Fatal(err)
	}
	fake := &fakeHandoff{confirmed: true, remoteUID: 987}
	svc.SetHandoffPublisher(fake)
	svc.PublishHandoffs(ctx)

	if len(fake.appends) != 1 {
		t.Fatalf("appends=%d want 1", len(fake.appends))
	}
	ap := fake.appends[0]
	if ap.inboxID != box.ID || ap.handoffID == "" || ap.messageID == "" {
		t.Fatalf("append correlation %+v", ap)
	}
	if fake.seenHeader == "" {
		t.Fatalf("frozen MIME missing %s header", model.HandoffHeader)
	}
	h, _ := svc.Store.LatestAssistantHandlingForDraft(ctx, u.AccountID, d.ID)
	got, err := svc.Store.GetAssistantHandlingInternal(ctx, u.AccountID, h.ID)
	if err != nil || got.Publication != model.HandoffPublished || got.RemoteUID != 987 || got.PublishedAt == nil {
		t.Fatalf("published state %+v err=%v", got, err)
	}
	// The local draft is consumed, the handoff record remains.
	if _, err := svc.Store.GetDraft(ctx, asst, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("local draft not cleaned up: %v", err)
	}
	// Re-running publication does not append again.
	svc.PublishHandoffs(ctx)
	if len(fake.appends) != 1 {
		t.Fatalf("handoff re-appended: %d appends", len(fake.appends))
	}
}

// TestRemoteDraftHandoffVerifiedByLookup proves an append with no APPENDUID is
// confirmed by lookup and only then settled as published.
func TestRemoteDraftHandoffVerifiedByLookup(t *testing.T) {
	t.Parallel()
	svc, u, _, _ := testService(t)
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "lookup@remote.example")
	asst := assistantPrincipal(u.AccountID, box.ID)
	d := standaloneDraft(t, svc, u.AccountID, box)
	if _, err := svc.RequestSend(ctx, asst, d.ID, false); err != nil {
		t.Fatal(err)
	}
	fake := &fakeHandoff{confirmed: false, lookupHit: true, remoteUID: 55}
	svc.SetHandoffPublisher(fake)
	svc.PublishHandoffs(ctx)
	if fake.lookups != 1 {
		t.Fatalf("lookups=%d want 1", fake.lookups)
	}
	h, _ := svc.Store.LatestAssistantHandlingForDraft(ctx, u.AccountID, d.ID)
	got, _ := svc.Store.GetAssistantHandlingInternal(ctx, u.AccountID, h.ID)
	if got.Publication != model.HandoffPublished || got.RemoteUID != 55 {
		t.Fatalf("lookup-verified state %+v", got)
	}
}

// TestRemoteDraftHandoffAmbiguousWhenUnverifiable proves an append that reports no
// APPENDUID and whose lookup cannot confirm is recorded as an explicit ambiguous
// state and is never re-appended.
func TestRemoteDraftHandoffAmbiguousWhenUnverifiable(t *testing.T) {
	t.Parallel()
	svc, u, _, _ := testService(t)
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "amb@remote.example")
	asst := assistantPrincipal(u.AccountID, box.ID)
	d := standaloneDraft(t, svc, u.AccountID, box)
	if _, err := svc.RequestSend(ctx, asst, d.ID, false); err != nil {
		t.Fatal(err)
	}
	fake := &fakeHandoff{confirmed: false, lookupHit: false}
	svc.SetHandoffPublisher(fake)
	svc.PublishHandoffs(ctx)
	h, _ := svc.Store.LatestAssistantHandlingForDraft(ctx, u.AccountID, d.ID)
	got, _ := svc.Store.GetAssistantHandlingInternal(ctx, u.AccountID, h.ID)
	if got.Publication != model.HandoffAmbiguous {
		t.Fatalf("state=%q want ambiguous", got.Publication)
	}
	svc.PublishHandoffs(ctx)
	if len(fake.appends) != 1 {
		t.Fatalf("ambiguous handoff re-appended: %d appends", len(fake.appends))
	}
}

// TestRemoteDraftHandoffTransientVerificationRetries proves a transient lookup
// failure during verification leaves the handoff pending and is retried by lookup
// on the next pass (never a blind re-append), rather than being terminalised as
// ambiguous.
func TestRemoteDraftHandoffTransientVerificationRetries(t *testing.T) {
	t.Parallel()
	svc, u, _, _ := testService(t)
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "verify@remote.example")
	asst := assistantPrincipal(u.AccountID, box.ID)
	d := standaloneDraft(t, svc, u.AccountID, box)
	if _, err := svc.RequestSend(ctx, asst, d.ID, false); err != nil {
		t.Fatal(err)
	}
	// The append is accepted but unconfirmed; the first verification lookup fails
	// transiently, and a later lookup finds the draft.
	fake := &fakeHandoff{confirmed: false, lookupErrOnce: true, lookupErr: errors.New("temporary verification failure"), lookupHit: true, remoteUID: 42}
	svc.SetHandoffPublisher(fake)
	svc.PublishHandoffs(ctx)
	h, _ := svc.Store.LatestAssistantHandlingForDraft(ctx, u.AccountID, d.ID)
	got, _ := svc.Store.GetAssistantHandlingInternal(ctx, u.AccountID, h.ID)
	if got.Publication != model.HandoffPending {
		t.Fatalf("state=%q want pending after transient verification failure", got.Publication)
	}
	// The next pass re-verifies by lookup and publishes; it must not append again.
	svc.PublishHandoffs(ctx)
	got, _ = svc.Store.GetAssistantHandlingInternal(ctx, u.AccountID, h.ID)
	if got.Publication != model.HandoffPublished || got.RemoteUID != 42 {
		t.Fatalf("state=%q uid=%d want published/42", got.Publication, got.RemoteUID)
	}
	if len(fake.appends) != 1 {
		t.Fatalf("appends=%d want 1 (no duplicate draft)", len(fake.appends))
	}
}

// TestRemoteDraftHandoffNoPublisherHolds proves a queued handoff stays pending
// when no publisher is installed (the remote integration is a later wave), rather
// than being failed.
func TestRemoteDraftHandoffNoPublisherHolds(t *testing.T) {
	t.Parallel()
	svc, u, _, _ := testService(t)
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "hold@remote.example")
	asst := assistantPrincipal(u.AccountID, box.ID)
	d := standaloneDraft(t, svc, u.AccountID, box)
	if _, err := svc.RequestSend(ctx, asst, d.ID, false); err != nil {
		t.Fatal(err)
	}
	svc.PublishHandoffs(ctx) // nil publisher: no-op
	h, _ := svc.Store.LatestAssistantHandlingForDraft(ctx, u.AccountID, d.ID)
	got, _ := svc.Store.GetAssistantHandlingInternal(ctx, u.AccountID, h.ID)
	if got.Publication != model.HandoffPending {
		t.Fatalf("state=%q want pending (held)", got.Publication)
	}
}

// TestRemoteDraftHandoffPermanentFailure proves a terminal append error settles
// the handoff as failed and unfreezes the draft.
func TestRemoteDraftHandoffPermanentFailure(t *testing.T) {
	t.Parallel()
	svc, u, _, _ := testService(t)
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "fail@remote.example")
	asst := assistantPrincipal(u.AccountID, box.ID)
	d := standaloneDraft(t, svc, u.AccountID, box)
	if _, err := svc.RequestSend(ctx, asst, d.ID, false); err != nil {
		t.Fatal(err)
	}
	fake := &fakeHandoff{failAppend: true, appendErr: store.ErrForbidden}
	svc.SetHandoffPublisher(fake)
	svc.PublishHandoffs(ctx)
	h, _ := svc.Store.LatestAssistantHandlingForDraft(ctx, u.AccountID, d.ID)
	got, _ := svc.Store.GetAssistantHandlingInternal(ctx, u.AccountID, h.ID)
	if got.Publication != model.HandoffFailed {
		t.Fatalf("state=%q want failed", got.Publication)
	}
	draft, err := svc.Store.GetDraft(ctx, asst, d.ID)
	if err != nil || draft.Status != model.DraftStatusDraft {
		t.Fatalf("draft after permanent failure %+v err=%v", draft, err)
	}
}

// TestRemoteDraftHandoffDefinitiveRemoteRefusalFails proves that a remote
// server refusing the append for a reason that cannot resolve itself — the
// destination folder does not exist, or the server refuses the operation —
// settles the handoff as failed. The refusal is definitive, so the handoff is
// not retried into an ambiguous state, and its local draft is unfrozen.
func TestRemoteDraftHandoffDefinitiveRemoteRefusalFails(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err    error
		reason string
	}{
		"destination folder does not exist": {
			err:    model.NewMailboxError(model.ErrKindNotFound, "remote destination does not exist", false, errors.New("NO [TRYCREATE] No such folder : Drafts")),
			reason: "Drafts folder",
		},
		"server refuses the append": {
			err:    model.NewMailboxError(model.ErrKindForbidden, "remote server refused the operation", false, errors.New("NO [CANNOT]")),
			reason: "refused the append",
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc, u, _, _ := testService(t)
			ctx := context.Background()
			box := standaloneInbox(t, svc, u.AccountID, "refused@remote.example")
			asst := assistantPrincipal(u.AccountID, box.ID)
			d := standaloneDraft(t, svc, u.AccountID, box)
			if _, err := svc.RequestSend(ctx, asst, d.ID, false); err != nil {
				t.Fatal(err)
			}
			fake := &fakeHandoff{failAppend: true, appendErr: tc.err}
			svc.SetHandoffPublisher(fake)
			svc.PublishHandoffs(ctx)
			h, _ := svc.Store.LatestAssistantHandlingForDraft(ctx, u.AccountID, d.ID)
			got, _ := svc.Store.GetAssistantHandlingInternal(ctx, u.AccountID, h.ID)
			if got.Publication != model.HandoffFailed {
				t.Fatalf("state=%q want failed", got.Publication)
			}
			if !strings.Contains(got.LastError, tc.reason) {
				t.Fatalf("reason=%q want it to name %q", got.LastError, tc.reason)
			}
			draft, err := svc.Store.GetDraft(ctx, asst, d.ID)
			if err != nil || draft.Status != model.DraftStatusDraft {
				t.Fatalf("draft after terminal failure %+v err=%v", draft, err)
			}
			// A settled handoff is never claimed again: no verification lookup,
			// no re-append.
			svc.PublishHandoffs(ctx)
			if fake.lookups != 0 {
				t.Fatalf("lookups=%d want 0 (a settled handoff is not verified again)", fake.lookups)
			}
		})
	}
}

// TestRemoteDraftRequestRequiresCanAssist proves a read-only principal cannot
// request a handoff.
func TestRemoteDraftRequestRequiresCanAssist(t *testing.T) {
	t.Parallel()
	svc, u, _, _ := testService(t)
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "perm@remote.example")
	d := standaloneDraft(t, svc, u.AccountID, box)
	read := model.Principal{AccountID: u.AccountID, MailboxRoles: map[string]string{box.ID: "read"}}
	if _, err := svc.RequestSend(ctx, read, d.ID, false); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("read request err=%v want forbidden", err)
	}
}

// TestRemoteDraftModeSnapshotImmutable proves the mode is snapshotted per request:
// flipping the inbox setting to LocalApproval after a handoff is created does
// not convert the in-flight handoff.
func TestRemoteDraftModeSnapshotImmutable(t *testing.T) {
	t.Parallel()
	svc, u, _, _ := testService(t)
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "snap@remote.example")
	own := ownerPrincipal(u.AccountID, box.ID)
	asst := assistantPrincipal(u.AccountID, box.ID)
	d := standaloneDraft(t, svc, u.AccountID, box)
	if _, err := svc.RequestSend(ctx, asst, d.ID, false); err != nil {
		t.Fatal(err)
	}
	// Flip the inbox to LocalApproval.
	if err := svc.Store.SetInboxAuthoringMode(ctx, own, box.ID, model.AuthoringLocalApproval); err != nil {
		t.Fatal(err)
	}
	// The in-flight request is still a handoff with mode remote_draft and is not
	// converted to an approval request.
	h, err := svc.Store.LatestAssistantHandlingForDraft(ctx, u.AccountID, d.ID)
	if err != nil || h == nil || h.Mode != model.AuthoringRemoteDraft {
		t.Fatalf("handoff mode changed by setting flip: %+v err=%v", h, err)
	}
	if srs, _ := svc.Store.ListSendRequests(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, box.ID, false, 10); len(srs) != 0 {
		t.Fatalf("setting flip converted the handoff: %+v", srs)
	}
}

// TestRemoteDraftModeMammothMailOptIn proves an operator can opt a standalone inbox
// back into the Mammoth Mail approval workflow, which then creates a real send
// request with a queued notification even though the inbox has no domain provider
// (the notification is queued and held, not failed).
func TestRemoteDraftModeMammothMailOptIn(t *testing.T) {
	t.Parallel()
	svc, u, _, _ := testService(t)
	svc.Config.ApprovalExpiryHours = 48
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "optin@remote.example")
	own := ownerPrincipal(u.AccountID, box.ID)
	asst := assistantPrincipal(u.AccountID, box.ID)
	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, box.ID, approverAddress); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxAuthoringMode(ctx, own, box.ID, model.AuthoringLocalApproval); err != nil {
		t.Fatal(err)
	}
	d := standaloneDraft(t, svc, u.AccountID, box)
	got, err := svc.RequestSend(ctx, asst, d.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.SendRequest == nil || got.SendRequest.ApproverEmail != approverAddress {
		t.Fatalf("standalone approval request %+v", got.SendRequest)
	}
	// The notification is queued (held); the request reports the missing-provider
	// state through the workflow job, not an invalid domain.
	if got.SendRequest.ApprovalWorkflowID == "" {
		t.Fatalf("no notification workflow queued: %+v", got.SendRequest)
	}
	w, err := svc.Store.GetWorkflowInternal(ctx, u.AccountID, got.SendRequest.ApprovalWorkflowID)
	if err != nil {
		t.Fatal(err)
	}
	if w.Provider != "" || w.LastError == "" {
		t.Fatalf("standalone workflow job %+v; want no provider and a held reason", w)
	}
}

// TestHandleRemoteApprovalControlMatchesFromWithoutEnvelope proves the standalone
// approval path authorizes on the exact nominated From address alone — no envelope
// is fabricated — and consumes the message.
func TestHandleRemoteApprovalControlMatchesFromWithoutEnvelope(t *testing.T) {
	t.Parallel()
	svc, u, _, _ := testService(t)
	svc.Config.ApprovalExpiryHours = 48
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "remote-approve@remote.example")
	own := ownerPrincipal(u.AccountID, box.ID)
	asst := assistantPrincipal(u.AccountID, box.ID)
	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, box.ID, approverAddress); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxAuthoringMode(ctx, own, box.ID, model.AuthoringLocalApproval); err != nil {
		t.Fatal(err)
	}
	d := standaloneDraft(t, svc, u.AccountID, box)
	if _, err := svc.RequestSend(ctx, asst, d.ID, true); err != nil {
		t.Fatal(err)
	}
	token := approvalToken(t, svc, u.AccountID, box.ID)

	parsed := mailparse.Parsed{
		Subject: "[GH-APPROVE:" + token + "]",
		From:    mailparse.Address{Name: "Ben", Address: approverAddress},
		Text:    "[GH-FEEDBACK-BEGIN]\r\n\r\n[GH-FEEDBACK-END]\r\n",
	}
	raw := []byte("From: Ben <" + approverAddress + ">\r\nSubject: [GH-APPROVE:" + token + "]\r\n\r\nbody")
	if err := svc.HandleRemoteApprovalControl(ctx, box, parsed, raw); !errors.Is(err, transport.ErrInboundIgnored) {
		t.Fatalf("standalone control err=%v", err)
	}
	sr, err := svc.Store.GetSendRequestByDraft(ctx, asst, d.ID)
	if err != nil || sr.Status != model.SendRequestApproved || sr.DecisionMethod != model.DecisionMethodEmail {
		t.Fatalf("standalone approval %+v err=%v", sr, err)
	}
}

// TestHandleRemoteApprovalControlRejectsMismatchedFrom proves a From that is not
// the nominated approver does not authorize the request.
func TestHandleRemoteApprovalControlRejectsMismatchedFrom(t *testing.T) {
	t.Parallel()
	svc, u, _, _ := testService(t)
	svc.Config.ApprovalExpiryHours = 48
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "remote-mismatch@remote.example")
	own := ownerPrincipal(u.AccountID, box.ID)
	asst := assistantPrincipal(u.AccountID, box.ID)
	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, box.ID, approverAddress); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxAuthoringMode(ctx, own, box.ID, model.AuthoringLocalApproval); err != nil {
		t.Fatal(err)
	}
	d := standaloneDraft(t, svc, u.AccountID, box)
	if _, err := svc.RequestSend(ctx, asst, d.ID, true); err != nil {
		t.Fatal(err)
	}
	token := approvalToken(t, svc, u.AccountID, box.ID)
	parsed := mailparse.Parsed{
		Subject: "[GH-APPROVE:" + token + "]",
		From:    mailparse.Address{Address: "mallory@evil.test"},
	}
	raw := []byte("From: mallory@evil.test\r\n\r\nbody")
	if err := svc.HandleRemoteApprovalControl(ctx, box, parsed, raw); !errors.Is(err, transport.ErrInboundIgnored) {
		t.Fatalf("mismatched control err=%v", err)
	}
	sr, err := svc.Store.GetSendRequestByDraft(ctx, asst, d.ID)
	if err != nil || sr.Status != model.SendRequestPending {
		t.Fatalf("mismatched From changed state: %+v err=%v", sr, err)
	}
}

// TestHandleRemoteApprovalControlExcludesHandoffHeader proves a message carrying
// the handoff correlation header is excluded from approval handling.
func TestHandleRemoteApprovalControlExcludesHandoffHeader(t *testing.T) {
	t.Parallel()
	svc, u, _, _ := testService(t)
	svc.Config.ApprovalExpiryHours = 48
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "remote-exclude@remote.example")
	own := ownerPrincipal(u.AccountID, box.ID)
	asst := assistantPrincipal(u.AccountID, box.ID)
	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, box.ID, approverAddress); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxAuthoringMode(ctx, own, box.ID, model.AuthoringLocalApproval); err != nil {
		t.Fatal(err)
	}
	d := standaloneDraft(t, svc, u.AccountID, box)
	if _, err := svc.RequestSend(ctx, asst, d.ID, true); err != nil {
		t.Fatal(err)
	}
	token := approvalToken(t, svc, u.AccountID, box.ID)
	parsed := mailparse.Parsed{
		Subject: "[GH-APPROVE:" + token + "]",
		From:    mailparse.Address{Address: approverAddress},
	}
	raw := []byte(model.HandoffHeader + ": hnd_x\r\nFrom: Ben <" + approverAddress + ">\r\nSubject: [GH-APPROVE:" + token + "]\r\n\r\nbody")
	if err := svc.HandleRemoteApprovalControl(ctx, box, parsed, raw); !errors.Is(err, transport.ErrInboundIgnored) {
		t.Fatalf("handoff-header control err=%v", err)
	}
	// A draft appended before the rename keeps the old spelling.
	legacy := []byte(model.LegacyHandoffHeader + ": hnd_x\r\nFrom: Ben <" + approverAddress + ">\r\nSubject: [GH-APPROVE:" + token + "]\r\n\r\nbody")
	if err := svc.HandleRemoteApprovalControl(ctx, box, parsed, legacy); !errors.Is(err, transport.ErrInboundIgnored) {
		t.Fatalf("legacy handoff-header control err=%v", err)
	}
	sr, err := svc.Store.GetSendRequestByDraft(ctx, asst, d.ID)
	if err != nil || sr.Status != model.SendRequestPending {
		t.Fatalf("handoff-header message changed the request: %+v err=%v", sr, err)
	}
}

// TestRemoteDraftHandoffTransientFailureRetriesNextPass proves a transient append
// error leaves the handoff pending and it is retried on the next pass, without
// spinning within a single pass.
func TestRemoteDraftHandoffTransientFailureRetriesNextPass(t *testing.T) {
	t.Parallel()
	svc, u, _, _ := testService(t)
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "transient@remote.example")
	asst := assistantPrincipal(u.AccountID, box.ID)
	d := standaloneDraft(t, svc, u.AccountID, box)
	if _, err := svc.RequestSend(ctx, asst, d.ID, false); err != nil {
		t.Fatal(err)
	}
	// A transient failure (not permanent) leaves the handoff pending and appends
	// once per pass.
	fake := &fakeHandoff{failAppend: true, appendErr: errors.New("temporary network fault")}
	svc.SetHandoffPublisher(fake)
	svc.PublishHandoffs(ctx)
	h, _ := svc.Store.LatestAssistantHandlingForDraft(ctx, u.AccountID, d.ID)
	got, _ := svc.Store.GetAssistantHandlingInternal(ctx, u.AccountID, h.ID)
	if got.Publication != model.HandoffPending {
		t.Fatalf("transient failure state=%q want pending", got.Publication)
	}
	if got.Attempts != 1 {
		t.Fatalf("one pass counted %d attempts", got.Attempts)
	}
	// The record is still claimable and a subsequent pass publishes it.
	fake.failAppend = false
	fake.confirmed = true
	fake.remoteUID = 42
	svc.PublishHandoffs(ctx)
	got, _ = svc.Store.GetAssistantHandlingInternal(ctx, u.AccountID, h.ID)
	if got.Publication != model.HandoffPublished || got.RemoteUID != 42 {
		t.Fatalf("retry did not publish: %+v", got)
	}
}

func TestHandoffFailureDoesNotBlockAnotherAccount(t *testing.T) {
	t.Parallel()
	svc, u, _, _ := testService(t)
	ctx := context.Background()
	broken := standaloneInbox(t, svc, u.AccountID, "broken@remote.example")
	draft := standaloneDraft(t, svc, u.AccountID, broken)
	if _, err := svc.RequestSend(ctx, assistantPrincipal(u.AccountID, broken.ID), draft.ID, false); err != nil {
		t.Fatal(err)
	}
	other, err := svc.Store.CreateAccountAndAdmin(ctx, "Other", "other@example.com", "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	healthy := standaloneInbox(t, svc, other.AccountID, "healthy@remote.example")
	otherDraft := standaloneDraft(t, svc, other.AccountID, healthy)
	if _, err := svc.RequestSend(ctx, assistantPrincipal(other.AccountID, healthy.ID), otherDraft.ID, false); err != nil {
		t.Fatal(err)
	}
	fake := &fakeHandoff{failAppend: true, failInbox: broken.ID, appendErr: errors.New("temporary network fault"), confirmed: true, remoteUID: 42}
	svc.SetHandoffPublisher(fake)
	for pass := 1; pass <= 8; pass++ {
		svc.PublishHandoffs(ctx)
		failed, err := svc.Store.LatestAssistantHandlingForDraft(ctx, u.AccountID, draft.ID)
		if err != nil || failed == nil {
			t.Fatalf("broken handoff: %+v, %v", failed, err)
		}
		want := model.HandoffPending
		if pass == 8 {
			want = model.HandoffFailed
		}
		if failed.Attempts != pass || failed.Publication != want {
			t.Fatalf("pass %d: attempts=%d publication=%s, want %s", pass, failed.Attempts, failed.Publication, want)
		}
		published, err := svc.Store.LatestAssistantHandlingForDraft(ctx, other.AccountID, otherDraft.ID)
		if err != nil || published == nil || published.Publication != model.HandoffPublished || published.Attempts != 1 {
			t.Fatalf("healthy account blocked on pass %d: %+v, %v", pass, published, err)
		}
	}
}

// TestOwnerSendOnStandaloneIsNotHandoff proves an Owner's direct Send is not
// converted to a handoff: it queues a normal outbound message (held for lack of a
// provider) rather than creating a handling record.
func TestOwnerSendOnStandaloneIsNotHandoff(t *testing.T) {
	t.Parallel()
	svc, u, _, _ := testService(t)
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "owner-send@remote.example")
	own := ownerPrincipal(u.AccountID, box.ID)
	if _, err := svc.Send(ctx, own, app.SendInput{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "b"}, ""); err != nil {
		t.Fatal(err)
	}
	if h, _ := svc.Store.LatestAssistantHandlingForDraft(ctx, u.AccountID, ""); h != nil {
		t.Fatalf("owner send created a handoff: %+v", h)
	}
	msgs, err := svc.Store.ListOutbox(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, box.ID, 10)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("owner send not queued: %+v err=%v", msgs, err)
	}
}
