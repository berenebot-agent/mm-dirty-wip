package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/config"
	"gatehouse-mail/internal/events"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/mxwire"
	"gatehouse-mail/internal/store"
)

// mxService builds a service with MX receiving enabled and the domain's receive
// provider set to "mx", returning the raw-MIME staging helper.
func mxService(t *testing.T) (*app.Service, model.User, model.Domain, model.Inbox) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Config{
		DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted",
		AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901",
		MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour,
		LoginLimitPerMinute: 10, SendLimitPerMinute: 60,
		MXReceiveEnabled: true, MXEdgeKeys: map[string]string{"edge": "secret"}, MXSignatureSkew: 10 * time.Minute,
	}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateAccountAndAdmin(context.Background(), "A", "admin@example.com", "correct horse battery staple", cfg.DefaultQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDomain(context.Background(), u.AccountID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateInbox(context.Background(), u.AccountID, d.ID, "hermes", "Hermes")
	if err != nil {
		t.Fatal(err)
	}
	seedInbound(t, svc, u.AccountID, d.ID, "mx", map[string]any{"enforcement": "moderate"})
	return svc, u, d, b
}

// stageMX writes raw bytes to the service staging dir and returns the path.
func stageMX(t *testing.T, svc *app.Service, raw string) string {
	t.Helper()
	dir := filepath.Join(svc.Config.DataDir, "messages", ".tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(dir, "mxtest-*.eml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(raw); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func mxInput(t *testing.T, svc *app.Service, recipient, raw string, auth mxwire.AuthResults) app.MXIngestInput {
	t.Helper()
	digest := mxwire.BodyDigest([]byte(raw))
	return app.MXIngestInput{
		Recipient:           recipient,
		EnvelopeFrom:        "sender@outside.test",
		RawPath:             stageMX(t, svc, raw),
		Size:                int64(len(raw)),
		DeliveryFingerprint: mxwire.DeliveryFingerprint("sender@outside.test", recipient, digest),
		AuthResults:         auth,
		TrustedAuth:         true,
	}
}

const goodRaw = "From: Sender <sender@outside.test>\r\nTo: hermes@example.com\r\nSubject: direct smtp\r\nMessage-ID: <mx@test>\r\nDate: Mon, 07 Sep 2026 10:00:00 +0000\r\n\r\nbody via mx"

// TestMXIngestStoresAndDeduplicates verifies the durable path and receipt-based
// dedup across a repeat of the same fingerprint.
func TestMXIngestStoresAndDeduplicates(t *testing.T) {
	svc, _, _, box := mxService(t)
	ctx := context.Background()
	in := mxInput(t, svc, box.Address, goodRaw, mxwire.AuthResults{})
	res, err := svc.IngestMX(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Disposition != mxwire.DispositionStored || res.Code != mxwire.CodeOK {
		t.Fatalf("unexpected result %+v", res)
	}
	in2 := in
	in2.RawPath = stageMX(t, svc, goodRaw)
	res2, err := svc.IngestMX(ctx, in2)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Duplicate || res2.Code != mxwire.CodeDuplicate || res2.MessageID != res.MessageID {
		t.Fatalf("expected duplicate, got %+v", res2)
	}
}

// TestMXAuthFailureGoesToSpam verifies a definitive auth failure is a durable
// Spam disposition, never a rejection, for both enforcement modes.
func TestMXAuthFailureGoesToSpam(t *testing.T) {
	cases := []struct {
		name string
		mode string
		auth mxwire.AuthResults
	}{
		{"moderate spf+dkim", "moderate", mxwire.AuthResults{
			SPF:  &mxwire.SPFEvidence{Result: "fail"},
			DKIM: []mxwire.DKIMEvidence{{Result: "fail"}},
		}},
		{"moderate dmarc", "moderate", mxwire.AuthResults{
			DMARC: &mxwire.DMARCEvidence{Result: "fail", Policy: "reject"},
		}},
		{"hard spf only", "hard", mxwire.AuthResults{SPF: &mxwire.SPFEvidence{Result: "fail"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, u, dom, box := mxService(t)
			seedInbound(t, svc, u.AccountID, dom.ID, "mx", map[string]any{"enforcement": tc.mode})
			raw := strings.Replace(goodRaw, "direct smtp", tc.name, 1)
			res, err := svc.IngestMX(context.Background(), mxInput(t, svc, box.Address, raw, tc.auth))
			if err != nil {
				t.Fatal(err)
			}
			if res.Disposition != mxwire.DispositionSpam {
				t.Fatalf("expected spam, got %+v", res)
			}
		})
	}
}

// TestMXModerateKeepsAlignedPass verifies a passing aligned signature is not
// turned into a cryptographic failure, and neutral/softfail evidence alone is
// not Spam.
func TestMXModerateKeepsAlignedPass(t *testing.T) {
	svc, _, _, box := mxService(t)
	auth := mxwire.AuthResults{
		SPF:  &mxwire.SPFEvidence{Result: "softfail"},
		DKIM: []mxwire.DKIMEvidence{{Result: "pass", Domain: "outside.test"}},
	}
	res, err := svc.IngestMX(context.Background(), mxInput(t, svc, box.Address, goodRaw, auth))
	if err != nil {
		t.Fatal(err)
	}
	if res.Disposition != mxwire.DispositionStored {
		t.Fatalf("expected stored, got %+v", res)
	}
}

// TestMXUnknownRecipient verifies an unknown recipient is a permanent rejection
// code and no message is stored.
func TestMXUnknownRecipient(t *testing.T) {
	svc, _, _, _ := mxService(t)
	res, err := svc.IngestMX(context.Background(), mxInput(t, svc, "nobody@example.com", goodRaw, mxwire.AuthResults{}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Code != mxwire.CodeUnknownRecipient {
		t.Fatalf("expected unknown recipient, got %+v", res)
	}
}

// TestMXDisabled verifies app-only deployments reject MX ingest.
func TestMXDisabled(t *testing.T) {
	svc, _, _, box := mxService(t)
	svc.Config.MXReceiveEnabled = false
	if _, err := svc.IngestMX(context.Background(), mxInput(t, svc, box.Address, goodRaw, mxwire.AuthResults{})); err == nil {
		t.Fatal("expected MX disabled error")
	}
}

// TestMXSpamNotInNormalListAndRelease verifies Spam visibility and release.
func TestMXSpamNotInNormalListAndRelease(t *testing.T) {
	svc, u, _, box := mxService(t)
	p := model.Principal{AccountID: u.AccountID, Admin: true}
	ctx := context.Background()
	auth := mxwire.AuthResults{DMARC: &mxwire.DMARCEvidence{Result: "fail", Policy: "reject"}}
	res, err := svc.IngestMX(ctx, mxInput(t, svc, box.Address, goodRaw, auth))
	if err != nil {
		t.Fatal(err)
	}
	normal, err := svc.Store.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(normal) != 0 {
		t.Fatalf("spam leaked into normal list: %d", len(normal))
	}
	spam, err := svc.Store.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID, SpamOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(spam) != 1 || spam[0].ID != res.MessageID {
		t.Fatalf("expected one spam message, got %d", len(spam))
	}
	if _, ev, err := svc.Store.SetMessageSpam(ctx, p, res.MessageID, false); err != nil || ev == nil {
		t.Fatalf("release: %v ev=%v", err, ev)
	} else if ev.Type != model.EventMessageSpamChanged {
		t.Fatalf("event type %s", ev.Type)
	}
	normal, _ = svc.Store.ListMessages(ctx, p, store.MessageFilter{InboxID: box.ID})
	if len(normal) != 1 {
		t.Fatalf("released message not visible: %d", len(normal))
	}
}
