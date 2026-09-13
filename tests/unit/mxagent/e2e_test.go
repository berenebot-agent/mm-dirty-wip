package mxagent_test

import (
	"context"
	"io"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/emersion/go-smtp"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/config"
	"gatehouse-mail/internal/events"
	"gatehouse-mail/internal/httpapp"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/mxagent"
	"gatehouse-mail/internal/store"
)

// mxCore spins up a real core HTTP handler with MX enabled and returns the
// server URL plus the delivery inbox.
func mxCore(t *testing.T) (*app.Service, *httptest.Server, model.Inbox) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Config{
		DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true,
		AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20,
		SessionTTL: time.Hour, LoginLimitPerMinute: 20, SendLimitPerMinute: 60,
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
	if _, _, err := svc.SaveDomainReceivingConfig(context.Background(), u.AccountID, d.ID, "mx", map[string]any{"enforcement": "moderate"}, false); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpapp.New(svc, nil).InboundHandler())
	t.Cleanup(srv.Close)
	return svc, srv, b
}

func startEdgeWithConns(t *testing.T, coreURL string, maxConns int) (addr string, stop func()) {
	t.Helper()
	cfg := mxagent.Config{
		IngestURL: coreURL, KeyID: "edge", Secret: "secret", EdgeName: "test", Hostname: "mx.example.test",
		ListenAddr: "127.0.0.1:0", MaxMessageBytes: 5 << 20, MaxStagingBytes: 64 << 20, MaxRecipients: 10, MaxConnections: maxConns,
		ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, DataTimeout: 10 * time.Second,
		DNSTimeout: 2 * time.Second,
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := mxagent.NewServer(cfg, nil)
	done := make(chan struct{})
	go func() { _ = srv.ListenAndServe(ctx, ln); close(done) }()
	return ln.Addr().String(), func() { cancel(); <-done }
}

func startEdge(t *testing.T, coreURL string) (addr string, stop func()) {
	return startEdgeWithConns(t, coreURL, 16)
}

// TestEdgeConnectionSlotsReleased verifies the per-connection slot is returned
// when a session ends, so more than MaxConnections sequential sessions all
// succeed instead of the edge wedging with 421.
func TestEdgeConnectionSlotsReleased(t *testing.T) {
	_, core, box := mxCore(t)
	addr, stop := startEdgeWithConns(t, core.URL, 2)
	defer stop()

	for i := 0; i < 5; i++ {
		c, err := smtp.Dial(addr)
		if err != nil {
			t.Fatalf("session %d dial: %v", i, err)
		}
		if err := c.Hello("client.test"); err != nil {
			c.Close()
			t.Fatalf("session %d hello (slot leaked?): %v", i, err)
		}
		if err := c.Mail("sender@outside.test", nil); err != nil {
			c.Close()
			t.Fatalf("session %d mail: %v", i, err)
		}
		if err := c.Rcpt(box.Address, nil); err != nil {
			c.Close()
			t.Fatalf("session %d rcpt: %v", i, err)
		}
		w, err := c.Data()
		if err != nil {
			c.Close()
			t.Fatalf("session %d data: %v", i, err)
		}
		_, _ = io.WriteString(w, "From: Sender <sender@outside.test>\r\nTo: "+box.Address+"\r\nSubject: slot\r\n\r\nbody")
		if err := w.Close(); err != nil {
			c.Close()
			t.Fatalf("session %d close data: %v", i, err)
		}
		if err := c.Quit(); err != nil {
			t.Fatalf("session %d quit: %v", i, err)
		}
	}
}

// TestEdgeSMTPSessionDeliversAndRejects performs a real SMTP transaction and
// verifies delivery plus an unknown-recipient rejection.
func TestEdgeSMTPSessionDeliversAndRejects(t *testing.T) {
	svc, core, box := mxCore(t)
	addr, stop := startEdge(t, core.URL)
	defer stop()

	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Hello("client.test"); err != nil {
		t.Fatal(err)
	}
	if err := c.Mail("sender@outside.test", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Rcpt("nobody@example.com", nil); err == nil {
		t.Fatal("expected unknown recipient rejection")
	}
	if err := c.Rcpt(box.Address, nil); err != nil {
		t.Fatal(err)
	}
	w, err := c.Data()
	if err != nil {
		t.Fatal(err)
	}
	msg := "From: Sender <sender@outside.test>\r\nTo: " + box.Address + "\r\nSubject: e2e\r\nMessage-ID: <e2e@test>\r\n\r\nhello edge"
	if _, err := io.WriteString(w, msg); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Quit(); err != nil {
		t.Fatal(err)
	}
	// Allow the commit to land, then assert one stored message.
	deadline := time.Now().Add(3 * time.Second)
	for {
		msgs, _ := svc.Store.ListMessages(context.Background(), model.Principal{AccountID: box.AccountID, Admin: true}, store.MessageFilter{InboxID: box.ID})
		if len(msgs) == 1 {
			if msgs[0].Subject != "e2e" {
				t.Fatalf("subject %q", msgs[0].Subject)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("message not stored")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestEdgeDuplicateRetry verifies a repeated transaction is acknowledged and
// deduplicated by the core receipt.
func TestEdgeDuplicateRetry(t *testing.T) {
	svc, core, box := mxCore(t)
	addr, stop := startEdge(t, core.URL)
	defer stop()
	send := func() {
		c, err := smtp.Dial(addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := c.Hello("client.test"); err != nil {
			t.Fatal(err)
		}
		if err := c.Mail("sender@outside.test", nil); err != nil {
			t.Fatal(err)
		}
		if err := c.Rcpt(box.Address, nil); err != nil {
			t.Fatal(err)
		}
		w, err := c.Data()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(w, "From: Sender <sender@outside.test>\r\nTo: "+box.Address+"\r\nSubject: dup\r\n\r\nsame")
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		_ = c.Quit()
	}
	send()
	send()
	msgs, _ := svc.Store.ListMessages(context.Background(), model.Principal{AccountID: box.AccountID, Admin: true}, store.MessageFilter{InboxID: box.ID})
	if len(msgs) != 1 {
		t.Fatalf("expected dedup to one message, got %d", len(msgs))
	}
}
