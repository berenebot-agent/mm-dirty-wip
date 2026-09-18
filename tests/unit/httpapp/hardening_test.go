package httpapp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/config"
	"gatehouse-mail/internal/events"
	"gatehouse-mail/internal/httpapp"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/store"
)

func TestSendForbiddenForReadAndAssistant(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	for _, role := range []string{"read", "assistant"} {
		_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, role, false, map[string]string{box.ID: role})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "/v1/send", strings.NewReader(`{"inbox_id":"`+box.ID+`","to":["x@y.test"],"subject":"s","text":"t"}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s send = %d, want 403", role, rr.Code)
		}
	}
}

func TestRawMIMERouteIsCanonical(t *testing.T) {
	_, h, _, _, box := httpFixture(t)
	req := signedMGRequest(t, testMailgunKey, "raw-mime-1", box.Address, inboundRawMessage())
	req.URL.Path = "/internal/ingest/mailgun/raw-mime"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("raw-mime route = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestEventsWaitReturnsNewEvent(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest("GET", "/v1/events/wait?timeout=5", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		done <- rr
	}()
	time.Sleep(100 * time.Millisecond)
	// Commit an inbound message and publish its event to the hub so the
	// long-poll wakes up.
	m, ev, _, err := svc.Store.CommitInbound(ctx, store.InboundRecord{
		Inbox: box, Provider: "mailgun", ProviderDeliveryID: "wait-1", RFCMessageID: "<wait@test>",
		From: model.Address{Name: "Sender", Address: "sender@outside.test"}, To: []string{box.Address},
		EnvelopeTo: []string{box.Address}, Subject: "Wait event", Text: "body",
		RawPath: "messages/test.eml", SizeBytes: 4, ReceivedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.Hub.Publish(ev)
	select {
	case rr := <-done:
		if rr.Code != 200 {
			t.Fatalf("wait = %d body=%s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), m.ID) {
			t.Fatalf("wait body %s", rr.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("events/wait did not return")
	}
}

func TestSSEReplayAndLive(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Seed an event before connecting.
	m1, ev1, _, err := svc.Store.CommitInbound(ctx, store.InboundRecord{
		Inbox: box, Provider: "mailgun", ProviderDeliveryID: "sse-1", RFCMessageID: "<sse1@test>",
		From: model.Address{Address: "sender@outside.test"}, To: []string{box.Address},
		EnvelopeTo: []string{box.Address}, Subject: "SSE one", Text: "one",
		RawPath: "messages/test.eml", SizeBytes: 4, ReceivedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.Hub.Publish(ev1)
	ts := httptest.NewServer(h)
	defer ts.Close()
	req, err := http.NewRequest("GET", ts.URL+"/v1/events/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	// Read the replayed event.
	readUntil := func(substr string) error {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			line, err := br.ReadString('\n')
			if err != nil {
				return err
			}
			if strings.Contains(line, substr) {
				return nil
			}
		}
		return fmt.Errorf("timed out waiting for %q", substr)
	}
	if err = readUntil(m1.ID); err != nil {
		t.Fatal(err)
	}
	// Ingest a live event and confirm it arrives on the same stream.
	m2, ev2, _, err := svc.Store.CommitInbound(ctx, store.InboundRecord{
		Inbox: box, Provider: "mailgun", ProviderDeliveryID: "sse-2", RFCMessageID: "<sse2@test>",
		From: model.Address{Address: "sender@outside.test"}, To: []string{box.Address},
		EnvelopeTo: []string{box.Address}, Subject: "SSE two", Text: "two",
		RawPath: "messages/test.eml", SizeBytes: 4, ReceivedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.Hub.Publish(ev2)
	if err = readUntil(m2.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSearchFiltersFromToBefore(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	ctx := context.Background()
	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	seedInbound(t, svc, box, "f-1", "<f1@test>", "Alpha", "alpha body")
	seedInbound(t, svc, box, "f-2", "<f2@test>", "Beta", "beta body")
	// from filter
	req := httptest.NewRequest("GET", "/v1/search?q=body&from=sender@outside.test", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Alpha") || !strings.Contains(rr.Body.String(), "Beta") {
		t.Fatalf("from filter %d %s", rr.Code, rr.Body.String())
	}
	// to filter
	req = httptest.NewRequest("GET", "/v1/search?q=body&to="+box.Address, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Alpha") {
		t.Fatalf("to filter %d %s", rr.Code, rr.Body.String())
	}
	// before filter (cursor = first message id)
	msgs, err := svc.Store.ListMessages(ctx, model.Principal{AccountID: u.AccountID, Admin: true}, store.MessageFilter{Limit: 1})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("list %v %#v", err, msgs)
	}
	req = httptest.NewRequest("GET", "/v1/search?q=body&before="+msgs[0].ID, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("before filter %d %s", rr.Code, rr.Body.String())
	}
	var got []model.Message
	if err = json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("before filter returned %d messages, want 1", len(got))
	}
}

func TestBootstrapTokenRequired(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 20, SendLimitPerMinute: 60, AdminBootstrapToken: "sekret-token"}
	svc, err := app.New(cfg, st, events.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	h := httpapp.New(svc, nil).Handler()
	// Without the token, setup is rejected.
	form := "account=A&email=admin@example.com&password=correct-horse-battery-staple&_csrf=csrf"
	req := httptest.NewRequest("POST", "/setup", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "ghm_csrf", Value: "csrf"})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("setup without token = %d", rr.Code)
	}
	if has, _ := st.HasUsers(context.Background()); has {
		t.Fatal("admin created without bootstrap token")
	}
	// With the token, setup succeeds.
	form = "account=A&email=admin@example.com&password=correct-horse-battery-staple&_csrf=csrf&bootstrap_token=sekret-token"
	req = httptest.NewRequest("POST", "/setup", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "ghm_csrf", Value: "csrf"})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("setup with token = %d body=%s", rr.Code, rr.Body.String())
	}
	if has, _ := st.HasUsers(context.Background()); !has {
		t.Fatal("admin not created with bootstrap token")
	}
}

func TestSendRejectsHeaderInjection(t *testing.T) {
	svc, h, u, _, box := httpFixture(t)
	_, key, err := svc.Store.CreateAPIKey(context.Background(), u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"inbox_id":"` + box.ID + `","to":["victim@example.net\r\nBcc: attacker@example.net"],"subject":"s","text":"t"}`
	req := httptest.NewRequest("POST", "/v1/send", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("send with injected address = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestUntrustedProxyHeaderIgnored(t *testing.T) {
	newHandler := func(t *testing.T, trusted string) http.Handler {
		t.Helper()
		dir := t.TempDir()
		st, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		cfg := config.Config{DataDir: dir, BaseURL: "https://mail.example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 20, SendLimitPerMinute: 60}
		if trusted != "" {
			cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix(trusted)}
		}
		svc, err := app.New(cfg, st, events.NewHub())
		if err != nil {
			t.Fatal(err)
		}
		return httpapp.New(svc, nil).Handler()
	}
	// Untrusted peer: spoofed X-Forwarded-Proto must NOT set Secure cookies.
	h := newHandler(t, "")
	req := httptest.NewRequest("GET", "/setup", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.RemoteAddr = "203.0.113.5:1234"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	for _, c := range rr.Result().Cookies() {
		if c.Secure {
			t.Fatalf("untrusted peer set Secure cookie: %#v", c)
		}
	}
	// Trusted peer: X-Forwarded-Proto https sets Secure cookies.
	h = newHandler(t, "203.0.113.0/24")
	req = httptest.NewRequest("GET", "/setup", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.RemoteAddr = "203.0.113.5:1234"
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	found := false
	for _, c := range rr.Result().Cookies() {
		if c.Secure {
			found = true
		}
	}
	if !found {
		t.Fatal("trusted peer should set Secure cookies")
	}
}

// TestClientIPSpoofingResistsAppendingProxy proves the rate-limit identity
// cannot be chosen by prepending a fake leftmost X-Forwarded-For entry: an
// appending proxy preserves the attacker's entry as
// "fake, real", and the limiter must see the proxy-observed address.
func TestClientIPSpoofingResistsAppendingProxy(t *testing.T) {
	newLoginHandler := func(t *testing.T, trusted string) http.Handler {
		t.Helper()
		dir := t.TempDir()
		st, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		cfg := config.Config{DataDir: dir, BaseURL: "http://example.test", Mode: "selfhosted", AllowPrivateOutbound: true, AppEncryptionKey: "01234567890123456789012345678901", MaxMessageBytes: 5 << 20, DefaultQuotaBytes: 50 << 20, SessionTTL: time.Hour, LoginLimitPerMinute: 1, SendLimitPerMinute: 60}
		if trusted != "" {
			cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix(trusted)}
		}
		svc, err := app.New(cfg, st, events.NewHub())
		if err != nil {
			t.Fatal(err)
		}
		return httpapp.New(svc, nil).Handler()
	}
	login := func(h http.Handler, remoteAddr, xff string) int {
		req := httptest.NewRequest("POST", "/login", strings.NewReader("email=a@b&password=x&_csrf=csrf"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: "ghm_csrf", Value: "csrf"})
		req.RemoteAddr = remoteAddr
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	proxy := "203.0.113.5:1234"
	// Trusted proxy, attacker prepends a rotating fake per attempt: every
	// attempt must still count against the same proxy-observed key, so the
	// second attempt is rate limited.
	h := newLoginHandler(t, "203.0.113.0/24")
	if code := login(h, proxy, "198.51.100.9, 192.0.2.44"); code != 303 && code != 200 {
		t.Fatalf("first login attempt = %d", code)
	}
	if code := login(h, proxy, "198.51.100.10, 192.0.2.44"); code != 429 {
		t.Fatalf("spoofed second login = %d, want 429 (same limit key)", code)
	}
	// Multi-hop: strip both trusted proxies, keep the originating client. A
	// prepended fake must not change the key (leftmost parsing would see
	// 198.51.100.99 and miss the limit).
	h = newLoginHandler(t, "10.0.0.0/8")
	if code := login(h, "10.0.0.9:1", "192.0.2.7, 10.0.0.1, 10.0.0.2"); code != 303 && code != 200 {
		t.Fatalf("multi-hop first = %d", code)
	}
	if code := login(h, "10.0.0.9:1", "198.51.100.99, 192.0.2.7, 10.0.0.1, 10.0.0.2"); code != 429 {
		t.Fatalf("multi-hop spoofed second = %d, want 429", code)
	}
	// Untrusted peer: XFF is ignored, the peer itself is the key.
	h = newLoginHandler(t, "")
	if code := login(h, "198.51.100.5:1234", "192.0.2.44"); code != 303 && code != 200 {
		t.Fatalf("untrusted first = %d", code)
	}
	if code := login(h, "198.51.100.5:1234", "192.0.2.99"); code != 429 {
		t.Fatalf("untrusted second with different XFF = %d, want 429 (peer key)", code)
	}
}
