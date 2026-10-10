package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// standaloneBox creates a standalone inbox owned by the account.
func standaloneBox(t *testing.T, s *store.Store, accountID, address string) model.Inbox {
	t.Helper()
	b, err := s.CreateStandaloneInbox(context.Background(), accountID, store.StandaloneCreate{
		DisplayName: "Agent", Address: address,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAuthoringModeDefaultsByKind(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	domain := b[0]
	standalone := standaloneBox(t, s, u.AccountID, "agent@remote.example")

	own := owner(domain, u.AccountID)
	// Domain default: MailMoose approval.
	ds, err := s.GetInboxAuthoringSettings(ctx, own, domain.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ds.Mode != model.AuthoringMailMooseApproval {
		t.Fatalf("domain default mode=%q", ds.Mode)
	}
	// Standalone default: RemoteDraft, with the connected address as notify.
	ownS := owner(standalone, u.AccountID)
	ss, err := s.GetInboxAuthoringSettings(ctx, ownS, standalone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ss.Mode != model.AuthoringRemoteDraft {
		t.Fatalf("standalone default mode=%q", ss.Mode)
	}
	if ss.NotifyAddress != standalone.Address || ss.NotifyOverridden {
		t.Fatalf("standalone notify=%q overridden=%v", ss.NotifyAddress, ss.NotifyOverridden)
	}
	// An explicit override is honoured and cleared.
	if err = s.SetInboxAuthoringMode(ctx, ownS, standalone.ID, model.AuthoringMailMooseApproval); err != nil {
		t.Fatal(err)
	}
	if err = s.SetInboxNotifyAddress(ctx, ownS, standalone.ID, "notify@example.com"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetInboxAuthoringSettings(ctx, ownS, standalone.ID)
	if got.Mode != model.AuthoringMailMooseApproval || got.NotifyAddress != "notify@example.com" || !got.NotifyOverridden {
		t.Fatalf("override settings=%+v", got)
	}
	if err = s.SetInboxNotifyAddress(ctx, ownS, standalone.ID, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetInboxAuthoringSettings(ctx, ownS, standalone.ID)
	if got.NotifyAddress != standalone.Address || got.NotifyOverridden {
		t.Fatalf("cleared notify=%+v", got)
	}
	// A non-owner cannot change the mode.
	asst := assistant(standalone, u.AccountID)
	if err = s.SetInboxAuthoringMode(ctx, asst, standalone.ID, model.AuthoringRemoteDraft); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("assistant set mode err=%v", err)
	}
}

func TestCreateAssistantHandlingFreezesAndSettles(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	asst := assistant(box, u.AccountID)

	d, err := s.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	created, events, err := s.CreateAssistantHandling(ctx, asst, store.AssistantHandlingInsert{
		DraftID: d.ID, InboxID: box.ID, Mode: model.AuthoringRemoteDraft,
		ContentHash: "hash", HandoffID: "hnd_1", MessageID: "<m@remote.example>",
		RemoteFolder: "Drafts", RawPath: "handoff/aa/bb/x.eml", SizeBytes: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Publication != model.HandoffPending || created.NotificationStatus != model.NotificationNone {
		t.Fatalf("created=%+v", created)
	}
	if len(events) != 1 || events[0].Type != model.EventDraftHandoffRequested {
		t.Fatalf("events=%+v", events)
	}
	// The draft is frozen.
	stored, err := s.GetDraft(ctx, asst, d.ID)
	if err != nil || stored.Status != model.DraftStatusPendingApproval {
		t.Fatalf("frozen draft=%+v err=%v", stored, err)
	}
	// The handling record is retrievable by handoff id and message id.
	if r, err := s.HandoffByHandoffID(ctx, u.AccountID, "hnd_1"); err != nil || r.ID != created.ID {
		t.Fatalf("by handoff id %+v err=%v", r, err)
	}
	if r, err := s.HandoffByMessageID(ctx, u.AccountID, "<m@remote.example>"); err != nil || r.ID != created.ID {
		t.Fatalf("by message id %+v err=%v", r, err)
	}
	// A second handoff for the same draft conflicts.
	if _, _, err = s.CreateAssistantHandling(ctx, asst, store.AssistantHandlingInsert{DraftID: d.ID, InboxID: box.ID, Mode: model.AuthoringRemoteDraft}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second handoff err=%v", err)
	}

	// Claim, confirm publication, and idempotently re-confirm.
	claimed, ok, err := s.ClaimNextHandoff(ctx, "wk")
	if err != nil || !ok || claimed.ID != created.ID || claimed.Attempts != 1 {
		t.Fatalf("claim %+v ok=%v err=%v", claimed, ok, err)
	}
	ev, err := s.MarkHandoffPublished(ctx, u.AccountID, created.ID, 77)
	if err != nil || ev == nil || ev.Type != model.EventDraftHandoffPublished {
		t.Fatalf("publish ev=%+v err=%v", ev, err)
	}
	if again, err := s.MarkHandoffPublished(ctx, u.AccountID, created.ID, 78); err != nil || again != nil {
		t.Fatalf("idempotent publish ev=%+v err=%v", again, err)
	}
	got, _ := s.GetAssistantHandlingInternal(ctx, u.AccountID, created.ID)
	if got.Publication != model.HandoffPublished || got.RemoteUID != 77 || got.PublishedAt == nil {
		t.Fatalf("published record=%+v", got)
	}

	// Cleanup consumes the local draft and retains the handoff state.
	if err = s.CleanupPublishedHandoffLocalDraft(ctx, u.AccountID, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.GetDraft(ctx, asst, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("draft not consumed: %v", err)
	}
	got, _ = s.GetAssistantHandlingInternal(ctx, u.AccountID, created.ID)
	if got.Publication != model.HandoffPublished {
		t.Fatalf("handoff state lost: %+v", got)
	}
	// Cleanup is idempotent.
	if err = s.CleanupPublishedHandoffLocalDraft(ctx, u.AccountID, created.ID); err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
}

func TestHandoffAmbiguousIsExplicitAndNotRetried(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	asst := assistant(box, u.AccountID)
	d, _ := s.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "b"})
	created, _, err := s.CreateAssistantHandling(ctx, asst, store.AssistantHandlingInsert{
		DraftID: d.ID, InboxID: box.ID, Mode: model.AuthoringRemoteDraft,
		ContentHash: "h", HandoffID: "hnd_amb", MessageID: "<amb@remote.example>", RemoteFolder: "Drafts",
	})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := s.MarkHandoffAmbiguous(ctx, u.AccountID, created.ID, "no appenduid")
	if err != nil || ev == nil || ev.Type != model.EventDraftHandoffAmbiguous {
		t.Fatalf("ambiguous ev=%+v err=%v", ev, err)
	}
	got, _ := s.GetAssistantHandlingInternal(ctx, u.AccountID, created.ID)
	if got.Publication != model.HandoffAmbiguous || got.LastError != "no appenduid" {
		t.Fatalf("ambiguous record=%+v", got)
	}
	// It is no longer pending, so the worker does not retry it.
	if _, ok, _ := s.ClaimNextHandoff(ctx, "wk"); ok {
		t.Fatal("ambiguous handoff was re-claimed")
	}
	// It may still be resolved to Published by a later verified lookup, but a
	// second ambiguous settle is a no-op (it is not pending).
	if again, err := s.MarkHandoffAmbiguous(ctx, u.AccountID, created.ID, "again"); err != nil || again != nil {
		t.Fatalf("second ambiguous ev=%+v err=%v", again, err)
	}
	pub, err := s.MarkHandoffPublished(ctx, u.AccountID, created.ID, 9)
	if err != nil || pub == nil {
		t.Fatalf("resolve ambiguous->published ev=%+v err=%v", pub, err)
	}
}

func TestHandoffNotificationIndependentOfPublication(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	asst := assistant(box, u.AccountID)
	d, _ := s.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "b"})
	created, _, err := s.CreateAssistantHandling(ctx, asst, store.AssistantHandlingInsert{
		DraftID: d.ID, InboxID: box.ID, Mode: model.AuthoringRemoteDraft,
		ContentHash: "h", HandoffID: "hnd_n", MessageID: "<n@remote.example>", RemoteFolder: "Drafts",
	})
	if err != nil {
		t.Fatal(err)
	}
	// A failed notification does not affect the handoff's publication state.
	ev, err := s.MarkHandoffNotificationFailed(ctx, u.AccountID, created.ID, "no provider")
	if err != nil || ev == nil || ev.Type != model.EventDraftHandoffNotificationFailed {
		t.Fatalf("notif failed ev=%+v err=%v", ev, err)
	}
	got, _ := s.GetAssistantHandlingInternal(ctx, u.AccountID, created.ID)
	if got.NotificationStatus != model.NotificationFailed || got.Publication != model.HandoffPending {
		t.Fatalf("states=%+v", got)
	}
	// Publication still succeeds independently.
	if _, err = s.MarkHandoffPublished(ctx, u.AccountID, created.ID, 1); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetAssistantHandlingInternal(ctx, u.AccountID, created.ID)
	if got.Publication != model.HandoffPublished || got.NotificationStatus != model.NotificationFailed {
		t.Fatalf("independent states=%+v", got)
	}
	// Notifications never carry a token: the workflow job kind is the handoff kind.
	wf, err := s.CommitHandoffNotification(ctx, u.AccountID, got, store.WorkflowRecord{
		Inbox: box, From: model.Address{Address: box.Address}, To: []string{box.Address},
		Subject: "Draft placed", Text: "x", RawPath: "handoff/n.eml", SizeBytes: 1,
	}, "", "", "<notify@remote.example>")
	if err != nil || wf == "" {
		t.Fatalf("commit notification wf=%q err=%v", wf, err)
	}
	id, err := s.HandoffNotificationWorkflowID(ctx, u.AccountID, created.ID)
	if err != nil || id != wf {
		t.Fatalf("workflow id=%q want %q err=%v", id, wf, err)
	}
	// The notification's Message-ID is durable, so a notification delivered back
	// into the inbox is excluded by a lookup, and the frozen draft's Message-ID is
	// still resolvable separately.
	byNotify, err := s.HandoffByNotificationMessageID(ctx, u.AccountID, "<notify@remote.example>")
	if err != nil || byNotify.ID != created.ID {
		t.Fatalf("HandoffByNotificationMessageID = %+v err=%v", byNotify, err)
	}
	if _, err := s.HandoffByNotificationMessageID(ctx, u.AccountID, "<n@remote.example>"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("frozen draft id wrongly matched notification: %v", err)
	}
	if byDraft, err := s.HandoffByMessageID(ctx, u.AccountID, "<n@remote.example>"); err != nil || byDraft.ID != created.ID {
		t.Fatalf("HandoffByMessageID = %+v err=%v", byDraft, err)
	}
}

func TestHandoffCancelUnfreezes(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	asst := assistant(box, u.AccountID)
	d, _ := s.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "b"})
	_, _, err := s.CreateAssistantHandling(ctx, asst, store.AssistantHandlingInsert{
		DraftID: d.ID, InboxID: box.ID, Mode: model.AuthoringRemoteDraft,
		ContentHash: "h", HandoffID: "hnd_c", MessageID: "<c@remote.example>", RemoteFolder: "Drafts",
	})
	if err != nil {
		t.Fatal(err)
	}
	r, ev, err := s.CancelHandoff(ctx, asst, d.ID)
	if err != nil || ev.Type != model.EventDraftHandoffCancelled {
		t.Fatalf("cancel r=%+v ev=%+v err=%v", r, ev, err)
	}
	got, _ := s.GetDraft(ctx, asst, d.ID)
	if got.Status != model.DraftStatusDraft {
		t.Fatalf("draft not unfrozen: %q", got.Status)
	}
}

func TestStandaloneSendingTargetResolves(t *testing.T) {
	ctx := context.Background()
	s, u, _, _ := testStore(t)
	box := standaloneBox(t, s, u.AccountID, "send@remote.example")
	asst := assistant(box, u.AccountID)
	// A standalone draft resolves the sender to the inbox's own address, with no
	// managed domain.
	d, err := s.CreateDraft(ctx, asst, model.Draft{InboxID: box.ID, To: []string{"x@y.test"}, Subject: "s", Text: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if d.FromAddress != box.Address {
		t.Fatalf("from=%q want %q", d.FromAddress, box.Address)
	}
	from, target, err := s.ResolveSendingTarget(ctx, u.AccountID, box.ID, "")
	if err != nil || from.Address != box.Address {
		t.Fatalf("resolve target from=%+v err=%v", from, err)
	}
	if target.DomainID != "" || target.InboxID != box.ID {
		t.Fatalf("standalone target=%+v want inbox-scoped, no domain", target)
	}
	// A different sender is not allowed on a standalone inbox.
	if _, _, err := s.ResolveSendingTarget(ctx, u.AccountID, box.ID, "other@remote.example"); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("foreign sender err=%v want forbidden", err)
	}
	// With no standalone sender bridge, sending config for the standalone inbox is
	// a clean ErrNoProvider (queued/held), never a domain FK error.
	if _, err := s.SendingConfigForTarget(ctx, u.AccountID, box.ID, target); !errors.Is(err, store.ErrNoProvider) {
		t.Fatalf("standalone sending config err=%v want ErrNoProvider", err)
	}
}
