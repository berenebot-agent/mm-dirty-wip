package app_test

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/dialmx/receiver"
	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/config"
	"github.com/dellarb/mailmoose/internal/events"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/mxagent"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
	"github.com/emersion/go-smtp"
)

// dialMXCore is one application core with a single dialmx receiving domain.
type dialMXCore struct {
	svc    *app.Service
	user   model.User
	domain model.Domain
	inbox  model.Inbox
}

// dialMXDomain opens an isolated store-backed core, creates one dialmx
// receiving domain, and returns it with the domain's first inbox.
func dialMXDomain(t *testing.T, name, local string) (*dialMXCore, model.Inbox) {
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
		LoginLimitPerMinute: 10, SendLimitPerMinute: 60, InboundConcurrency: 8,
		MXReceiveEnabled: true, MXReceiptRetention: time.Hour,
	}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	u, err := svc.Store.CreateAccountAndAdmin(ctx, "A-"+name, "admin@"+name, "correct horse battery staple", 50<<20)
	if err != nil {
		t.Fatal(err)
	}
	d, err := svc.Store.CreateDomain(ctx, u.AccountID, name)
	if err != nil {
		t.Fatal(err)
	}
	box, err := svc.Store.CreateInbox(ctx, u.AccountID, d.ID, local, local)
	if err != nil {
		t.Fatal(err)
	}
	return &dialMXCore{svc: svc, user: u, domain: d, inbox: box}, box
}

// dialMXTXT builds the MM1 TXT value proving the core's exact signing key.
func dialMXTXT(t *testing.T, svc *app.Service, accountID, domainID string) string {
	t.Helper()
	cred, err := svc.Store.GetDialMXCredential(context.Background(), accountID, domainID)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := base64.RawURLEncoding.DecodeString(cred.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		t.Fatalf("bad stored public key: %v", err)
	}
	return mxwire.DomainTXT(cred.KeyID, ed25519.PublicKey(pub))
}

// dialMXRecords is a live, per-domain TXT table the receiver resolves against.
// It is populated after each core mints its credential.
type dialMXRecords struct {
	mu sync.Mutex
	m  map[string]string
}

func (r *dialMXRecords) set(domain, txt string) {
	r.mu.Lock()
	r.m[domain] = txt
	r.mu.Unlock()
}

func (r *dialMXRecords) lookup(domain string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return []string{r.m[domain]}
}

// dialMXReceiver starts a real standalone receiver whose DNS proof resolves
// each domain to the exact key its own core publishes.
func dialMXReceiver(t *testing.T, records *dialMXRecords) (*receiver.Receiver, *httptest.Server, *tls.Config) {
	t.Helper()
	lookup := func(_ context.Context, q string) ([]string, error) {
		return records.lookup(strings.TrimPrefix(q, "_mailmoose-mx.")), nil
	}
	r := receiver.New(receiver.Config{
		Mode:      "shared",
		SMTP:      mxagent.Config{Hostname: "mx.test", MaxMessageBytes: 1 << 20, MaxStagingBytes: 2 << 20, MaxRecipients: 10, MaxConnections: 16, DataTimeout: 5 * time.Second, DNSTimeout: 2 * time.Second},
		LookupTXT: lookup,
		// Gate-2 routing: the domain's MX names this receiver's own hostname.
		LookupMX: func(context.Context, string) ([]*net.MX, error) {
			return []*net.MX{{Host: "mx.test."}}, nil
		},
	}, nil)
	srv := httptest.NewUnstartedServer(r.Handler())
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return r, srv, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

func dialMXManager(t *testing.T, be mxdial.Backend, client *tls.Config, dataDir string) *mxdial.Manager {
	t.Helper()
	m := mxdial.New(be, mxdial.Config{DataDir: dataDir, TLSConfig: client, ReconcileInterval: 20 * time.Millisecond, AuthRetryInterval: 200 * time.Millisecond, AllowPrivateDestinations: true})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("dialmx manager did not stop")
		}
	})
	return m
}

func dialMXReady(t *testing.T, m *mxdial.Manager, domain string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := m.Status(domain); len(s) > 0 && s[0].State == "ready" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s not ready: %#v", domain, m.Status(domain))
}

// dialMXWaitReconnect waits for a broken session to be observed as not-ready,
// then for a fresh session to authenticate again.
func dialMXWaitReconnect(t *testing.T, m *mxdial.Manager, domain string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := m.Status(domain); len(s) == 0 || s[0].State != "ready" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	dialMXReady(t, m, domain)
}

// dialMXEdge serves the real SMTL edge, each transaction delivered by the shared
// receiver. Verification is disabled so no external DNS is consulted.
func dialMXEdge(t *testing.T, r *receiver.Receiver) string {
	t.Helper()
	edge := mxagent.NewServerWithHandoff(mxagent.Config{
		Hostname: "mx.test", MaxMessageBytes: 1 << 20, MaxStagingBytes: 2 << 20,
		MaxRecipients: 10, MaxConnections: 16, DataTimeout: 5 * time.Second, DNSTimeout: 2 * time.Second,
	}, nil, func() mxagent.Delivery { return r.NewDelivery() })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = edge.ListenAndServe(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })
	return ln.Addr().String()
}

// dialMXSend runs one SMTP transaction and returns the final DATA error.
func dialMXSend(t *testing.T, addr, from string, tos []string, raw string) error {
	t.Helper()
	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.Hello("sender.test"); err != nil {
		t.Fatal(err)
	}
	if err = c.Mail(from, nil); err != nil {
		t.Fatal(err)
	}
	for _, to := range tos {
		if err = c.Rcpt(to, nil); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	_, _ = io.WriteString(w, raw)
	return w.Close()
}

func dialMX451(t *testing.T, err error) {
	t.Helper()
	var se *smtp.SMTPError
	if !errors.As(err, &se) || se.Code != 451 {
		t.Fatalf("expected SMTP 451, got %v", err)
	}
}

// scriptedBackend wraps the real core backend to make a chosen ingest attempt
// fail after (or instead of) its durable commit.
type scriptedBackend struct {
	mxdial.Backend
	mu        sync.Mutex
	failNext  bool
	after     func()
	afterOnce sync.Once
}

func (b *scriptedBackend) Ingest(ctx context.Context, domains []string, meta mxwire.IngestMetadata, path, receiverURL string) (mxwire.IngestResponse, error) {
	b.mu.Lock()
	fail := b.failNext
	b.failNext = false
	after := b.after
	b.mu.Unlock()
	if fail {
		out := mxwire.IngestResponse{Version: mxwire.V2Protocol, MachineCode: mxwire.CodeTempFail}
		for _, r := range meta.Recipients {
			out.PerRecipient = append(out.PerRecipient, mxwire.RecipientIngestResult{Recipient: r, MachineCode: mxwire.CodeTempFail})
		}
		return out, nil
	}
	res, err := b.Backend.Ingest(ctx, domains, meta, path, receiverURL)
	if after != nil {
		b.afterOnce.Do(after)
	}
	return res, err
}

func dialMXMessages(t *testing.T, svc *app.Service, accountID, inboxID string) []model.Message {
	t.Helper()
	msgs, err := svc.Store.ListMessages(context.Background(), model.Principal{AccountID: accountID, Admin: true}, store.MessageFilter{InboxID: inboxID})
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func dialMXEvents(t *testing.T, svc *app.Service, accountID string) []model.Event {
	t.Helper()
	evs, err := svc.Store.ListEvents(context.Background(), model.Principal{AccountID: accountID, Admin: true}, 0, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func dialMXUsed(t *testing.T, svc *app.Service, accountID string) int64 {
	t.Helper()
	acct, err := svc.Store.GetAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	return acct.StorageUsedBytes
}

// dialMXAssertRaw proves the core stored the original bytes untouched: it may
// carry only the SMTP DATA line terminator the transport adds, and must never
// gain a Received trace header.
func dialMXAssertRaw(t *testing.T, svc *app.Service, m model.Message, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(svc.Config.DataDir, filepath.FromSlash(m.RawPath)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), want) || strings.TrimRight(string(got), "\r\n") != want {
		t.Fatalf("stored raw MIME changed:\nwant %q\ngot  %q", want, string(got))
	}
	head, _, _ := strings.Cut(string(got), "\r\n\r\n")
	if strings.Contains(head, "Received:") {
		t.Fatal("core appended a Received trace header to stored MIME")
	}
}

const dialMXRaw = "From: Sender <sender@outside.test>\r\nTo: A <a@first.example.com>\r\nCc: B <b@second.example.com>\r\nSubject: dialmx e2e\r\nMessage-ID: <dialmx-e2e@test>\r\nDate: Mon, 07 Sep 2026 10:00:00 +0000\r\n\r\nbody via dial mx"

// TestDialMXEndToEndFanoutRetry drives two independent cores through one real
// receiver and one real SMTP edge. The first transaction defers on the second
// core after the first has committed; the retry returns 250 and committed
// recipients deduplicate. A Bcc-only recipient is delivered even though it
// never appears in a header.
func TestDialMXEndToEndFanoutRetry(t *testing.T) {
	coreA, boxA := dialMXDomain(t, "first.example.com", "a")
	boxC, err := coreA.svc.Store.CreateInbox(context.Background(), coreA.user.AccountID, coreA.domain.ID, "c", "C")
	if err != nil {
		t.Fatal(err)
	}
	coreB, boxB := dialMXDomain(t, "second.example.com", "b")

	records := &dialMXRecords{m: map[string]string{}}
	r, srv, client := dialMXReceiver(t, records)
	for _, c := range []*dialMXCore{coreA, coreB} {
		if _, _, err := c.svc.SaveDomainReceivingConfig(context.Background(), c.user.AccountID, c.domain.ID, "dialmx", map[string]any{"receiver_urls": srv.URL}, false); err != nil {
			t.Fatal(err)
		}
		records.set(c.domain.Name, dialMXTXT(t, c.svc, c.user.AccountID, c.domain.ID))
	}
	beA := &scriptedBackend{Backend: coreA.svc.DialMXBackend()}
	beB := &scriptedBackend{Backend: coreB.svc.DialMXBackend()}
	beB.failNext = true
	coreB.svc.Config.MXReceiveEnabled = false
	local, err := coreA.svc.Store.CreateDomain(context.Background(), coreA.user.AccountID, "local.example.com")
	if err != nil {
		t.Fatal(err)
	}
	localBox, err := coreA.svc.Store.CreateInbox(context.Background(), coreA.user.AccountID, local.ID, "local", "Local")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := coreA.svc.SaveDomainReceivingConfig(context.Background(), coreA.user.AccountID, local.ID, "mx", nil, false); err != nil {
		t.Fatal(err)
	}
	if results := coreA.svc.ResolveMXRecipients(context.Background(), []string{localBox.Address}); len(results) != 1 || !results[0].Accept {
		t.Fatal("local MX cannot coexist with Dial MX")
	}
	mA := dialMXManager(t, beA, client, coreA.svc.Config.DataDir)
	mB := dialMXManager(t, beB, client, coreB.svc.Config.DataDir)
	dialMXReady(t, mA, coreA.domain.Name)
	dialMXReady(t, mB, coreB.domain.Name)
	edge := dialMXEdge(t, r)
	var rejected *smtp.SMTPError
	if err := dialMXSend(t, edge, "sender@outside.test", []string{"unknown@" + coreA.domain.Name}, dialMXRaw); !errors.As(err, &rejected) || rejected.Code != 550 {
		t.Fatalf("unknown recipient must be permanently rejected: %v", err)
	}

	// Envelope has three recipients; Bcc never appears in the headers.
	tos := []string{boxA.Address, boxB.Address, "c@" + coreA.domain.Name}
	dialMX451(t, dialMXSend(t, edge, "sender@outside.test", tos, dialMXRaw))
	if err := dialMXSend(t, edge, "sender@outside.test", tos, dialMXRaw); err != nil {
		t.Fatalf("retry should be 250: %v", err)
	}

	// core A: the To recipient and the header-absent Bcc recipient, each in its
	// own inbox. The Bcc address never appears in the headers but is delivered.
	msgsA := dialMXMessages(t, coreA.svc, coreA.user.AccountID, boxA.ID)
	if len(msgsA) != 1 {
		t.Fatalf("core A To inbox: got %d messages, want 1", len(msgsA))
	}
	dialMXAssertRaw(t, coreA.svc, msgsA[0], dialMXRaw)
	msgsC := dialMXMessages(t, coreA.svc, coreA.user.AccountID, boxC.ID)
	if len(msgsC) != 1 {
		t.Fatalf("core A Bcc inbox: got %d messages, want 1", len(msgsC))
	}
	if strings.Contains(dialMXRaw, "c@"+coreA.domain.Name) {
		t.Fatal("Bcc recipient must not appear in the message headers")
	}
	msgsB := dialMXMessages(t, coreB.svc, coreB.user.AccountID, boxB.ID)
	if len(msgsB) != 1 {
		t.Fatalf("core B: got %d messages, want 1", len(msgsB))
	}
	if got, want := dialMXUsed(t, coreA.svc, coreA.user.AccountID), msgsA[0].SizeBytes+msgsC[0].SizeBytes; got != want {
		t.Fatalf("core A quota bytes = %d, want %d", got, want)
	}
	if got, want := dialMXUsed(t, coreB.svc, coreB.user.AccountID), int64(msgsB[0].SizeBytes); got != want {
		t.Fatalf("core B quota bytes = %d, want %d", got, want)
	}
	if got := len(dialMXEvents(t, coreA.svc, coreA.user.AccountID)); got != 2 {
		t.Fatalf("core A events = %d, want 2 (exactly once per recipient)", got)
	}
	if got := len(dialMXEvents(t, coreB.svc, coreB.user.AccountID)); got != 1 {
		t.Fatalf("core B events = %d, want 1", got)
	}
}

// TestDialMXLostACKDeduplicates commits durably, then breaks the receiver
// session before the result reaches the edge, so the sender sees 451. The retry
// re-sends byte-identical MIME and must deduplicate to one message, one quota
// charge and one event, with the original bytes preserved exactly.
func TestDialMXLostACKDeduplicates(t *testing.T) {
	core, box := dialMXDomain(t, "lost.example.com", "inbox")
	records := &dialMXRecords{m: map[string]string{}}
	r, srv, client := dialMXReceiver(t, records)
	if _, _, err := core.svc.SaveDomainReceivingConfig(context.Background(), core.user.AccountID, core.domain.ID, "dialmx", map[string]any{"receiver_urls": srv.URL}, false); err != nil {
		t.Fatal(err)
	}
	records.set(core.domain.Name, dialMXTXT(t, core.svc, core.user.AccountID, core.domain.ID))
	be := &scriptedBackend{Backend: core.svc.DialMXBackend()}
	be.after = func() {
		// The real ingest has durably committed; now drop the session so the
		// result never reaches the edge (lost ACK).
		srv.CloseClientConnections()
	}
	m := dialMXManager(t, be, client, core.svc.Config.DataDir)
	dialMXReady(t, m, core.domain.Name)
	edge := dialMXEdge(t, r)

	dialMX451(t, dialMXSend(t, edge, "sender@outside.test", []string{box.Address}, dialMXRaw))
	dialMXWaitReconnect(t, m, core.domain.Name)
	if err := dialMXSend(t, edge, "sender@outside.test", []string{box.Address}, dialMXRaw); err != nil {
		t.Fatalf("retry should be 250: %v", err)
	}

	msgs := dialMXMessages(t, core.svc, core.user.AccountID, box.ID)
	if len(msgs) != 1 {
		t.Fatalf("lost-ACK retry stored %d messages, want 1", len(msgs))
	}
	dialMXAssertRaw(t, core.svc, msgs[0], dialMXRaw)
	if got := dialMXUsed(t, core.svc, core.user.AccountID); got != int64(msgs[0].SizeBytes) {
		t.Fatalf("quota bytes = %d, want one charge of %d", got, msgs[0].SizeBytes)
	}
	if got := len(dialMXEvents(t, core.svc, core.user.AccountID)); got != 1 {
		t.Fatalf("events = %d, want 1", got)
	}
}
