package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func TestTerminalFailureOverQuotaCommitsOutcome(t *testing.T) {
	t.Parallel()
	for _, cap := range []string{"account", "inbox", "both"} {
		t.Run(cap, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s, u, _, boxes := testStore(t)
			box := boxes[0]
			p := model.Principal{AccountID: u.AccountID, Admin: true}
			m := outboundForFailure(t, s, box)
			db := rawDB(t, s.Path())
			defer db.Close()
			if cap != "inbox" {
				if _, err := db.ExecContext(ctx, `UPDATE accounts SET storage_quota_bytes=storage_used_bytes WHERE id=?`, u.AccountID); err != nil {
					t.Fatal(err)
				}
			}
			if cap != "account" {
				if _, err := db.ExecContext(ctx, `UPDATE inboxes SET storage_quota_bytes=storage_used_bytes WHERE id=?`, box.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.BeginMessageSend(ctx, u.AccountID, m.ID, "attempt"); err != nil {
				t.Fatal(err)
			}
			failed, events, err := s.MarkFailed(ctx, u.AccountID, m.ID, "550 rejected", time.Time{}, 1, "mx", "attempt")
			if err != nil || failed.Status != "failed" || failed.LastError != "550 rejected" || len(events) != 2 {
				t.Fatalf("terminal outcome: %+v, events=%+v, err=%v", failed, events, err)
			}
			if inFlight, err := s.MessageSendInFlight(ctx, u.AccountID, m.ID); err != nil || inFlight {
				t.Fatalf("pre-send marker remains: %v, %v", inFlight, err)
			}
			if _, _, err := s.MarkFailed(ctx, u.AccountID, m.ID, "550 rejected", time.Time{}, 1, "mx", "attempt"); err != nil {
				t.Fatal(err)
			}
			notices, err := s.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID, Direction: "inbound"})
			if err != nil || len(notices) != 1 {
				t.Fatalf("notices: %+v, %v", notices, err)
			}
			want := m.SizeBytes + notices[0].SizeBytes
			acc, err := s.GetAccount(ctx, u.AccountID)
			if err != nil || acc.StorageUsedBytes != want {
				t.Fatalf("account accounting: %+v, want %d, err=%v", acc, want, err)
			}
			updated, err := s.GetInbox(ctx, p, box.ID)
			if err != nil || updated.StorageUsedBytes != want {
				t.Fatalf("inbox accounting: %+v, want %d, err=%v", updated, want, err)
			}
			if _, _, err := s.CommitOutbound(ctx, store.OutboundRecord{Inbox: box, To: []string{"x@y.test"}, SizeBytes: 1}); !errors.Is(err, store.ErrQuota) {
				t.Fatalf("ordinary sends bypass quota: %v", err)
			}
		})
	}
}

// outboundForFailure enqueues a normal outbound message From the inbox's own
// address, so a terminal failure has a well-defined sender to notify.
func outboundForFailure(t *testing.T, s *store.Store, box model.Inbox) model.Message {
	t.Helper()
	rec := store.OutboundRecord{
		Inbox:        box,
		Provider:     "mx",
		RFCMessageID: "<fail@example.com>",
		From:         model.Address{Name: "Owner", Address: box.Address},
		To:           []string{"recipient@microsoft.test"},
		Subject:      "Quarterly numbers",
		Text:         "body",
		RawPath:      "messages/o.eml",
		SizeBytes:    100,
	}
	m, _, err := s.CommitOutbound(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestTerminalFailureWritesNoticeAndClearsOutbox proves the core behaviour: a
// terminal outbound failure drops a delivery-failure notice into the inbox as a
// normal inbound message, clears the failed original from the outbox, and emits
// the events a live consumer needs. This is the behaviour that makes a terminal
// failure obvious to a human (and an agent reading the mailbox) instead of
// leaving it silently parked in the outbox queue.
func TestTerminalFailureWritesNoticeAndClearsOutbox(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	m := outboundForFailure(t, s, box)

	// maxAttempts 1 makes the first failure terminal, exactly as a permanent MX
	// 5xx does on the send path.
	failed, events, err := s.MarkFailed(ctx, u.AccountID, m.ID, "Direct MX returned SMTP 550: recipient rejected", time.Time{}, 1, "mx", "")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "failed" {
		t.Fatalf("status = %q, want failed", failed.Status)
	}

	// The original has left the outbox queue.
	out, err := s.ListOutbox(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, box.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range out {
		if o.ID == m.ID {
			t.Fatalf("failed message still in outbox: %+v", o)
		}
	}

	// A notice landed in the inbox, addressed to the failed message's sender.
	msgs, err := s.ListMessages(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, store.MessageFilter{InboxID: box.ID, Direction: "inbound"})
	if err != nil {
		t.Fatal(err)
	}
	var notice model.Message
	found := false
	for _, msg := range msgs {
		if strings.HasPrefix(msg.Subject, "Undeliverable:") {
			notice, found = msg, true
			break
		}
	}
	if !found {
		t.Fatalf("no delivery-failure notice in inbox; got %d inbound messages", len(msgs))
	}
	if notice.From.Address != "MAILER-DAEMON@example.com" {
		t.Fatalf("notice from = %q, want MAILER-DAEMON@example.com", notice.From.Address)
	}
	if notice.Subject != "Undeliverable: Quarterly numbers" {
		t.Fatalf("notice subject = %q", notice.Subject)
	}
	if len(notice.To) != 1 || notice.To[0] != box.Address {
		t.Fatalf("notice to = %v, want [%s]", notice.To, box.Address)
	}
	if !strings.Contains(notice.Text, "recipient@microsoft.test") {
		t.Fatalf("notice body missing recipient: %q", notice.Text)
	}
	if !strings.Contains(notice.Text, m.ID) {
		t.Fatalf("notice body missing original message id: %q", notice.Text)
	}

	// Events: the notice arrival plus the original being cleared.
	var received, trashed bool
	for _, ev := range events {
		switch ev.Type {
		case "message.received":
			received = true
		case "message.trashed":
			trashed = true
			if ev.EntityID != m.ID {
				t.Fatalf("trashed event entity = %q, want %q", ev.EntityID, m.ID)
			}
		}
	}
	if !received || !trashed {
		t.Fatalf("events received=%v trashed=%v, want both", received, trashed)
	}
}

// TestRetryableFailureWritesNoNotice proves a retryable failure is untouched:
// the message stays queued in the outbox, no notice is written, and no
// delivery-failure event is emitted. Only a terminal failure is surfaced.
func TestRetryableFailureWritesNoNotice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	m := outboundForFailure(t, s, box)

	// Attempt 1 of 6 -> rescheduled, not terminal.
	failed, events, err := s.MarkFailed(ctx, u.AccountID, m.ID, "temporary MX failure", time.Now().UTC().Add(time.Minute), 6, "mx", "")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "pending" {
		t.Fatalf("status = %q, want pending", failed.Status)
	}
	// Still queued in the outbox.
	out, err := s.ListOutbox(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, box.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].ID != m.ID {
		t.Fatalf("outbox = %+v, want the single pending message", out)
	}
	// No notice.
	msgs, err := s.ListMessages(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, store.MessageFilter{InboxID: box.ID, Direction: "inbound"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("retryable failure wrote %d inbound messages, want 0", len(msgs))
	}
	if len(events) != 0 {
		t.Fatalf("retryable failure emitted %d events, want 0", len(events))
	}
}

// TestTerminalFailureNoticeIsNotDuplicated proves a second terminal mark on the
// same message does not write a second notice, so a re-marked failure cannot
// spam the mailbox.
func TestTerminalFailureNoticeIsNotDuplicated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	m := outboundForFailure(t, s, box)

	if _, _, err := s.MarkFailed(ctx, u.AccountID, m.ID, "550 rejected", time.Time{}, 1, "mx", ""); err != nil {
		t.Fatal(err)
	}
	// A second terminal mark (e.g. a duplicated worker outcome) must be inert.
	if _, _, err := s.MarkFailed(ctx, u.AccountID, m.ID, "550 rejected", time.Time{}, 1, "mx", ""); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.ListMessages(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, store.MessageFilter{InboxID: box.ID, Direction: "inbound"})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, msg := range msgs {
		if strings.HasPrefix(msg.Subject, "Undeliverable:") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("got %d delivery-failure notices, want exactly 1", n)
	}
}

// TestTerminalFailureNoticeFallbackSenderDomain proves a message sent from a
// standalone inbox (no managed domain) still produces a notice from the
// non-routable fallback domain rather than emitting an empty sender.
func TestTerminalFailureNoticeFallbackSenderDomain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, u, _, _ := testStore(t)
	box, err := s.CreateStandaloneInbox(ctx, u.AccountID, store.StandaloneCreate{
		DisplayName: "Agent Inbox",
		Address:     "agent@standalone.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	m := outboundForFailure(t, s, box)

	if _, _, err := s.MarkFailed(ctx, u.AccountID, m.ID, "550 blocked", time.Time{}, 1, "mx", ""); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.ListMessages(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, store.MessageFilter{InboxID: box.ID, Direction: "inbound"})
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range msgs {
		if strings.HasPrefix(msg.Subject, "Undeliverable:") {
			if msg.From.Address != "MAILER-DAEMON@mailer-daemon.invalid" {
				t.Fatalf("fallback notice from = %q", msg.From.Address)
			}
			return
		}
	}
	t.Fatal("no delivery-failure notice written")
}
