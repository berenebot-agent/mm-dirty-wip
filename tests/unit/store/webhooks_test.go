package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/auth"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestMigration032MigratesLegacyClients upgrades a pre-032 database and proves
// existing API keys and Hermes connections keep working through the unified
// client tables.
func TestMigration032MigratesLegacyClients(t *testing.T) {
	dir := newV012(t)
	seedV012(t, dir)
	db := rawDB(t, filepath.Join(dir, "inbox.db"))
	legacy := []string{
		`INSERT INTO api_keys(id,account_id,name,key_prefix,key_hash,is_admin,created_at) VALUES('keyOld','acc1','Legacy','mmm_legacy',?,0,'2026-01-01T00:00:00Z')`,
		`INSERT INTO api_key_mailbox_roles(api_key_id,inbox_id,role) VALUES('keyOld','inbA','owner')`,
		`INSERT INTO hermes_connections(id,account_id,inbox_id,name,gateway_id,secret_encrypted,delivery_key_encrypted,last_ack_event_id,created_at) VALUES('hrmOld','acc1','inbA','H','gw-old','sec','del',3,'2026-01-01T00:00:00Z')`,
	}
	if _, err := db.ExecContext(context.Background(), legacy[0], auth.HashToken("mmm_legacy")); err != nil {
		t.Fatal(err)
	}
	for _, s := range legacy[1:] {
		if _, err := db.ExecContext(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	defer st.Close()

	p, err := st.APIKeyPrincipal(context.Background(), "mmm_legacy")
	if err != nil {
		t.Fatalf("legacy key auth after upgrade: %v", err)
	}
	if p.APIKeyID != "keyOld" || p.AccountID != "acc1" || p.MailboxRoles["inbA"] != "owner" {
		t.Fatalf("migrated key principal %#v", p)
	}

	h, err := st.GetHermesConnectionByGateway(context.Background(), "gw-old")
	if err != nil {
		t.Fatalf("legacy hermes after upgrade: %v", err)
	}
	if h.ID != "hrmOld" || h.InboxID != "inbA" || h.SecretEncrypted != "sec" || h.LastAckEventID != 3 {
		t.Fatalf("migrated hermes %#v", h)
	}
}

func TestWebhookClientLifecycle(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)

	c, err := s.CreateWebhookClient(ctx, u.AccountID, b[0].ID, "ep", "https://hooks.example.test/x", "notify", "signature", "enc-1")
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != "notify" || c.AuthMode != "signature" || !c.Enabled || c.ID == "" {
		t.Fatalf("created client %#v", c)
	}
	got, err := s.GetWebhookClient(ctx, u.AccountID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SecretEncrypted != "enc-1" || got.URL != "https://hooks.example.test/x" {
		t.Fatalf("get %#v", got)
	}
	list, err := s.ListWebhookClients(ctx, u.AccountID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list %v %#v", err, list)
	}

	if err := s.UpdateWebhookClient(ctx, u.AccountID, c.ID, "ep2", "https://hooks.example.test/y", "forward", "bearer"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetWebhookClient(ctx, u.AccountID, c.ID)
	if got.Name != "ep2" || got.URL != "https://hooks.example.test/y" || got.Mode != "forward" || got.AuthMode != "bearer" {
		t.Fatalf("after update %#v", got)
	}

	if err := s.RotateWebhookSecret(ctx, u.AccountID, c.ID, "enc-2"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetWebhookClient(ctx, u.AccountID, c.ID)
	if got.SecretEncrypted != "enc-2" {
		t.Fatalf("secret not rotated %#v", got)
	}

	if err := s.SetWebhookEnabled(ctx, u.AccountID, c.ID, false); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetWebhookClient(ctx, u.AccountID, c.ID)
	if got.Enabled {
		t.Fatal("client should be paused")
	}

	// Another account cannot see or mutate the client.
	ub, err := s.CreateAccountAndAdmin(ctx, "B", "b@example.com", "correct horse battery staple", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWebhookClient(ctx, ub.AccountID, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-account get err=%v, want not found", err)
	}

	if err := s.DeleteWebhookClient(ctx, u.AccountID, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWebhookClient(ctx, u.AccountID, c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted client still visible: %v", err)
	}
}

func TestWebhookDeliverySchedulesInOrder(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	c, err := s.CreateWebhookClient(ctx, u.AccountID, b[0].ID, "ep", "https://hooks.example.test/x", "notify", "signature", "enc")
	if err != nil {
		t.Fatal(err)
	}
	_, e1, _, err := s.CommitInbound(ctx, inbound(b[0], "d1", "<w1@test>", "", nil, "one", "body one"))
	if err != nil {
		t.Fatal(err)
	}
	_, e2, _, err := s.CommitInbound(ctx, inbound(b[0], "d2", "<w2@test>", "", nil, "two", "body two"))
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	first, err := s.NextWebhookDelivery(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.EventID != e1.ID || first.Type != "message.received" {
		t.Fatalf("first delivery %#v want event %d", first, e1.ID)
	}

	// A failed attempt that is not yet due must block the queue: the newer
	// event is not delivered ahead of the older one.
	if err := s.RecordWebhookDelivery(ctx, c.ID, e1.ID, false, "boom", now.Add(time.Hour), now.Add(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NextWebhookDelivery(ctx, now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("head-of-line blocked err=%v, want not found", err)
	}

	// Once due, the same event is retried.
	due, err := s.NextWebhookDelivery(ctx, now.Add(2*time.Hour))
	if err != nil || due.EventID != e1.ID {
		t.Fatalf("retry due %v %#v", err, due)
	}

	// A success advances the cursor to the next event.
	if err := s.RecordWebhookDelivery(ctx, c.ID, e1.ID, true, "", now, now.Add(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	second, err := s.NextWebhookDelivery(ctx, now)
	if err != nil || second.EventID != e2.ID {
		t.Fatalf("second delivery %v %#v", err, second)
	}
	if second.Client.LastAckEventID != e1.ID {
		t.Fatalf("ack cursor %d, want %d", second.Client.LastAckEventID, e1.ID)
	}
}

func TestWebhookDeliveryExhaustionAdvances(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	c, err := s.CreateWebhookClient(ctx, u.AccountID, b[0].ID, "ep", "https://hooks.example.test/x", "notify", "signature", "enc")
	if err != nil {
		t.Fatal(err)
	}
	_, e1, _, err := s.CommitInbound(ctx, inbound(b[0], "d1", "<x1@test>", "", nil, "one", "body"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// A retry scheduled past the window is terminal: the event is marked
	// failed and the cursor advances so the queue is not blocked forever.
	if err := s.RecordWebhookDelivery(ctx, c.ID, e1.ID, false, "gone", now.Add(48*time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NextWebhookDelivery(ctx, now.Add(72*time.Hour)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("exhausted delivery still queued: %v", err)
	}
	got, err := s.GetWebhookClient(ctx, u.AccountID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastAckEventID != e1.ID || got.LastError != "gone" {
		t.Fatalf("terminal state %#v", got)
	}
}
