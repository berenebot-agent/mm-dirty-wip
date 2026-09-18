package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// TestInboxAliasCrossDomainIngest proves an alias on one domain delivers into
// an inbox on another domain of the same account, and that retries dedup.
func TestInboxAliasCrossDomainIngest(t *testing.T) {
	svc, u, _, box := testService(t)
	ctx := context.Background()

	aliasDomain, err := svc.Store.CreateDomain(ctx, u.AccountID, "other.com")
	if err != nil {
		t.Fatal(err)
	}
	// The alias's own domain carries the receiving provider that authenticates
	// the delivery; the target inbox lives on a different domain.
	seedInbound(t, svc, u.AccountID, aliasDomain.ID, "cloudflare", map[string]any{"webhook_secret": testCFSecret})
	if err = svc.Store.SetInboxAliases(ctx, u.AccountID, box.ID, []store.AliasInput{{DomainID: aliasDomain.ID, LocalPart: "sales"}}); err != nil {
		t.Fatal(err)
	}

	raw := "From: Sender <sender@outside.test>\r\nTo: sales@other.com\r\nSubject: via alias\r\nMessage-ID: <alias@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nalias body"
	m, dup, err := svc.IngestInbound(ctx, "cloudflare", cfRequest(t, testCFSecret, "alias-1", "sales@other.com", raw))
	if err != nil || dup {
		t.Fatalf("alias ingest %v dup=%v", err, dup)
	}
	if m.InboxID != box.ID {
		t.Fatalf("delivered to inbox %s, want target %s", m.InboxID, box.ID)
	}
	if len(m.To) != 1 || m.To[0] != "sales@other.com" {
		t.Fatalf("alias recipient not preserved: %#v", m.To)
	}

	// Retrying the same provider delivery collapses.
	m2, dup, err := svc.IngestInbound(ctx, "cloudflare", cfRequest(t, testCFSecret, "alias-1", "sales@other.com", raw))
	if err != nil || !dup || m2.ID != m.ID {
		t.Fatalf("alias dedup err=%v dup=%v ids=%s/%s", err, dup, m2.ID, m.ID)
	}
}

// TestInboxAliasDisabledTargetUnrouted proves a disabled target inbox is not a
// delivery destination: the alias is treated as unrouted.
func TestInboxAliasDisabledTargetUnrouted(t *testing.T) {
	svc, u, dom, box := testService(t)
	ctx := context.Background()
	seedInbound(t, svc, u.AccountID, dom.ID, "cloudflare", map[string]any{"webhook_secret": testCFSecret})
	if err := svc.Store.SetInboxAliases(ctx, u.AccountID, box.ID, []store.AliasInput{{DomainID: dom.ID, LocalPart: "sales"}}); err != nil {
		t.Fatal(err)
	}
	disabled := false
	if err := svc.Store.UpdateInbox(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, box.ID, "", &disabled); err != nil {
		t.Fatal(err)
	}
	raw := "From: Sender <sender@outside.test>\r\nTo: sales@example.com\r\nSubject: no\r\nMessage-ID: <off@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nbody"
	if _, _, err := svc.IngestInbound(ctx, "cloudflare", cfRequest(t, testCFSecret, "off-1", "sales@example.com", raw)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("disabled target err=%v", err)
	}
}
