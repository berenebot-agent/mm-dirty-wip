package httpapp_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/httpapp"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/imap"
	"github.com/dellarb/mailmoose/internal/transport/microsoft"
)

type microsoftPushLogEntry struct {
	message string
	attrs   map[string]string
}

type microsoftPushLogSink struct {
	mu      sync.Mutex
	entries []microsoftPushLogEntry
}

func (h *microsoftPushLogSink) Enabled(context.Context, slog.Level) bool { return true }
func (h *microsoftPushLogSink) Handle(_ context.Context, r slog.Record) error {
	entry := microsoftPushLogEntry{message: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		entry.attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.entries = append(h.entries, entry)
	h.mu.Unlock()
	return nil
}
func (h *microsoftPushLogSink) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *microsoftPushLogSink) WithGroup(string) slog.Handler      { return h }

func (h *microsoftPushLogSink) records(message string) []microsoftPushLogEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []microsoftPushLogEntry
	for _, entry := range h.entries {
		if entry.message == message {
			out = append(out, entry)
		}
	}
	return out
}

func (h *microsoftPushLogSink) containsValue(value string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, entry := range h.entries {
		if strings.Contains(entry.message, value) {
			return true
		}
		for _, attr := range entry.attrs {
			if strings.Contains(attr, value) {
				return true
			}
		}
	}
	return false
}

func TestMicrosoftPushLifecycleAndDurableDelivery(t *testing.T) {
	svc, _, user, _, _, fake := standaloneFixture(t)
	ctx := context.Background()
	logSink := &microsoftPushLogSink{}
	logger := slog.New(logSink)
	svc.Log = logger
	svc.Config.BaseURL = "https://ui.example.net"
	svc.Config.DedicatedReceiverURL = "https://receiver.example.net"
	box, err := svc.Store.CreateStandaloneInbox(ctx, user.AccountID, store.StandaloneCreate{Address: "outlook@example.net"})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var sub microsoft.Subscription
	var creates, renews, deletes int
	provider := "historical"
	var graph *httptest.Server
	graph = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/token") {
			json.NewEncoder(w).Encode(microsoft.Token{AccessToken: "access", RefreshToken: "refresh-next", ExpiresIn: 3600})
			return
		}
		if r.Header.Get("Authorization") != "Bearer access" || r.Header.Get("Prefer") != `IdType="ImmutableId"` {
			t.Error("Graph request headers missing")
		}
		switch {
		case r.URL.Path == "/subscriptions" && r.Method == "GET":
			values := []microsoft.Subscription{}
			if sub.ID != "" {
				values = append(values, sub)
			}
			json.NewEncoder(w).Encode(map[string]any{"value": values})
		case r.URL.Path == "/subscriptions" && r.Method == "POST":
			json.NewDecoder(r.Body).Decode(&sub)
			creates++
			sub.ID = "subscription"
			if sub.NotificationURL != "https://receiver.example.net"+app.MicrosoftPushPath || sub.LifecycleURL != sub.NotificationURL || sub.Resource != "users/identity/mailFolders('inbox')/messages" {
				t.Errorf("unexpected subscription: %#v", sub)
			}
			json.NewEncoder(w).Encode(sub)
		case r.URL.Path == "/subscriptions/subscription" && r.Method == "PATCH":
			var patch microsoft.Subscription
			json.NewDecoder(r.Body).Decode(&patch)
			sub.ExpiresAt = patch.ExpiresAt
			renews++
			json.NewEncoder(w).Encode(sub)
		case r.URL.Path == "/subscriptions/subscription" && r.Method == "DELETE":
			deletes++
			sub.ID = ""
			w.WriteHeader(204)
		case strings.HasSuffix(r.URL.Path, "/mailFolders/inbox"):
			w.Write([]byte(`{"id":"inbox-id","displayName":"Inbox"}`))
		case strings.HasSuffix(r.URL.Path, "/mailFolders"):
			w.Write([]byte(`{"value":[{"id":"inbox-id","displayName":"Inbox"}]}`))
		case strings.HasSuffix(r.URL.Path, "/messages/delta"):
			json.NewEncoder(w).Encode(map[string]any{"value": []any{map[string]any{"id": provider, "parentFolderId": "inbox-id", "conversationId": provider, "subject": provider, "receivedDateTime": "2026-10-11T10:00:00Z"}}, "@odata.deltaLink": graph.URL + r.URL.Path + "?cursor=next"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer graph.Close()
	newRemote := func() *app.RemoteMailboxService {
		rm := app.NewRemoteMailboxService(svc)
		rm.Microsoft = microsoft.NewClient(graph.Client())
		rm.Microsoft.BaseURL, rm.Microsoft.AuthBaseURL = graph.URL, graph.URL
		t.Cleanup(rm.Stop)
		return rm
	}
	rm := newRemote()
	grant := store.MicrosoftConnection{AccountID: user.AccountID, InboxID: box.ID, ClientID: "client", Tenant: "common", IdentityID: "identity", Mailbox: box.Address}
	if err = rm.SaveMicrosoftGrant(ctx, grant, microsoft.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: microsoft.Scope(false)}); err != nil {
		t.Fatal(err)
	}
	if err = rm.ReconcileMicrosoftPush(ctx); err != nil {
		t.Fatal(err)
	}
	push, err := svc.Store.MicrosoftPush(ctx, user.AccountID, box.ID)
	if err != nil || push.Status != "push active" || creates != 1 {
		t.Fatalf("setup: %#v %v creates=%d", push, err, creates)
	}
	boxes, err := svc.Store.ListInboxes(ctx, model.Principal{AccountID: user.AccountID, Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range boxes {
		if b.ID == box.ID {
			found = b.Capabilities != nil && b.Capabilities.Realtime
		}
	}
	if !found {
		t.Fatal("active push not exposed as realtime")
	}
	if strings.Contains(push.EncryptedState, sub.ClientState) || strings.Contains(push.EncryptedToken, "refresh") {
		t.Fatal("plaintext secret stored")
	}
	arrivals, err := svc.Store.ListRemoteArrivalsForInbox(ctx, user.AccountID, box.ID, 100)
	if err != nil || len(arrivals) != 0 {
		t.Fatalf("historical arrivals: %v %v", arrivals, err)
	}
	if _, err = svc.Store.MicrosoftPush(ctx, "other-account", box.ID); err != store.ErrNotFound {
		t.Fatal("cross-account push access")
	}
	srv := httpapp.New(svc, logger)
	srv.SetRemoteMailbox(rm)
	for _, handler := range []http.Handler{srv.Handler(), srv.InboundHandler()} {
		r := httptest.NewRequest("POST", app.MicrosoftPushPath+"?validationToken="+url.QueryEscape("opaque + challenge"), nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 || w.Body.String() != "opaque + challenge" || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
			t.Fatalf("challenge: %d %s", w.Code, w.Body.String())
		}
	}
	notify := func(id, state, lifecycle string) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"value": []any{map[string]string{"subscriptionId": id, "clientState": state, "changeType": "created", "lifecycleEvent": lifecycle}}})
		w := httptest.NewRecorder()
		srv.InboundHandler().ServeHTTP(w, httptest.NewRequest("POST", app.MicrosoftPushPath, strings.NewReader(string(b))))
		if w.Code != 202 {
			t.Fatalf("notification: %d %s", w.Code, w.Body.String())
		}
	}
	notify(sub.ID, "forged", "")
	notify("wrong-subscription", sub.ClientState, "")
	if got := len(logSink.records("microsoft push ignored")); got != 2 {
		t.Fatalf("logged %d unmatched callbacks at debug, want 2", got)
	}
	p, _ := svc.Store.MicrosoftPush(ctx, user.AccountID, box.ID)
	if p.Requested != push.Requested {
		t.Fatal("forged notification queued")
	}
	mu.Lock()
	provider = "new-arrival"
	state := sub.ClientState
	mu.Unlock()
	notify("subscription", state, "")
	notify("subscription", state, "")
	p, _ = svc.Store.MicrosoftPush(ctx, user.AccountID, box.ID)
	if p.RequestedSource != "microsoft push" {
		t.Fatalf("queued source = %q, want microsoft push", p.RequestedSource)
	}
	if logSink.containsValue(state) || logSink.containsValue("opaque + challenge") {
		t.Fatal("Microsoft push log included callback secret or validation token")
	}
	// Restart the process-owned surface; queued work survives without a poll.
	rm = newRemote()
	if err = rm.ReconcileMicrosoftPush(ctx); err != nil {
		t.Fatal(err)
	}
	arrivals, err = svc.Store.ListRemoteArrivalsForInbox(ctx, user.AccountID, box.ID, 100)
	if err != nil || len(arrivals) != 1 {
		t.Fatalf("push arrival dedup: %v %v", arrivals, err)
	}
	if arrivals[0].Source != "microsoft push" {
		t.Fatalf("push arrival source = %q, want microsoft push", arrivals[0].Source)
	}
	p, _ = svc.Store.MicrosoftPush(ctx, user.AccountID, box.ID)
	if p.Requested != p.Completed {
		t.Fatal("durable work not completed")
	}
	if p.RequestedSource != "" {
		t.Fatalf("completed work retained source %q", p.RequestedSource)
	}
	// The generation completion cannot consume a later callback.
	notify("subscription", state, "missed")
	if err = svc.Store.CompleteMicrosoftPush(ctx, user.AccountID, box.ID, p.Requested); err != nil {
		t.Fatal(err)
	}
	p, _ = svc.Store.MicrosoftPush(ctx, user.AccountID, box.ID)
	if p.Requested <= p.Completed || p.Lifecycle != "missed" {
		t.Fatal("concurrent notification lost")
	}
	notify("subscription", state, "reauthorizationRequired")
	if err = rm.ReconcileMicrosoftPush(ctx); err != nil {
		t.Fatal(err)
	}
	if renews != 1 {
		t.Fatalf("reauthorization renewals=%d", renews)
	}
	p, _ = svc.Store.MicrosoftPush(ctx, user.AccountID, box.ID)
	p.ExpiresAt = time.Now().Add(time.Hour)
	if err = svc.Store.SaveMicrosoftPush(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err = rm.ReconcileMicrosoftPush(ctx); err != nil {
		t.Fatal(err)
	}
	if renews != 2 {
		t.Fatal("expiry renewal missing")
	}
	// Lost create response: persisted secret recovers the matching upstream sub.
	p, _ = svc.Store.MicrosoftPush(ctx, user.AccountID, box.ID)
	p.SubscriptionID = ""
	if err = svc.Store.SaveMicrosoftPush(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err = rm.ReconcileMicrosoftPush(ctx); err != nil {
		t.Fatal(err)
	}
	if creates != 1 {
		t.Fatal("ambiguous create duplicated subscription")
	}
	// With an hour-long fallback poll, only the push wake-up can deliver this
	// second live arrival promptly through the durable event pipeline.
	rm.SetRemoteDialer(func(context.Context, imap.Config) (app.RemoteSession, error) { return fake, nil })
	srv = httpapp.New(svc, logger)
	srv.SetRemoteMailbox(rm)
	worker := app.NewRemoteWorker(svc, rm, logger)
	worker.SetPeriod(time.Hour)
	worker.SetPollInterval(time.Hour)
	worker.Start()
	stopped := false
	defer func() {
		if !stopped {
			worker.Stop()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		as, err := svc.Store.ListRemoteArrivalsForInbox(ctx, user.AccountID, box.ID, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(as) == 1 && as[0].DeliveryState == store.RemoteArrivalDelivered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker did not settle initial arrival: %#v", as)
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	provider = "push-worker-arrival"
	mu.Unlock()
	notify("subscription", state, "")
	deadline = time.Now().Add(5 * time.Second)
	for {
		as, err := svc.Store.ListRemoteArrivalsForInbox(ctx, user.AccountID, box.ID, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(as) == 2 && as[0].DeliveryState == store.RemoteArrivalDelivered && as[1].DeliveryState == store.RemoteArrivalDelivered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("push wake-up did not deliver: %#v", as)
		}
		time.Sleep(10 * time.Millisecond)
	}
	receivedLogs := logSink.records("remote received")
	if len(receivedLogs) != 2 {
		t.Fatalf("remote received logs = %d, want 2: %#v", len(receivedLogs), receivedLogs)
	}
	events, err := svc.Store.ListEvents(ctx, model.Principal{AccountID: user.AccountID, Admin: true}, 0, box.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	remoteEvents := 0
	eventSources := map[string]string{}
	sawPushEvent := false
	for _, event := range events {
		if event.Type != store.EventRemoteMessageReceived {
			continue
		}
		remoteEvents++
		source, _ := event.Payload["source"].(string)
		if event.Payload["provider"] != "microsoft" || (source != "microsoft" && source != "microsoft push") {
			t.Fatalf("remote event provenance = %#v", event.Payload)
		}
		eventSources[event.EntityID] = source
		sawPushEvent = sawPushEvent || source == "microsoft push"
	}
	if remoteEvents != 2 {
		t.Fatalf("remote message events = %d, want 2", remoteEvents)
	}
	if !sawPushEvent {
		t.Fatal("push-triggered arrival did not retain its source")
	}
	for _, entry := range receivedLogs {
		messageID := entry.attrs["message_id"]
		if entry.attrs["provider"] != "microsoft" || entry.attrs["to"] != box.Address || eventSources[messageID] != entry.attrs["source"] {
			t.Fatalf("remote received attrs = %#v, event source=%q", entry.attrs, eventSources[messageID])
		}
	}
	worker.Stop()
	stopped = true
	notify("subscription", state, "subscriptionRemoved")
	if err = rm.ReconcileMicrosoftPush(ctx); err != nil {
		t.Fatal(err)
	}
	if creates != 2 || deletes != 1 {
		t.Fatalf("removed subscription recovery creates=%d deletes=%d", creates, deletes)
	}
	if err = rm.ReconcileMicrosoftPush(ctx); err != nil {
		t.Fatal(err)
	}
	if creates != 2 || deletes != 1 {
		t.Fatal("removed lifecycle replay recreated subscription")
	}
	// Deletion after a lost create response must also find the upstream sub.
	p, _ = svc.Store.MicrosoftPush(ctx, user.AccountID, box.ID)
	p.SubscriptionID = ""
	if err = svc.Store.SaveMicrosoftPush(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Store.PurgeInbox(ctx, user.AccountID, box.ID); err != nil {
		t.Fatal(err)
	}
	if err = rm.ReconcileMicrosoftPush(ctx); err != nil {
		t.Fatal(err)
	}
	if deletes != 2 {
		t.Fatalf("deletion cleanup=%d", deletes)
	}
	if _, err = svc.Store.MicrosoftPush(ctx, user.AccountID, box.ID); err != store.ErrNotFound {
		t.Fatal("cleanup retained credentials")
	}
}

func TestMicrosoftPushSharedAndHTTPSFallback(t *testing.T) {
	svc, _, user, _, _, _ := standaloneFixture(t)
	ctx := context.Background()
	svc.Config.BaseURL = "http://localhost:8080"
	svc.Config.DedicatedReceiverURL = ""
	rm := app.NewRemoteMailboxService(svc)
	defer rm.Stop()
	for _, shared := range []bool{false, true} {
		address := "own@example.net"
		if shared {
			address = "shared@example.net"
		}
		box, err := svc.Store.CreateStandaloneInbox(ctx, user.AccountID, store.StandaloneCreate{Address: address})
		if err != nil {
			t.Fatal(err)
		}
		c := store.MicrosoftConnection{AccountID: user.AccountID, InboxID: box.ID, IdentityID: "identity", Mailbox: address, Shared: shared, ClientID: "client", Tenant: "common"}
		if err = rm.SaveMicrosoftGrant(ctx, c, microsoft.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600, Scope: microsoft.Scope(shared)}); err != nil {
			t.Fatal(err)
		}
		if err = rm.ReconcileMicrosoftPush(ctx); err != nil {
			t.Fatal(err)
		}
		p, err := svc.Store.MicrosoftPush(ctx, user.AccountID, box.ID)
		if shared {
			if err != store.ErrNotFound {
				t.Fatal("shared delegated subscription provisioned")
			}
		} else if err != nil || p.Status != "polling; public HTTPS receiver required" {
			t.Fatalf("HTTPS fallback: %#v %v", p, err)
		}
	}
	srv := httpapp.New(svc, nil)
	srv.SetRemoteMailbox(rm)
	for _, body := range []string{`{`, `{"value":[]}`, `{"value":[{}]}{}`, strings.Repeat("x", (1<<20)+1)} {
		w := httptest.NewRecorder()
		srv.InboundHandler().ServeHTTP(w, httptest.NewRequest("POST", app.MicrosoftPushPath, strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatalf("malformed callback accepted: %d", w.Code)
		}
	}
}
