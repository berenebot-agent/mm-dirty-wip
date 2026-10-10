package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/mailparse"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport"
)

// fakeHandoff is a deterministic HandoffPublisher. It records appends and can be
// configured to report a confirmed append (APPENDUID), an unconfirmed append that
// a lookup then verifies, an unconfirmed append whose lookup also fails
// (ambiguous), or a terminal failure.
type fakeHandoff struct {
	confirmed  bool
	remoteUID  uint32
	appendErr  error
	lookupErr  error
	lookupHit  bool
	appends    []fakeAppend
	lookups    int
	failAppend bool
	seenHeader string
}

type fakeAppend struct {
	inboxID   string
	raw       []byte
	messageID string
	handoffID string
}

func (f *fakeHandoff) Append(_ context.Context, inboxID string, raw []byte, messageID, handoffID string) (app.HandoffOutcome, error) {
	if f.failAppend {
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
	if f.lookupErr != nil {
		return app.HandoffOutcome{}, f.lookupErr
	}
	if f.lookupHit {
		return app.HandoffOutcome{Confirmed: true, RemoteUID: f.remoteUID, RemoteFolder: "Drafts"}, nil
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
// draft, creates a handling record and queues a notification, and never creates a
// MailMoose approval request.
func TestRequestSendStandaloneDefaultsToRemoteDraft(t *testing.T) {
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
	// No MailMoose approval request was created.
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
	// The handoff notification is queued through the existing workflow queue with
	// the handoff kind and no approval token.
	wfID, err := svc.Store.HandoffNotificationWorkflowID(ctx, u.AccountID, h.ID)
	if err != nil || wfID == "" {
		t.Fatalf("no handoff notification workflow queued: %v", err)
	}
	w, err := svc.Store.GetWorkflowInternal(ctx, u.AccountID, wfID)
	if err != nil {
		t.Fatal(err)
	}
	if w.Kind != model.WorkflowKindHandoff {
		t.Fatalf("notification workflow kind=%q want %q", w.Kind, model.WorkflowKindHandoff)
	}
	if strings.Contains(w.Text, "[GH-") || strings.Contains(w.HTML, "[GH-") {
		t.Fatalf("handoff notification carries an approval token marker")
	}
}

// TestRemoteDraftHandoffEndToEnd proves the frozen draft is published to the
// remote Drafts folder through the injected publisher, carries the handoff
// header, and cleans up the local draft while retaining the handoff state.
func TestRemoteDraftHandoffEndToEnd(t *testing.T) {
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

// TestRemoteDraftHandoffNoPublisherHolds proves a queued handoff stays pending
// when no publisher is installed (the remote integration is a later wave), rather
// than being failed.
func TestRemoteDraftHandoffNoPublisherHolds(t *testing.T) {
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

// TestRemoteDraftRequestRequiresCanAssist proves a read-only principal cannot
// request a handoff.
func TestRemoteDraftRequestRequiresCanAssist(t *testing.T) {
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
// flipping the inbox setting to MailMooseApproval after a handoff is created does
// not convert the in-flight handoff.
func TestRemoteDraftModeSnapshotImmutable(t *testing.T) {
	svc, u, _, _ := testService(t)
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "snap@remote.example")
	own := ownerPrincipal(u.AccountID, box.ID)
	asst := assistantPrincipal(u.AccountID, box.ID)
	d := standaloneDraft(t, svc, u.AccountID, box)
	if _, err := svc.RequestSend(ctx, asst, d.ID, false); err != nil {
		t.Fatal(err)
	}
	// Flip the inbox to MailMooseApproval.
	if err := svc.Store.SetInboxAuthoringMode(ctx, own, box.ID, model.AuthoringMailMooseApproval); err != nil {
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

// TestRemoteDraftModeMailMooseOptIn proves an operator can opt a standalone inbox
// back into the MailMoose approval workflow, which then creates a real send
// request with a queued notification even though the inbox has no domain provider
// (the notification is queued and held, not failed).
func TestRemoteDraftModeMailMooseOptIn(t *testing.T) {
	svc, u, _, _ := testService(t)
	svc.Config.ApprovalExpiryHours = 48
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "optin@remote.example")
	own := ownerPrincipal(u.AccountID, box.ID)
	asst := assistantPrincipal(u.AccountID, box.ID)
	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, box.ID, approverAddress); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxAuthoringMode(ctx, own, box.ID, model.AuthoringMailMooseApproval); err != nil {
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
	svc, u, _, _ := testService(t)
	svc.Config.ApprovalExpiryHours = 48
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "remote-approve@remote.example")
	own := ownerPrincipal(u.AccountID, box.ID)
	asst := assistantPrincipal(u.AccountID, box.ID)
	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, box.ID, approverAddress); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxAuthoringMode(ctx, own, box.ID, model.AuthoringMailMooseApproval); err != nil {
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
	svc, u, _, _ := testService(t)
	svc.Config.ApprovalExpiryHours = 48
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "remote-mismatch@remote.example")
	own := ownerPrincipal(u.AccountID, box.ID)
	asst := assistantPrincipal(u.AccountID, box.ID)
	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, box.ID, approverAddress); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxAuthoringMode(ctx, own, box.ID, model.AuthoringMailMooseApproval); err != nil {
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

// TestHandleRemoteApprovalControlExcludesHandoffNotification proves a message
// carrying the handoff correlation header is excluded from approval handling.
func TestHandleRemoteApprovalControlExcludesHandoffNotification(t *testing.T) {
	svc, u, _, _ := testService(t)
	svc.Config.ApprovalExpiryHours = 48
	ctx := context.Background()
	box := standaloneInbox(t, svc, u.AccountID, "remote-exclude@remote.example")
	own := ownerPrincipal(u.AccountID, box.ID)
	asst := assistantPrincipal(u.AccountID, box.ID)
	if err := svc.Store.SetInboxApprover(ctx, u.AccountID, box.ID, approverAddress); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxAuthoringMode(ctx, own, box.ID, model.AuthoringMailMooseApproval); err != nil {
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
	sr, err := svc.Store.GetSendRequestByDraft(ctx, asst, d.ID)
	if err != nil || sr.Status != model.SendRequestPending {
		t.Fatalf("handoff notification changed the request: %+v err=%v", sr, err)
	}
}

// TestRemoteDraftHandoffTransientFailureRetriesNextPass proves a transient append
// error leaves the handoff pending and it is retried on the next pass, without
// spinning within a single pass.
func TestRemoteDraftHandoffTransientFailureRetriesNextPass(t *testing.T) {
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

// TestOwnerSendOnStandaloneIsNotHandoff proves an Owner's direct Send is not
// converted to a handoff: it queues a normal outbound message (held for lack of a
// provider) rather than creating a handling record.
func TestOwnerSendOnStandaloneIsNotHandoff(t *testing.T) {
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
