package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

func TestDomainConfigCAS(t *testing.T) {
	ctx := context.Background()
	s, u, d, _ := testStore(t)

	first, err := s.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "brevo", "enc-1", store.ConfigVersion{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 || first.ID == "" {
		t.Fatalf("create %+v", first)
	}
	// Create-only conflicts when a row already exists.
	if _, err := s.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "smtp", "enc-2", store.ConfigVersion{}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second create err=%v, want conflict", err)
	}
	// Correct version updates in place, even across a provider swap.
	second, err := s.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "smtp", "enc-2", store.ConfigVersion{ID: first.ID, Revision: first.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Revision != 2 || second.Provider != "smtp" || second.EncryptedConfig != "enc-2" {
		t.Fatalf("update %+v", second)
	}
	// A stale revision conflicts and does not overwrite.
	if _, err := s.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "resend", "enc-3", store.ConfigVersion{ID: first.ID, Revision: 1}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale revision err=%v, want conflict", err)
	}
	got, err := s.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if err != nil || got.Revision != 2 || got.Provider != "smtp" {
		t.Fatalf("after conflict %+v err=%v", got, err)
	}
	// A mismatched id conflicts.
	if _, err := s.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "resend", "enc-4", store.ConfigVersion{ID: "dsc_other", Revision: 2}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("wrong id err=%v, want conflict", err)
	}
	// Deleting is idempotent for an existing domain.
	if err = s.DeleteDomainSendingConfig(ctx, u.AccountID, d.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteDomainSendingConfig(ctx, u.AccountID, d.ID); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := s.GetDomainSendingConfig(ctx, u.AccountID, d.ID); !errors.Is(err, store.ErrNoProvider) {
		t.Fatalf("after delete err=%v, want ErrNoProvider", err)
	}
	// A delete against a missing domain is ErrNotFound.
	if err = s.DeleteDomainSendingConfig(ctx, u.AccountID, "dom_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete missing domain err=%v, want not found", err)
	}
}

func TestDomainConfigAccountIsolation(t *testing.T) {
	ctx := context.Background()
	s, u, d, boxes := testStore(t)
	ub, err := s.CreateAccountAndAdmin(ctx, "B", "admin@b.test", "correct horse battery staple", 100<<20)
	if err != nil {
		t.Fatal(err)
	}
	db, err := s.CreateDomain(ctx, ub.AccountID, "isolated.test")
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := s.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "brevo", "secret-A", store.ConfigVersion{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "resend", "recv-A", store.ConfigVersion{}); err != nil {
		t.Fatal(err)
	}

	// Account B cannot read, write or delete account A's domain config.
	if _, err = s.GetDomainSendingConfig(ctx, ub.AccountID, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-account get err=%v", err)
	}
	if _, err = s.SaveDomainSendingConfig(ctx, ub.AccountID, d.ID, "smtp", "x", store.ConfigVersion{ID: cfg.ID, Revision: 1}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-account save err=%v", err)
	}
	if err = s.DeleteDomainSendingConfig(ctx, ub.AccountID, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-account delete err=%v", err)
	}
	// A foreign account cannot configure its own domain using A's config id:
	// the CAS token matches no row for B's domain, so the save conflicts.
	if _, err = s.SaveDomainSendingConfig(ctx, ub.AccountID, db.ID, "smtp", "x", store.ConfigVersion{ID: cfg.ID, Revision: 1}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("foreign config id err=%v", err)
	}
	// Account A's config is untouched.
	still, err := s.GetDomainSendingConfig(ctx, u.AccountID, d.ID)
	if err != nil || still.ID != cfg.ID || still.EncryptedConfig != "secret-A" || still.Revision != 1 {
		t.Fatalf("account A config changed %+v err=%v", still, err)
	}

	// Delivery history is account-scoped.
	if _, _, err = s.CommitOutbound(ctx, store.OutboundRecord{Inbox: boxes[0], Provider: "brevo", RFCMessageID: "<iso@test>", From: model.Address{Address: boxes[0].Address}, To: []string{"x@y.test"}, Subject: "s", Text: "t", RawPath: "messages/iso.eml", SizeBytes: 10}); err != nil {
		t.Fatal(err)
	}
	sent := func(account, domain string) (map[string]time.Time, error) { return s.LastSentByDomain(ctx, account) }
	lastA, err := sent(u.AccountID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := lastA[d.ID]; ok {
		t.Fatal("unexpected sent history before any successful send")
	}
	lastB, err := s.LastSentByDomain(ctx, ub.AccountID)
	if err != nil || len(lastB) != 0 {
		t.Fatalf("account B history leaked %v err=%v", lastB, err)
	}
	// Cross-account domain history listing is not found.
	if _, err = s.ListDomainDeliveryAttempts(ctx, ub.AccountID, d.ID, 10, 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-account history err=%v", err)
	}
}

func TestDeliveryHistorySurvivesConfigAndDomainDeletes(t *testing.T) {
	ctx := context.Background()
	s, u, d, boxes := testStore(t)
	box := boxes[0]
	if _, err := s.SaveDomainSendingConfig(ctx, u.AccountID, d.ID, "brevo", "enc", store.ConfigVersion{}); err != nil {
		t.Fatal(err)
	}
	m, _, err := s.CommitOutbound(ctx, store.OutboundRecord{Inbox: box, Provider: "brevo", RFCMessageID: "<h@test>", From: model.Address{Address: box.Address}, To: []string{"x@y.test"}, Subject: "s", Text: "t", RawPath: "messages/h.eml", SizeBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.MarkSent(ctx, u.AccountID, m.ID, "<pid>", "brevo"); err != nil {
		t.Fatal(err)
	}

	// Deleting the sending config must not hide the history.
	if err = s.DeleteDomainSendingConfig(ctx, u.AccountID, d.ID); err != nil {
		t.Fatal(err)
	}
	attempts, err := s.ListDomainDeliveryAttempts(ctx, u.AccountID, d.ID, 10, 0)
	if err != nil || len(attempts) != 1 || attempts[0].MessageID != m.ID {
		t.Fatalf("history after config delete %+v err=%v", attempts, err)
	}
	// LastSentByDomain is likewise independent of the current config.
	last, err := s.LastSentByDomain(ctx, u.AccountID)
	if err != nil || last[d.ID].IsZero() {
		t.Fatalf("last sent after config delete %v err=%v", last, err)
	}

	// RecordDeliveryAttempt validates/derives the domain.
	if err = s.RecordDeliveryAttempt(ctx, store.DeliveryAttempt{AccountID: u.AccountID, MessageID: m.ID, Provider: "brevo", Attempt: 2, Status: "sent"}); err != nil {
		t.Fatal(err)
	}
	attempts, err = s.ListDomainDeliveryAttempts(ctx, u.AccountID, d.ID, 10, 0)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("derived attempt not recorded %+v err=%v", attempts, err)
	}
	if err = s.RecordDeliveryAttempt(ctx, store.DeliveryAttempt{AccountID: u.AccountID, DomainID: "dom_missing", Provider: "brevo", Attempt: 1, Status: "failed"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign domain attempt err=%v", err)
	}

	// Deleting the message clears the link but keeps the rows attributed.
	p := model.Principal{AccountID: u.AccountID, Admin: true, MailboxRoles: map[string]string{box.ID: "owner"}}
	if _, _, _, err = s.DeleteMessage(ctx, p, m.ID); err != nil {
		t.Fatal(err)
	}
	attempts, err = s.ListDomainDeliveryAttempts(ctx, u.AccountID, d.ID, 10, 0)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("history after message delete %+v err=%v", attempts, err)
	}
	for _, a := range attempts {
		if a.MessageID != "" || a.DomainID != d.ID {
			t.Fatalf("attempt after message delete %+v", a)
		}
	}

	// A receiving config still exists at this point; purging the domain must
	// cascade it away.
	if _, err = s.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "resend", "recv", store.ConfigVersion{}); err != nil {
		t.Fatal(err)
	}
	// Purging the domain keeps every attempt, with domain_id and message_id
	// nulled by their foreign keys.
	if _, err = s.PurgeDomain(ctx, u.AccountID, d.ID); err != nil {
		t.Fatal(err)
	}
	db := rawDB(t, s.Path())
	defer db.Close()
	var n int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM domain_receiving_configs WHERE domain_id=?`, d.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("config not cascaded on domain purge: %d", n)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM outbound_delivery_log WHERE account_id=?`, u.AccountID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("attempts after domain purge = %d, want 2", n)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM outbound_delivery_log WHERE account_id=? AND domain_id IS NULL AND message_id IS NULL`, u.AccountID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("attempts not nulled after domain purge: %d", n)
	}
}
