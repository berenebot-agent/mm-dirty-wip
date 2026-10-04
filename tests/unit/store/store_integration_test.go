package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/tests/support/testdb"
)

func testStore(t *testing.T) (*store.Store, model.User, model.Domain, []model.Inbox) {
	t.Helper()
	s := testdb.Open(t)
	u, err := s.CreateAccountAndAdmin(context.Background(), "Test", "admin@example.com", "correct horse battery staple", 100<<20)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDomain(context.Background(), u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	var boxes []model.Inbox
	for _, name := range []string{"owner", "assistant", "read", "other"} {
		b, err := s.CreateInbox(context.Background(), u.AccountID, d.ID, name, name)
		if err != nil {
			t.Fatal(err)
		}
		boxes = append(boxes, b)
	}
	return s, u, d, boxes
}

func inbound(box model.Inbox, delivery, rfc, inReply string, refs []string, subject, body string) store.InboundRecord {
	return store.InboundRecord{Inbox: box, Provider: "mailgun", ProviderDeliveryID: delivery, RFCMessageID: rfc, InReplyTo: inReply, References: refs,
		From: model.Address{Address: "sender@outside.test"}, To: []string{box.Address}, EnvelopeTo: []string{box.Address}, Subject: subject, Text: body,
		RawPath: "messages/test.eml", SizeBytes: 100, ReceivedAt: time.Now().UTC()}
}

func TestPermissionsThreadIsolationDedupSearchEvents(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	_, key, err := s.CreateAPIKey(ctx, u.AccountID, "mixed", false, map[string]string{b[0].ID: "owner", b[1].ID: "assistant", b[2].ID: "read"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.APIKeyPrincipal(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !p.CanOwn(b[0].ID) || !p.CanAssist(b[1].ID) || !p.CanRead(b[2].ID) || p.CanRead(b[3].ID) {
		t.Fatalf("bad roles: %#v", p.MailboxRoles)
	}
	if _, err = s.CreateDraft(ctx, p, model.Draft{InboxID: b[1].ID, Subject: "draft"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateDraft(ctx, p, model.Draft{InboxID: b[2].ID}); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("read created draft: %v", err)
	}

	m1, e1, dup, err := s.CommitInbound(ctx, inbound(b[0], "delivery-1", "<one@test>", "", nil, "Generator quote", "revised generator price"))
	if err != nil || dup {
		t.Fatalf("commit: %v dup=%v", err, dup)
	}
	mdup, _, dup, err := s.CommitInbound(ctx, inbound(b[0], "delivery-1", "<different@test>", "", nil, "different", "different"))
	if err != nil || !dup || mdup.ID != m1.ID {
		t.Fatalf("dedup failed err=%v dup=%v ids=%s/%s", err, dup, mdup.ID, m1.ID)
	}
	m2, _, _, err := s.CommitInbound(ctx, inbound(b[0], "delivery-2", "<two@test>", "<one@test>", nil, "Re: Generator quote", "reply"))
	if err != nil {
		t.Fatal(err)
	}
	if m2.ThreadID != m1.ThreadID {
		t.Fatalf("reply did not thread")
	}
	// Same forged header in another inbox must remain isolated.
	forged, _, _, err := s.CommitInbound(ctx, inbound(b[3], "delivery-3", "<evil@test>", "<one@test>", nil, "forged", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if forged.ThreadID == m1.ThreadID {
		t.Fatal("cross-inbox thread injection")
	}

	got, err := s.SearchMessages(ctx, p, "generator price", b[0].ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != m1.ID {
		t.Fatalf("search got %#v", got)
	}
	if _, err = s.SearchMessages(ctx, p, "forged", b[3].ID, 20); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("search scope: %v", err)
	}
	evs, err := s.ListEvents(ctx, p, 0, b[0].ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) < 2 || evs[0].ID != e1.ID {
		t.Fatalf("events %#v", evs)
	}
	replay, err := s.ListEvents(ctx, p, e1.ID, b[0].ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(replay) < 1 || replay[0].ID <= e1.ID {
		t.Fatalf("event replay %#v", replay)
	}
}

// TestInboundReplyThreadsViaProviderMessageIDFallback proves a reply that
// references a provider's wire Message-ID joins the outbound thread even for a
// row persisted before the id was promoted to rfc_message_id.
func TestInboundReplyThreadsViaProviderMessageIDFallback(t *testing.T) {
	ctx := context.Background()
	s, _, _, b := testStore(t)
	box := b[0]
	m, _, err := s.CommitOutbound(ctx, store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<synth@example.com>", ProviderMessageID: "<legacy-wire@relay.sendinblue.com>", From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "s", Text: "t", RawPath: "messages/o.eml", SizeBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	reply, _, _, err := s.CommitInbound(ctx, inbound(box, "reply-legacy", "<reply@outside.test>", "", []string{"<legacy-wire@relay.sendinblue.com>"}, "Re: s", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if reply.ThreadID != m.ThreadID {
		t.Fatalf("reply thread %q, want %q", reply.ThreadID, m.ThreadID)
	}
}

func TestSQLiteConcurrentWritesSerialized(t *testing.T) {
	ctx := context.Background()
	s, u, d, _ := testStore(t)
	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.CreateInbox(ctx, u.AccountID, d.ID, fmt.Sprintf("box%d", i), "")
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestQuotaEnforcementInboundAndOutbound(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	box := b[0]
	// Shrink the account quota so a single message exceeds it.
	db := rawDB(t, s.Path())
	if _, err := db.ExecContext(ctx, `UPDATE accounts SET storage_quota_bytes=10 WHERE id=?`, u.AccountID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rec := inbound(box, "quota-1", "<quota@test>", "", nil, "Big", "x")
	rec.SizeBytes = 100
	if _, _, _, err := s.CommitInbound(ctx, rec); !errors.Is(err, store.ErrQuota) {
		t.Fatalf("inbound quota: %v", err)
	}
	// Outbound quota.
	if _, _, err := s.CommitOutbound(ctx, store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<out@test>", From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "s", Text: "t", RawPath: "messages/o.eml", SizeBytes: 100, SentAt: time.Now().UTC()}); !errors.Is(err, store.ErrQuota) {
		t.Fatalf("outbound quota: %v", err)
	}
}

func TestPurgeDomainCleansStorageAndFiles(t *testing.T) {
	ctx := context.Background()
	s, u, d, b := testStore(t)
	box := b[0]
	// Commit a message with a raw path and size.
	rec := inbound(box, "purge-1", "<purge@test>", "", nil, "Purge", "body")
	rec.RawPath = "messages/purge.eml"
	rec.SizeBytes = 50
	if _, _, _, err := s.CommitInbound(ctx, rec); err != nil {
		t.Fatal(err)
	}
	acc, _ := s.GetAccount(ctx, u.AccountID)
	if acc.StorageUsedBytes != 50 {
		t.Fatalf("storage used = %d, want 50", acc.StorageUsedBytes)
	}
	paths, err := s.PurgeDomain(ctx, u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "messages/purge.eml" {
		t.Fatalf("paths %#v", paths)
	}
	acc, _ = s.GetAccount(ctx, u.AccountID)
	if acc.StorageUsedBytes != 0 {
		t.Fatalf("storage used after purge = %d, want 0", acc.StorageUsedBytes)
	}
	// FTS rows gone.
	got, err := s.SearchMessages(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, "purge", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("search after purge returned %#v", got)
	}
	// Domain gone.
	domains, err := s.ListDomains(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if len(domains) != 0 {
		t.Fatalf("domains after purge = %#v", domains)
	}
}

func TestIdempotencyReserveIsAtomic(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	// First caller wins the reservation.
	claimed, _, err := s.IdempotencyReserve(ctx, u.AccountID, "key-1", b[0].ID)
	if err != nil || !claimed {
		t.Fatalf("first reserve claimed=%v err=%v", claimed, err)
	}
	// Second concurrent caller must get a conflict (in-flight).
	claimed, _, err = s.IdempotencyReserve(ctx, u.AccountID, "key-1", b[0].ID)
	if err == nil || !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second reserve err=%v", err)
	}
	// Enqueueing the message completes the key in the same transaction.
	m, _, err := s.CommitOutbound(ctx, store.OutboundRecord{Inbox: b[0], Provider: "smtp", RFCMessageID: "<idem@test>",
		From: model.Address{Address: b[0].Address}, To: []string{"friend@example.net"}, Subject: "s", Text: "b",
		RawPath: "messages/idem.eml", SizeBytes: 10, IdemKey: "key-1"})
	if err != nil {
		t.Fatal(err)
	}
	// A new reserve now replays the existing message.
	claimed, mid, err := s.IdempotencyReserve(ctx, u.AccountID, "key-1", b[0].ID)
	if err != nil || claimed || mid != m.ID {
		t.Fatalf("completed reserve claimed=%v mid=%q err=%v", claimed, mid, err)
	}
	// Release a pending reservation so a failed send can retry.
	if err = s.IdempotencyRelease(ctx, u.AccountID, "key-2"); err != nil {
		t.Fatal(err)
	}
	claimed, _, err = s.IdempotencyReserve(ctx, u.AccountID, "key-2", b[0].ID)
	if err != nil || !claimed {
		t.Fatalf("release+reserve claimed=%v err=%v", claimed, err)
	}
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateAccountAndAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", 100<<20)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.CreateDomain(context.Background(), u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateInbox(context.Background(), u.AccountID, d.ID, "hermes", "Hermes")
	if err != nil {
		t.Fatal(err)
	}
	rec := inbound(b, "bk-1", "<bk@test>", "", nil, "Backup", "restore me")
	rec.RawPath = "messages/bk.eml"
	if _, _, _, err = s.CommitInbound(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen the same directory (simulating restore onto a clean instance).
	s2, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.SearchMessages(context.Background(), model.Principal{AccountID: u.AccountID, Admin: true}, "restore", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Subject != "Backup" {
		t.Fatalf("restored search %#v", got)
	}
}
