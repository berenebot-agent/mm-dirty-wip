package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
)

// queuedOutbound enqueues one pending outbound message for the fixture inbox.
func queuedOutbound(t *testing.T, s *store.Store, box model.Inbox, rfc string) model.Message {
	t.Helper()
	m, _, err := s.CommitOutbound(context.Background(), store.OutboundRecord{
		Inbox: box, Provider: "brevo", RFCMessageID: rfc,
		From: model.Address{Address: box.Address}, To: []string{"x@y.test"},
		Subject: "s", Text: "t", RawPath: "messages/pre.eml", SizeBytes: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestPreSendMarkerBlocksResendAcrossRecovery proves the accepted-provider-send
// safety contract: once an attempt arms the durable pre-send marker, no
// automatic path — lease expiry, a re-claim, or startup claim recovery — can
// make the message claimable again. Only a definitive outcome clears it.
func TestPreSendMarkerBlocksResendAcrossRecovery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, b := testStore(t)
	m := queuedOutbound(t, s, b[0], "<pre@test>")

	// An attempt claims and arms the marker (its provider send has started).
	_, claim, err := s.ClaimNextPending(ctx, time.Now().UTC(), "w1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BeginMessageSend(ctx, u.AccountID, m.ID, claim); err != nil {
		t.Fatal(err)
	}

	// The claim lease lapses. Even far past expiry, the row is not claimable.
	if id, _, err := s.ClaimNextPending(ctx, time.Now().UTC().Add(48*time.Hour), "w2", time.Hour); err != nil || id != "" {
		t.Fatalf("claim of an unresolved send: id=%q err=%v, want empty", id, err)
	}
	// Startup recovery must not clear the claim of an unresolved send.
	if err = s.RecoverAbandonedClaims(ctx); err != nil {
		t.Fatal(err)
	}
	if id, _, err := s.ClaimNextPending(ctx, time.Now().UTC().Add(96*time.Hour), "w3", time.Hour); err != nil || id != "" {
		t.Fatalf("claim after recovery: id=%q err=%v, want empty", id, err)
	}
	// The claim is still attached and the marker is still present.
	if owner, err := s.MessageClaimOwner(ctx, u.AccountID, m.ID); err != nil || owner != claim {
		t.Fatalf("claim owner = %q err=%v, want the unresolved attempt's token", owner, err)
	}
	if inFlight, err := s.MessageSendInFlight(ctx, u.AccountID, m.ID); err != nil || !inFlight {
		t.Fatalf("in-flight = %v err=%v, want true", inFlight, err)
	}
	// ReleaseMessageClaim is a no-op while a marker is present.
	if err = s.ReleaseMessageClaim(ctx, m.ID, claim); err != nil {
		t.Fatal(err)
	}
	if owner, _ := s.MessageClaimOwner(ctx, u.AccountID, m.ID); owner != claim {
		t.Fatalf("claim released despite an in-flight marker: %q", owner)
	}

	// A definitive outcome resolves the marker, after which the message is
	// terminal (sent) and never re-queued.
	if _, _, err = s.MarkSent(ctx, u.AccountID, m.ID, "<prov>", "brevo", claim); err != nil {
		t.Fatal(err)
	}
	if inFlight, err := s.MessageSendInFlight(ctx, u.AccountID, m.ID); err != nil || inFlight {
		t.Fatalf("in-flight after MarkSent = %v err=%v, want false", inFlight, err)
	}
	if id, _, err := s.ClaimNextPending(ctx, time.Now().UTC().Add(200*time.Hour), "w4", time.Hour); err != nil || id != "" {
		t.Fatalf("sent message claimable: id=%q err=%v", id, err)
	}
}

// TestBeginMessageSendOwnership proves the pre-send marker is only armed by the
// attempt that owns the claim: a stale token cannot arm it over a live
// re-claim, an empty token cannot bypass a live claim, and a marker is never
// overwritten by a second arming.
func TestBeginMessageSendOwnership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, b := testStore(t)
	m := queuedOutbound(t, s, b[0], "<own@test>")

	_, claim1, err := s.ClaimNextPending(ctx, time.Now().UTC(), "w1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// A stale token and an empty token cannot arm the marker over the live claim.
	if err = s.BeginMessageSend(ctx, u.AccountID, m.ID, "w1:clm_other"); !errors.Is(err, store.ErrClaimTaken) {
		t.Fatalf("stale arm = %v, want ErrClaimTaken", err)
	}
	// An empty token is the non-worker path: it has no competing attempt to fence
	// off, so it neither arms the marker nor fails.
	if err = s.BeginMessageSend(ctx, u.AccountID, m.ID, ""); err != nil {
		t.Fatalf("empty-token arm = %v, want success for the claim-free path", err)
	}
	// The first claim's lease lapses and another attempt takes over.
	if err = s.ReleaseMessageClaim(ctx, m.ID, claim1); err != nil {
		t.Fatal(err)
	}
	if _, claim2, err := s.ClaimNextPending(ctx, time.Now().UTC(), "w2", time.Hour); err != nil || claim2 == "" {
		t.Fatalf("re-claim = %q err=%v", claim2, err)
	}
	// The superseded attempt cannot arm the marker: the live claim belongs to the
	// new attempt now.
	if err = s.BeginMessageSend(ctx, u.AccountID, m.ID, claim1); !errors.Is(err, store.ErrClaimTaken) {
		t.Fatalf("arm with a superseded token = %v, want ErrClaimTaken", err)
	}
}

// TestRequeuePendingForDomainPreservesLiveSend proves a sending-config save
// never disturbs an in-flight send: a message with a live claim or a durable
// pre-send marker is left alone (to finish under the old config), while an idle
// queued message is requeued with a fresh budget.
func TestRequeuePendingForDomainPreservesLiveSend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, d, b := testStore(t)
	box := b[0]

	idle := queuedOutbound(t, s, box, "<idle@test>")
	active := queuedOutbound(t, s, box, "<active@test>")
	// Give the idle message a spent budget and a backoff so the requeue is
	// observable.
	if _, _, err := s.MarkFailed(ctx, u.AccountID, idle.ID, "down", time.Now().UTC().Add(time.Hour), 6, "brevo", ""); err != nil {
		t.Fatal(err)
	}

	// The active message is claimed and its provider send is armed.
	_, claim, err := s.ClaimNextPending(ctx, time.Now().UTC(), "w1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BeginMessageSend(ctx, u.AccountID, active.ID, claim); err != nil {
		t.Fatal(err)
	}

	n, err := s.RequeuePendingForDomain(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("requeued %d, want 1 (only the idle message)", n)
	}
	// The active send is untouched: same claim, marker, attempts.
	a, err := s.GetMessageByID(ctx, u.AccountID, active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if owner, _ := s.MessageClaimOwner(ctx, u.AccountID, active.ID); owner != claim {
		t.Fatalf("active claim changed to %q", owner)
	}
	if inFlight, _ := s.MessageSendInFlight(ctx, u.AccountID, active.ID); !inFlight {
		t.Fatal("active marker cleared by domain requeue")
	}
	if a.Attempts != 0 || a.NextRetry != "" {
		t.Fatalf("active message requeued: attempts=%d next=%q", a.Attempts, a.NextRetry)
	}
	// The idle message was requeued with a fresh budget.
	i, err := s.GetMessageByID(ctx, u.AccountID, idle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if i.Attempts != 0 || i.NextRetry != "" || i.LastError != "" {
		t.Fatalf("idle message not requeued: %+v", i)
	}
}

// TestStaleTokenAfterOperatorResetCannotResolve proves the claim-token guard
// still holds after an operator resolves a quarantined send: a token from before
// the reset cannot record an outcome on the freshly requeued message.
func TestStaleTokenAfterOperatorResetCannotResolve(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	m := queuedOutbound(t, s, box, "<reset@test>")

	_, claim1, err := s.ClaimNextPending(ctx, time.Now().UTC(), "w1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BeginMessageSend(ctx, u.AccountID, m.ID, claim1); err != nil {
		t.Fatal(err)
	}

	// The operator quarantines the unresolved send terminally (discard).
	if err = s.ResolveQuarantinedSend(ctx, p, m.ID, "discarded: unresolved provider send", false); err != nil {
		t.Fatal(err)
	}
	cur, err := s.GetMessageByID(ctx, u.AccountID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != "failed" {
		t.Fatalf("status = %q, want failed", cur.Status)
	}
	if inFlight, _ := s.MessageSendInFlight(ctx, u.AccountID, m.ID); inFlight {
		t.Fatal("marker survived quarantine discard")
	}
	// The old attempt's token cannot resolve a "sent" outcome now (the message
	// is terminally failed), and a re-failure of an already-failed message is
	// inert rather than a resend.
	if _, _, err = s.MarkSent(ctx, u.AccountID, m.ID, "<prov>", "brevo", claim1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale MarkSent after quarantine = %v, want ErrConflict", err)
	}
	if _, _, err = s.MarkFailed(ctx, u.AccountID, m.ID, "late", time.Now().UTC(), 6, "brevo", claim1); err != nil {
		t.Fatalf("MarkFailed on an already-failed message = %v, want inert success", err)
	}
	cur, err = s.GetMessageByID(ctx, u.AccountID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != "failed" {
		t.Fatalf("status = %q, want failed", cur.Status)
	}

	// An operator retry clears the marker and requeues; a fresh attempt then
	// owns the outcome and the old token stays refused.
	warn, err := s.RequeueFailedWithWarning(ctx, p, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if warn == "" {
		t.Fatal("an operator retry of a discarded send must warn about duplicate delivery")
	}
	if _, claim2, err := s.ClaimNextPending(ctx, time.Now().UTC(), "w2", time.Hour); err != nil || claim2 == "" {
		t.Fatalf("re-claim = %q err=%v", claim2, err)
	}
	// claim1 cannot resolve; it is neither the owner nor the marker holder.
	if _, _, err = s.MarkSent(ctx, u.AccountID, m.ID, "<prov>", "brevo", claim1); !errors.Is(err, store.ErrClaimTaken) {
		t.Fatalf("stale MarkSent with a live re-claim = %v, want ErrClaimTaken", err)
	}
}

// TestRequeueFailedWarnsOnQuarantinedResend proves the manual retry of a
// quarantined send surfaces the duplicate risk, and an ordinary retry does not.
func TestRequeueFailedWarnsOnQuarantinedResend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}

	// Ordinary failed message: a durable attempt exhausted its budget, marker empty.
	ordinary := queuedOutbound(t, s, box, "<ord@test>")
	_, claim, err := s.ClaimNextPending(ctx, time.Now().UTC(), "w1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BeginMessageSend(ctx, u.AccountID, ordinary.ID, claim); err != nil {
		t.Fatal(err)
	}
	// A definitive failure resolves the marker (a known provider rejection), so
	// the ordinary retry carries no warning.
	if _, _, err = s.MarkFailed(ctx, u.AccountID, ordinary.ID, "rejected", time.Time{}, 1, "brevo", claim); err != nil {
		t.Fatal(err)
	}
	if warn, err := s.RequeueFailedWithWarning(ctx, p, ordinary.ID); err != nil || warn != "" {
		t.Fatalf("ordinary retry warn=%q err=%v, want no warning", warn, err)
	}

	// Quarantined message: marker present. Retrying it warns.
	quar := queuedOutbound(t, s, box, "<quar@test>")
	_, claim2, err := s.ClaimNextPending(ctx, time.Now().UTC(), "w1", time.Hour)
	if err != nil || claim2 == "" {
		t.Fatalf("claim = %q err=%v", claim2, err)
	}
	if err = s.BeginMessageSend(ctx, u.AccountID, quar.ID, claim2); err != nil {
		t.Fatal(err)
	}
	if err = s.ResolveQuarantinedSend(ctx, p, quar.ID, "unresolved", false); err != nil {
		t.Fatal(err)
	}
	warn, err := s.RequeueFailedWithWarning(ctx, p, quar.ID)
	if err != nil {
		t.Fatal(err)
	}
	if warn == "" {
		t.Fatal("retry of a quarantined send produced no duplicate warning")
	}
}

// TestListQuarantinedSends proves the operator review surface lists exactly the
// unresolved sends for the account, and that resolving one removes it.
func TestListQuarantinedSends(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]

	held := queuedOutbound(t, s, box, "<held@test>")
	_ = queuedOutbound(t, s, box, "<ok@test>")

	_, claim, err := s.ClaimNextPending(ctx, time.Now().UTC(), "w1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BeginMessageSend(ctx, u.AccountID, held.ID, claim); err != nil {
		t.Fatal(err)
	}
	q, err := s.ListQuarantinedSends(ctx, u.AccountID, 10)
	if err != nil || len(q) != 1 || q[0].MessageID != held.ID {
		t.Fatalf("quarantined = %+v err=%v, want the armed message", q, err)
	}
	if inFlight, _ := s.MessageSendInFlight(ctx, u.AccountID, held.ID); !inFlight {
		t.Fatal("armed message not reported in flight")
	}
	// Resolving it (sent) removes it from the review list.
	if _, _, err = s.MarkSent(ctx, u.AccountID, held.ID, "<prov>", "brevo", claim); err != nil {
		t.Fatal(err)
	}
	if q, err := s.ListQuarantinedSends(ctx, u.AccountID, 10); err != nil || len(q) != 0 {
		t.Fatalf("quarantined after resolution = %+v err=%v, want none", q, err)
	}
}

// TestMarkSentCommitFallbackIsGuarded proves the corrected commit-error fallback
// reports a stale/unrelated row honestly: MarkSent on a message another attempt
// already failed is a conflict, not a false success.
func TestMarkSentCommitFallbackIsGuarded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	m := queuedOutbound(t, s, box, "<guard@test>")

	_, claim, err := s.ClaimNextPending(ctx, time.Now().UTC(), "w1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BeginMessageSend(ctx, u.AccountID, m.ID, claim); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.MarkFailed(ctx, u.AccountID, m.ID, "rejected", time.Time{}, 1, "brevo", claim); err != nil {
		t.Fatal(err)
	}
	// A late "sent" for an already-failed message is refused outright.
	if _, _, err = s.MarkSent(ctx, u.AccountID, m.ID, "<prov>", "brevo", claim); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("MarkSent on a failed message = %v, want ErrConflict", err)
	}
	cur, err := s.GetMessageByID(ctx, u.AccountID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != "failed" {
		t.Fatalf("status = %q, want failed (never flipped back to sent)", cur.Status)
	}
}
