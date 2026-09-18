package app_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/transport"
	"github.com/dellarb/mailmoose/internal/transport/cloudflare"
)

func cfRequest(t *testing.T, secret, deliveryID, recipient, raw string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("POST", "/internal/ingest/cloudflare", strings.NewReader(raw))
	r.Header.Set("Content-Type", "message/rfc822")
	r.Header.Set(cloudflare.HeaderRecipient, recipient)
	r.Header.Set(cloudflare.HeaderEnvelopeTo, "sender@outside.test")
	if deliveryID != "" {
		r.Header.Set(cloudflare.HeaderDeliveryID, deliveryID)
	}
	if secret != "" {
		r.Header.Set("Authorization", "Bearer "+secret)
	}
	return r
}

func TestCloudflareIngestAndDedup(t *testing.T) {
	svc, u, dom, box := testService(t)
	seedInbound(t, svc, u.AccountID, dom.ID, "cloudflare", map[string]any{"webhook_secret": testCFSecret})
	ctx := context.Background()
	raw := "From: Sender <sender@outside.test>\r\nTo: hermes@example.com\r\nSubject: via cloudflare\r\nMessage-ID: <cf-inbound@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\ncloudflare body"
	m, dup, err := svc.IngestInbound(ctx, "cloudflare", cfRequest(t, testCFSecret, "cf-1", box.Address, raw))
	if err != nil || dup {
		t.Fatalf("ingest %v dup=%v", err, dup)
	}
	if m.Provider != "cloudflare" || m.Subject != "via cloudflare" {
		t.Fatalf("%+v", m)
	}
	m2, dup, err := svc.IngestInbound(ctx, "cloudflare", cfRequest(t, testCFSecret, "cf-1", box.Address, raw))
	if err != nil || !dup || m2.ID != m.ID {
		t.Fatalf("dedup %v dup=%v", err, dup)
	}
}

func TestInboundUnknownProviderAndUnauthorized(t *testing.T) {
	svc, u, dom, box := testService(t)
	seedInbound(t, svc, u.AccountID, dom.ID, "cloudflare", map[string]any{"webhook_secret": testCFSecret})
	ctx := context.Background()
	raw := "From: x@y.test\r\nTo: hermes@example.com\r\nSubject: nope\r\n\r\nhi"
	if _, _, err := svc.IngestInbound(ctx, "nope", cfRequest(t, testCFSecret, "x", box.Address, raw)); !errors.Is(err, transport.ErrUnknownProvider) {
		t.Fatalf("unknown provider err=%v", err)
	}
	if _, _, err := svc.IngestInbound(ctx, "cloudflare", cfRequest(t, "wrong", "x", box.Address, raw)); !errors.Is(err, transport.ErrInboundUnauthorized) {
		t.Fatalf("unauthorized err=%v", err)
	}
}

func TestInboundAllowedSenderWildcard(t *testing.T) {
	svc, u, dom, box := testService(t)
	seedInbound(t, svc, u.AccountID, dom.ID, "cloudflare", map[string]any{"webhook_secret": testCFSecret})
	ctx := context.Background()
	if err := svc.Store.SetInboxAllowedSenders(ctx, u.AccountID, box.ID, []string{"*@allowed.test", "*@*.corp.test"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetInboxSenderRestricted(ctx, u.AccountID, box.ID, true); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Format(time.RFC1123Z)
	cases := []struct {
		from    string
		blocked bool
	}{
		{"someone@allowed.test", false},
		{"anyone@deep.sub.corp.test", false},
		{"intruder@other.test", true},
		{"apex@corp.test", true},
	}
	for i, tc := range cases {
		raw := "From: " + tc.from + "\r\nTo: hermes@example.com\r\nSubject: s\r\nMessage-ID: <w" + strconv.Itoa(i) + "@test>\r\nDate: " + now + "\r\n\r\nbody"
		m, dup, err := svc.IngestInbound(ctx, "cloudflare", cfRequest(t, testCFSecret, "wild-"+strconv.Itoa(i), box.Address, raw))
		if err != nil || dup {
			t.Fatalf("case %d ingest err=%v dup=%v", i, err, dup)
		}
		if m.Blocked != tc.blocked {
			t.Fatalf("case %d from=%s blocked=%v want %v", i, tc.from, m.Blocked, tc.blocked)
		}
	}
}

func TestMailgunCanonicalRouteIngests(t *testing.T) {
	svc, u, dom, box := testService(t)
	seedInbound(t, svc, u.AccountID, dom.ID, "mailgun", map[string]any{"signing_key": testMailgunKey})
	ctx := context.Background()
	raw := "From: Sender <sender@outside.test>\r\nTo: hermes@example.com\r\nSubject: Hello\r\nMessage-ID: <inbound-compat@test>\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n\r\nPlease reply"
	m, dup, err := svc.IngestInbound(ctx, "mailgun", mgRequest(t, testMailgunKey, "compat-1", box.Address, raw))
	if err != nil || dup {
		t.Fatalf("ingest %v dup=%v", err, dup)
	}
	if m.Provider != "mailgun" {
		t.Fatalf("provider %q", m.Provider)
	}
}
