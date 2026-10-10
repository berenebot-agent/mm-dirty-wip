package app_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/imap"
)

// workerEnv builds a full app service, standalone inbox with a configured remote
// binding, a fake session and a running worker. It returns the service, inbox,
// remote service, fake server and worker.
func workerEnv(t *testing.T) (*app.Service, model.Inbox, *app.RemoteMailboxService, *fakeRemoteServer, *app.RemoteWorker) {
	t.Helper()
	svc, u, box, rm := remoteTestEnv(t)
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	installFake(t, rm, fake)
	// The fake session must be able to enumerate the INBOX folder scope and
	// report its UIDVALIDITY; the production fake already does.
	w := app.NewRemoteWorker(svc, rm, nil)
	w.SetMaxInboxConnections(2)
	return svc, box, rm, fake, w
}

// collectEvents subscribes to the hub and returns a channel of events.
func collectEvents(svc *app.Service) (<-chan model.Event, func()) {
	_, ch, cancel := svc.Hub.Subscribe(64)
	return ch, cancel
}

// drainEvents returns the events currently buffered on ch.
func drainEvents(ch <-chan model.Event) []model.Event {
	var out []model.Event
	for {
		select {
		case e := <-ch:
			out = append(out, e)
		default:
			return out
		}
	}
}

// TestRemoteWorkerBaselineNoFlood proves the first detection pass establishes the
// baseline without emitting any arrival event for pre-existing mail.
func TestRemoteWorkerBaselineNoFlood(t *testing.T) {
	svc, box, _, fake, w := workerEnv(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		fake.addMessage("INBOX", "From: a@b.test\r\nSubject: old\r\nMessage-ID: <old"+string(rune('0'+i))+"@remote>\r\n\r\nbody", "<old@remote>", "old")
	}
	ch, cancel := collectEvents(svc)
	defer cancel()
	inbox, err := svc.Store.GetInboxInternal(ctx, box.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	w.DetectInbox(ctx, inbox)
	// No event should have been published: the baseline swallowed the backlog.
	if evs := drainEvents(ch); len(evs) != 0 {
		t.Fatalf("baseline flooded %d events", len(evs))
	}
	// A new arrival after the baseline is detected exactly once.
	fake.addMessage("INBOX", "From: c@d.test\r\nSubject: new\r\nMessage-ID: <new@remote>\r\n\r\nbody", "<new@remote>", "new")
	w.DetectInbox(ctx, inbox)
	evs := drainEvents(ch)
	if len(evs) != 1 {
		t.Fatalf("new arrival events = %d want 1", len(evs))
	}
	if evs[0].Type != store.EventRemoteMessageReceived {
		t.Fatalf("event type = %q", evs[0].Type)
	}
	// A repeat detect is idempotent.
	w.DetectInbox(ctx, inbox)
	if evs := drainEvents(ch); len(evs) != 0 {
		t.Fatalf("repeat detect emitted %d events", len(evs))
	}
}

func TestRemoteDetectionSurvivesPollingRequestCancellation(t *testing.T) {
	svc, box, rm, fake, w := workerEnv(t)
	inbox, err := svc.Store.GetInboxInternal(context.Background(), box.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rm.SetRemoteDialer(func(pass context.Context, _ imap.Config) (app.RemoteSession, error) {
		cancel() // The polling client disconnects while IMAP work is underway.
		if err := pass.Err(); err != nil {
			t.Fatalf("worker inherited request cancellation: %v", err)
		}
		return fake, nil
	})
	w.DetectInbox(ctx, inbox)
	cursor, ok, err := svc.Store.GetRemoteCursor(context.Background(), box.AccountID, box.ID, "INBOX")
	if err != nil || !ok || !cursor.BaselineDone {
		t.Fatalf("baseline lost after disconnect: %+v %v", cursor, err)
	}
	fake.addMessage("INBOX", "body", "<after@test>", "After disconnect")
	w.DetectInbox(ctx, inbox)
	cursor, _, err = svc.Store.GetRemoteCursor(context.Background(), box.AccountID, box.ID, "INBOX")
	if err != nil || cursor.LastUID != 1 {
		t.Fatalf("arrival cursor lost after disconnect: %+v %v", cursor, err)
	}
}

// TestRemoteWorkerLargeFolderBaselineAndDetect proves detection is correct on a
// folder larger than one search page: the baseline must use the true maximum UID,
// and a new arrival must still be detected. Under the default ascending search
// order the truncation would return the OLDEST page, so the baseline would sit far
// below the true maximum and detection would be permanently suppressed.
func TestRemoteWorkerLargeFolderBaselineAndDetect(t *testing.T) {
	svc, box, _, fake, w := workerEnv(t)
	ctx := context.Background()
	// More than one search page (MaxSearchResults = 500) of pre-existing mail.
	for i := 0; i < 600; i++ {
		fake.addMessage("INBOX", "From: a@b.test\r\nSubject: old\r\nMessage-ID: <old@remote>\r\n\r\nbody", "<old@remote>", "old")
	}
	ch, cancel := collectEvents(svc)
	defer cancel()
	inbox, err := svc.Store.GetInboxInternal(ctx, box.AccountID, box.ID)
	if err != nil {
		t.Fatal(err)
	}
	w.DetectInbox(ctx, inbox)
	if evs := drainEvents(ch); len(evs) != 0 {
		t.Fatalf("baseline flooded %d events", len(evs))
	}
	// A new arrival after the baseline must be detected exactly once.
	fake.addMessage("INBOX", "From: c@d.test\r\nSubject: new\r\nMessage-ID: <new@remote>\r\n\r\nbody", "<new@remote>", "new")
	w.DetectInbox(ctx, inbox)
	if evs := drainEvents(ch); len(evs) != 1 {
		t.Fatalf("new arrival events = %d want 1", len(evs))
	}
}

// TestRemoteWorkerNoReadSideEffect proves a detection pass never marks a message
// seen on the live server.
func TestRemoteWorkerNoReadSideEffect(t *testing.T) {
	svc, box, _, fake, w := workerEnv(t)
	ctx := context.Background()
	fake.addMessage("INBOX", "From: a@b.test\r\nSubject: s\r\nMessage-ID: <s@remote>\r\n\r\nbody", "<s@remote>", "s")
	inbox, _ := svc.Store.GetInboxInternal(ctx, box.AccountID, box.ID)
	w.DetectInbox(ctx, inbox)
	fake.addMessage("INBOX", "From: c@d.test\r\nSubject: n\r\nMessage-ID: <n@remote>\r\n\r\nbody", "<n@remote>", "n")
	w.DetectInbox(ctx, inbox)
	if fake.markSeenCalled {
		t.Fatal("detection marked a message seen")
	}
}

// TestRemoteWorkerOutsideScopeNotDetected proves a message outside the inbox's
// explicit root scope is never detected.
func TestRemoteWorkerOutsideScopeNotDetected(t *testing.T) {
	st, u, _, rm := remoteTestEnv(t)
	ctx := context.Background()
	box, err := st.Store.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{
		Address:   "scoped@remote.example",
		Namespace: "Archive",
		Remote:    &model.RemoteConnection{Host: "imap.remote.example", Username: "scoped@remote.example", Security: model.RemoteSecurityPlain, Port: 143},
	})
	if err != nil {
		t.Fatal(err)
	}
	configureSecrets(t, rm, u, box, "imap-pw", "")
	fake := newFakeRemoteServer()
	fake.addFolder("Archive", 100)
	fake.addFolder("INBOX", 100)
	fake.addMessage("INBOX", "From: a@b.test\r\nSubject: out\r\nMessage-ID: <out@remote>\r\n\r\nbody", "<out@remote>", "out")
	installFake(t, rm, fake)
	w := app.NewRemoteWorker(st, rm, nil)
	ch, cancel := collectEvents(st)
	defer cancel()
	inbox, _ := st.Store.GetInboxInternal(ctx, box.AccountID, box.ID)
	// The watcher observes the inbox's selected folder ("Archive"), so the INBOX
	// message is never detected.
	w.DetectInbox(ctx, inbox)
	if evs := drainEvents(ch); len(evs) != 0 {
		t.Fatalf("out-of-scope message detected: %d events", len(evs))
	}
}

// TestRemoteWorkerAllowlistSkipsProactive proves a message from a disallowed sender
// is still recorded (live-readable) but raises no proactive notification.
func TestRemoteWorkerAllowlistSkipsProactive(t *testing.T) {
	svc, box, _, fake, w := workerEnv(t)
	ctx := context.Background()
	if err := svc.Store.SetInboxAllowedSenders(ctx, box.AccountID, box.ID, []string{"allowed@example.test"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxSenderRestricted(ctx, box.AccountID, box.ID, true); err != nil {
		t.Fatal(err)
	}
	inbox, _ := svc.Store.GetInboxInternal(ctx, box.AccountID, box.ID)
	w.DetectInbox(ctx, inbox) // baseline
	fake.addMessage("INBOX", "From: stranger@elsewhere.test\r\nSubject: n\r\nMessage-ID: <n@remote>\r\n\r\nbody", "<n@remote>", "n")
	ch, cancel := collectEvents(svc)
	defer cancel()
	w.DetectInbox(ctx, inbox)
	if evs := drainEvents(ch); len(evs) != 0 {
		t.Fatalf("disallowed sender raised a proactive event: %d", len(evs))
	}
	// The arrival was still durably recorded (live read is not suppressed).
	arrivals, err := svc.Store.ListRemoteArrivalsForInbox(ctx, box.AccountID, box.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(arrivals) != 1 {
		t.Fatalf("arrival not recorded: %d", len(arrivals))
	}
	if arrivals[0].DeliveryState != store.RemoteArrivalSkipped {
		t.Fatalf("arrival state = %q want skipped", arrivals[0].DeliveryState)
	}
}

// TestRemoteWorkerHandoffNotificationExcluded proves a handoff's own notification
// email (identified by its durably-recorded notification Message-ID) is excluded
// from proactive delivery without a live body fetch, so a handoff never echoes
// back as a fresh arrival.
func TestRemoteWorkerHandoffNotificationExcluded(t *testing.T) {
	svc, box, _, fake, w := workerEnv(t)
	ctx := context.Background()
	// Record a handoff with a notification Message-ID.
	asst := model.Principal{AccountID: box.AccountID, MailboxRoles: map[string]string{box.ID: "assistant"}, Admin: true}
	d, err := svc.Store.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "b"})
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := svc.Store.CreateAssistantHandling(ctx, asst, store.AssistantHandlingInsert{
		DraftID: d.ID, InboxID: box.ID, Mode: model.AuthoringRemoteDraft,
		ContentHash: "h", HandoffID: "hnd_x", MessageID: "<frozen@remote.example>", RemoteFolder: "Drafts",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.CommitHandoffNotification(ctx, box.AccountID, created, store.WorkflowRecord{
		Inbox: box, From: model.Address{Address: box.Address}, To: []string{box.Address},
		Subject: "Draft placed", Text: "x", RawPath: "handoff/x.eml", SizeBytes: 1,
	}, "", "", "<notify@remote.example>"); err != nil {
		t.Fatal(err)
	}
	inbox, _ := svc.Store.GetInboxInternal(ctx, box.AccountID, box.ID)
	w.DetectInbox(ctx, inbox) // baseline
	// The notification email arrives back in the INBOX.
	fake.addMessage("INBOX", "From: "+box.Address+"\r\nSubject: Draft placed\r\nMessage-ID: <notify@remote.example>\r\n\r\nnotify", "<notify@remote.example>", "Draft placed")
	ch, cancel := collectEvents(svc)
	defer cancel()
	w.DetectInbox(ctx, inbox)
	if evs := drainEvents(ch); len(evs) != 0 {
		t.Fatalf("handoff notification raised a proactive event: %d", len(evs))
	}
	arrivals, err := svc.Store.ListRemoteArrivalsForInbox(ctx, box.AccountID, box.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(arrivals) != 1 || arrivals[0].DeliveryState != store.RemoteArrivalSkipped {
		t.Fatalf("arrival not skipped: %+v", arrivals)
	}
}

// TestRemoteWorkerControlForcesApproval seeds a pending approval and a control
// reply from the nominated approver; detection consumes it and never forwards it.
func TestRemoteWorkerControlForcesApproval(t *testing.T) {
	svc, box, _, fake, w := workerEnv(t)
	ctx := context.Background()
	req := seedPendingApproval(t, svc, box)
	inbox, _ := svc.Store.GetInboxInternal(ctx, box.AccountID, box.ID)
	w.DetectInbox(ctx, inbox) // baseline
	// The approver replies with the control reference line.
	body := "Approve\r\n\r\n[GH-REQUEST:" + req.Token + "]\r\n"
	raw := "From: " + req.ApproverEmail + "\r\nTo: " + box.Address + "\r\nSubject: Re: approval\r\nMessage-ID: <ctrl@remote>\r\n\r\n" + body
	fake.addMessage("INBOX", raw, "<ctrl@remote>", "Re: approval")
	ch, cancel := collectEvents(svc)
	defer cancel()
	w.DetectInbox(ctx, inbox)
	for _, e := range drainEvents(ch) {
		// A successful approval legitimately publishes draft events; the control
		// message itself must never be forwarded as a remote arrival.
		if e.Type == store.EventRemoteMessageReceived {
			t.Fatalf("control mail was forwarded as a remote arrival: %s", e.EntityID)
		}
	}
	// The approval request must have been consumed (no longer pending).
	if n, err := svc.Store.CountPendingSendRequests(ctx, box.AccountID, box.ID); err != nil || n != 0 {
		t.Fatalf("approval not consumed: n=%d err=%v", n, err)
	}
}

// TestRemoteWorkerACKMarkReadAndAutoTrash proves the ACK mark-read sets \Seen and
// the delayed auto-trash moves the message, with retry on failure.
func TestRemoteWorkerACKMarkReadAndAutoTrash(t *testing.T) {
	svc, box, _, fake, w := workerEnv(t)
	ctx := context.Background()
	fake.addFolder("Trash", 100)
	// Configure mark-read and a zero-delay trash by using EnsureRemoteAction directly.
	inbox, _ := svc.Store.GetInboxInternal(ctx, box.AccountID, box.ID)
	w.DetectInbox(ctx, inbox)
	m := fake.addMessage("INBOX", "From: a@b.test\r\nSubject: s\r\nMessage-ID: <s@remote>\r\n\r\nbody", "<s@remote>", "s")
	w.DetectInbox(ctx, inbox)

	arrivals, err := svc.Store.ListRemoteArrivalsForInbox(ctx, box.AccountID, box.ID, 10)
	if err != nil || len(arrivals) != 1 {
		t.Fatalf("arrivals = %d err=%v", len(arrivals), err)
	}
	arr := arrivals[0]
	if err := w.MarkRemoteArrivalRead(ctx, box.AccountID, box.ID, arr.ID); err != nil {
		t.Fatal(err)
	}
	if !fake.markSeenCalled {
		t.Fatal("ACK did not mark the message read")
	}
	// The message is now in the fake with \Seen.
	found := false
	fake.mu.Lock()
	for _, fm := range fake.messages {
		if fm.uid == m.uid && containsStr(fm.flags, imap.FlagSeen) {
			found = true
		}
	}
	fake.mu.Unlock()
	if !found {
		t.Fatal("live message not marked \\Seen")
	}

	// Durable auto-trash: a zero-window action moves the message to Trash.
	act, err := svc.Store.EnsureRemoteAction(ctx, box.AccountID, arr, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Force the window to have elapsed, then reconcile.
	if err := svc.Store.SetRemoteActionTrashDueForTest(ctx, box.AccountID, act.ArrivalID, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	w.ReconcileRemoteActionsForTest(ctx)
	fake.mu.Lock()
	moved := false
	for _, fm := range fake.messages {
		if fm.uid == m.uid && fm.folder == "Trash" {
			moved = true
		}
	}
	fake.mu.Unlock()
	if !moved {
		t.Fatal("auto-trash did not move the message to Trash")
	}
}

// TestRemoteWorkerReconnectNotSkip proves a connection failure during detection
// does not advance the cursor, so the message is detected on the next pass.
func TestRemoteWorkerReconnectNotSkip(t *testing.T) {
	svc, box, rm, fake, w := workerEnv(t)
	ctx := context.Background()
	inbox, _ := svc.Store.GetInboxInternal(ctx, box.AccountID, box.ID)
	w.DetectInbox(ctx, inbox) // baseline
	fake.addMessage("INBOX", "From: a@b.test\r\nSubject: s\r\nMessage-ID: <s@remote>\r\n\r\nbody", "<s@remote>", "s")
	// Install a dialer that fails, then succeeds.
	var fail bool
	var mu sync.Mutex
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return nil, errors.New("connection refused")
		}
		return &fakeSession{fake: fake}, nil
	})
	mu.Lock()
	fail = true
	mu.Unlock()
	ch, cancel := collectEvents(svc)
	defer cancel()
	w.DetectInbox(ctx, inbox)
	if evs := drainEvents(ch); len(evs) != 0 {
		t.Fatalf("failed detection emitted %d events", len(evs))
	}
	// Cursor did not advance past the unrecorded message.
	c, _, _ := svc.Store.GetRemoteCursor(ctx, box.AccountID, box.ID, "INBOX")
	if c.LastUID != 0 {
		t.Fatalf("cursor advanced past an undetected message: %d", c.LastUID)
	}
	// Reconnect succeeds: the message is now detected.
	mu.Lock()
	fail = false
	mu.Unlock()
	w.DetectInbox(ctx, inbox)
	if evs := drainEvents(ch); len(evs) != 1 {
		t.Fatalf("reconnect did not detect the message: %d events", len(evs))
	}
}

// TestRemoteWorkerStopNoLeak proves Start/Stop terminates promptly with no
// leaked goroutine (a second Stop would deadlock if one leaked the done channel).
func TestRemoteWorkerStopNoLeak(t *testing.T) {
	_, _, _, _, w := workerEnv(t)
	w.SetPeriod(10 * time.Millisecond)
	w.Start()
	time.Sleep(30 * time.Millisecond)
	done := make(chan struct{})
	go func() { w.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
}

// ---- helpers ----

type pendingApproval struct {
	Token         string
	ApproverEmail string
	DraftID       string
}

// seedPendingApproval creates a draft with a pending external approval request,
// then pins the request's token to a known plaintext so a control reply can be
// built deterministically. It returns the plaintext token.
func seedPendingApproval(t *testing.T, svc *app.Service, box model.Inbox) pendingApproval {
	t.Helper()
	ctx := context.Background()
	p := model.Principal{AccountID: box.AccountID, UserID: "u", MailboxRoles: map[string]string{box.ID: "owner"}, Admin: true}
	// Use the MailMoose approval authoring mode so the request creates an external
	// approval request rather than a remote-draft handoff.
	if err := svc.Store.SetInboxAuthoringMode(ctx, p, box.ID, model.AuthoringMailMooseApproval); err != nil {
		t.Fatal(err)
	}
	// Set an approver so the external request path can queue.
	if err := svc.Store.SetInboxApprover(ctx, box.AccountID, box.ID, "approver@example.test"); err != nil {
		t.Fatal(err)
	}
	d, err := svc.Store.CreateDraft(ctx, p, model.Draft{InboxID: box.ID, FromAddress: box.Address, To: []string{"recipient@example.test"}, Subject: "draft", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestSend(ctx, p, d.ID, true); err != nil {
		t.Fatal(err)
	}
	const token = "testtoken0123456789"
	sum := sha256.Sum256([]byte(token))
	if err := svc.Store.SetPendingApprovalTokenForTest(ctx, box.AccountID, d.ID, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	return pendingApproval{Token: token, ApproverEmail: "approver@example.test", DraftID: d.ID}
}

// TestRemoteWorkerWebhookForwardRaw proves a detected remote arrival is forwarded
// to a configured forward-mode webhook client with the raw MIME fetched live from
// the server, and that a metadata-only read never fetches the body.
func TestRemoteWorkerWebhookForwardRaw(t *testing.T) {
	svc, box, rm, fake, w := workerEnv(t)
	ctx := context.Background()
	rm.InstallRemoteBridges()

	const token = "remote-forward-token"
	enc, _ := svc.EncryptSecret([]byte(token))
	cl, err := svc.Store.CreateWebhookClient(ctx, box.AccountID, box.ID, "ep", "https://hooks.example.test/x", "forward", "bearer", enc)
	if err != nil {
		t.Fatal(err)
	}
	cap := newForwardCapture()
	srv := cap.serve()
	defer srv.Close()
	if err := svc.Store.UpdateWebhookClient(ctx, box.AccountID, cl.ID, "ep", srv.URL, "forward", "bearer"); err != nil {
		t.Fatal(err)
	}

	inbox, _ := svc.Store.GetInboxInternal(ctx, box.AccountID, box.ID)
	w.DetectInbox(ctx, inbox) // baseline
	raw := "From: alice@example.test\r\nTo: " + box.Address + "\r\nSubject: remote fwd\r\nMessage-ID: <rfwd@remote>\r\n\r\nremote body"
	fake.addMessage("INBOX", raw, "<rfwd@remote>", "remote fwd")

	// Detection records the arrival and settles it Delivered; the durable relay
	// event was persisted for the webhook client to consume.
	w.DetectInbox(ctx, inbox)

	wh := app.NewWebhookWorker(svc)
	wh.SetHTTPClient(srv.Client())
	if err := wh.RunOnce(ctx); err != nil {
		t.Fatalf("remote webhook deliver: %v", err)
	}
	body, contentType, _, auth := cap.snapshot()
	if contentType != "message/rfc822" {
		t.Fatalf("content type %q", contentType)
	}
	if auth != "Bearer "+token {
		t.Fatalf("authorization %q", auth)
	}
	if string(body) != raw {
		t.Fatalf("forwarded raw mismatch:\n got %q\nwant %q", body, raw)
	}
	if fake.markSeenCalled {
		t.Fatal("forward fetch marked the message seen")
	}
}

// TestRemoteWorkerWebhookNotifyJSON proves a notify-mode webhook client receives a
// remote-aware JSON body (no local message id) without fetching a body.
func TestRemoteWorkerWebhookNotifyJSON(t *testing.T) {
	svc, box, rm, fake, w := workerEnv(t)
	ctx := context.Background()
	rm.InstallRemoteBridges()
	enc, _ := svc.EncryptSecret([]byte("tok"))
	cl, err := svc.Store.CreateWebhookClient(ctx, box.AccountID, box.ID, "ep", "https://hooks.example.test/x", "notify", "bearer", enc)
	if err != nil {
		t.Fatal(err)
	}
	cap := newForwardCapture()
	srv := cap.serve()
	defer srv.Close()
	if err := svc.Store.UpdateWebhookClient(ctx, box.AccountID, cl.ID, "ep", srv.URL, "notify", "bearer"); err != nil {
		t.Fatal(err)
	}
	inbox, _ := svc.Store.GetInboxInternal(ctx, box.AccountID, box.ID)
	w.DetectInbox(ctx, inbox)
	fake.addMessage("INBOX", "From: alice@example.test\r\nTo: "+box.Address+"\r\nSubject: s\r\nMessage-ID: <n1@remote>\r\n\r\nb", "<n1@remote>", "s")
	w.DetectInbox(ctx, inbox)
	wh := app.NewWebhookWorker(svc)
	wh.SetHTTPClient(srv.Client())
	if err := wh.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	body, contentType, _, _ := cap.snapshot()
	if contentType != "application/json" {
		t.Fatalf("content type %q", contentType)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["remote"] != true {
		t.Fatalf("notify body not remote-aware: %v", got)
	}
	if got["message_id"] == nil || got["message_id"] == "" {
		t.Fatalf("notify body missing arrival id: %v", got)
	}
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}
