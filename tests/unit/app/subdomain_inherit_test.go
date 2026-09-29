package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport"
)

// A subdomain inherits the parent's Cloudflare receiver: the single generated
// Worker secret accepts mail addressed to an inbox on the subdomain, and the
// message lands in that subdomain's inbox, scoped to the authenticated account.
func TestCloudflareIngestSubdomainInheritsParentReceiver(t *testing.T) {
	svc, u, dom, box := testService(t)
	seedInbound(t, svc, u.AccountID, dom.ID, "cloudflare", map[string]any{"webhook_secret": testCFSecret})
	ctx := context.Background()

	sub, err := svc.Store.CreateDomain(ctx, u.AccountID, "agent.example.com")
	if err != nil {
		t.Fatal(err)
	}
	subBox, err := svc.Store.CreateInbox(ctx, u.AccountID, sub.ID, "agent", "Agent")
	if err != nil {
		t.Fatal(err)
	}
	// A second address that only the subdomain's own catch-all can serve.
	catchAll, err := svc.Store.CreateInbox(ctx, u.AccountID, sub.ID, "catchall", "Catch all")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetDomainCatchAll(ctx, u.AccountID, sub.ID, catchAll.ID); err != nil {
		t.Fatal(err)
	}

	now := time.Now().Format(time.RFC1123Z)
	raw := "From: Sender <sender@outside.test>\r\nTo: agent@agent.example.com\r\nSubject: to subdomain\r\nMessage-ID: <sub-inbound@test>\r\nDate: " + now + "\r\n\r\nsub body"
	m, dup, err := svc.IngestInbound(ctx, "cloudflare", cfRequest(t, testCFSecret, "sub-1", subBox.Address, raw))
	if err != nil || dup {
		t.Fatalf("subdomain ingest %v dup=%v", err, dup)
	}
	if m.InboxID != subBox.ID || m.Subject != "to subdomain" {
		t.Fatalf("message routed to wrong inbox: %+v", m)
	}

	// The subdomain's own catch-all handles any local part, still using the
	// parent's connector.
	raw2 := "From: Sender <sender@outside.test>\r\nTo: anyone@agent.example.com\r\nSubject: catch all\r\nMessage-ID: <sub-inbound-2@test>\r\nDate: " + now + "\r\n\r\nsub body 2"
	m2, dup, err := svc.IngestInbound(ctx, "cloudflare", cfRequest(t, testCFSecret, "sub-2", "anyone@agent.example.com", raw2))
	if err != nil || dup {
		t.Fatalf("subdomain catch-all ingest %v dup=%v", err, dup)
	}
	if m2.InboxID != catchAll.ID {
		t.Fatalf("catch-all delivery to %s, want %s", m2.InboxID, catchAll.ID)
	}

	// The parent's Worker secret is still valid for the parent domain, and an
	// unknown provider/config is still rejected.
	if _, _, err := svc.IngestInbound(ctx, "cloudflare", cfRequest(t, testCFSecret, "parent-1", box.Address, raw)); err != nil {
		t.Fatalf("parent ingest after subdomain: %v", err)
	}
	if _, _, err := svc.IngestInbound(ctx, "cloudflare", cfRequest(t, "wrong", "x", subBox.Address, raw)); !errors.Is(err, transport.ErrInboundUnauthorized) {
		t.Fatalf("wrong secret on subdomain err=%v, want unauthorized", err)
	}
}

// With receiving inheritance turned off, the subdomain has no receiver of its
// own, so mail to it is rejected (404/unauthorized) rather than silently
// inheriting.
func TestCloudflareIngestSubdomainInheritanceOffRejects(t *testing.T) {
	svc, u, dom, _ := testService(t)
	seedInbound(t, svc, u.AccountID, dom.ID, "cloudflare", map[string]any{"webhook_secret": testCFSecret})
	ctx := context.Background()
	sub, err := svc.Store.CreateDomainWithOptions(ctx, u.AccountID, "agent.example.com", store.DomainCreateOptions{DisableReceiving: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.CreateInbox(ctx, u.AccountID, sub.ID, "agent", "Agent"); err != nil {
		t.Fatal(err)
	}
	raw := "From: x@y.test\r\nTo: agent@agent.example.com\r\nSubject: s\r\nMessage-ID: <off@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nbody"
	if _, _, err := svc.IngestInbound(ctx, "cloudflare", cfRequest(t, testCFSecret, "off-1", "agent@agent.example.com", raw)); !errors.Is(err, transport.ErrInboundUnauthorized) {
		t.Fatalf("inheritance off err=%v, want unauthorized", err)
	}
}

// A resolved inbox on the subdomain but authenticated under the parent's
// credential must still be scoped to the subdomain (the binding's DomainID),
// which the inheritance path preserves.
func TestSubdomainBindingKeepsDomainScope(t *testing.T) {
	svc, u, dom, _ := testService(t)
	seedInbound(t, svc, u.AccountID, dom.ID, "cloudflare", map[string]any{"webhook_secret": testCFSecret})
	ctx := context.Background()
	sub, err := svc.Store.CreateDomain(ctx, u.AccountID, "agent.example.com")
	if err != nil {
		t.Fatal(err)
	}
	binding, err := svc.ResolveInboundBinding(ctx, "cloudflare", "x@agent.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if binding.DomainID != sub.ID || binding.AccountID != u.AccountID {
		t.Fatalf("binding scope %+v, want subdomain %s", binding, sub.ID)
	}
}
