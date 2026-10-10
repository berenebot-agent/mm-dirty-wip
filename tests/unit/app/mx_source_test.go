package app_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// TestMXSourceLabelPerFamily locks in the activity-log source label each MX
// receiving family is stamped with: the short provider name for Direct MX and
// Remote MX, and "Antler: <host>" for an Antler Dial MX receiver (resolved from
// the saved receiver snapshot, not hard-coded).
func TestMXSourceLabelPerFamily(t *testing.T) {
	ctx := context.Background()

	t.Run("antler names the concrete receiver", func(t *testing.T) {
		svc, u, d := newMXServiceWithOutboundPolicy(t, true)
		box := createMXInbox(t, svc, u.AccountID, d.ID)
		svc.AntlerEndpoints = &stubAntler{receivers: antlerReceivers()}
		if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "dialmx", map[string]any{
			"service": mxdial.ServiceAntler, "contact_email": "ops@example.test",
		}, false); err != nil {
			t.Fatal(err)
		}
		src := ingestDialMX(t, svc, box, "https://antler2.example.test")
		if src != "Antler: antler2.example.test" {
			t.Fatalf("antler source = %q", src)
		}
	})

	t.Run("custom dialmx falls back to the dialed host", func(t *testing.T) {
		svc, u, d := newMXServiceWithOutboundPolicy(t, true)
		box := createMXInbox(t, svc, u.AccountID, d.ID)
		if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, d.ID, "dialmx", map[string]any{
			"receiver_urls": "https://receiver.example",
		}, false); err != nil {
			t.Fatal(err)
		}
		src := ingestDialMX(t, svc, box, "https://receiver.example")
		if src != "Dial MX: receiver.example" {
			t.Fatalf("custom source = %q", src)
		}
	})

	t.Run("direct mx uses the short provider name", func(t *testing.T) {
		svc, _, _, box := mxService(t)
		if _, err := svc.IngestMX(ctx, mxInput(t, svc, box.Address, goodRaw, mxwire.AuthResults{})); err != nil {
			t.Fatal(err)
		}
		if src := lastInboundSource(t, svc.Store, box); src != "Direct MX" {
			t.Fatalf("direct source = %q", src)
		}
	})
}

// logSink is a minimal slog handler that captures rendered attribute values by
// key so a test can read what the ingest log stamped for a given field.
type logSink struct {
	mu    sync.Mutex
	attrs map[string][]string
	msgs  []string
}

func newLogSink() *logSink { return &logSink{attrs: map[string][]string{}} }

func (h *logSink) Enabled(context.Context, slog.Level) bool { return true }
func (h *logSink) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	r.Attrs(func(a slog.Attr) bool {
		h.attrs[a.Key] = append(h.attrs[a.Key], a.Value.String())
		return true
	})
	return nil
}
func (h *logSink) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *logSink) WithGroup(string) slog.Handler      { return h }

func (h *logSink) values(key string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.attrs[key]...)
}

// messages returns the rendered slog record messages in arrival order.
func (h *logSink) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.msgs...)
}

// TestMXIngestLogsReceivingSourceAsProvider pins the "inbound received" log
// attribute to the human receiving source rather than the internal provider
// identifier, so the mx ingest path (which persists Provider="mx" for every
// receiving family) does not log provider=mx for Direct MX, Remote MX and Dial
// MX alike.
func TestMXIngestLogsReceivingSourceAsProvider(t *testing.T) {
	svc, _, _, box := mxService(t)
	sink := newLogSink()
	svc.Log = slog.New(sink)
	if _, err := svc.IngestMX(context.Background(), mxInput(t, svc, box.Address, goodRaw, mxwire.AuthResults{})); err != nil {
		t.Fatal(err)
	}
	got := sink.values("provider")
	if len(got) != 1 || got[0] != "Direct MX" {
		t.Fatalf("provider log attrs = %q, want [Direct MX]", got)
	}
}

// createMXInbox creates an inbox under the domain and registers a dialmx
// receiving config so the dialmx ingest path resolves the recipient.
func createMXInbox(t *testing.T, svc *app.Service, accountID, domainID string) model.Inbox {
	t.Helper()
	box, err := svc.Store.CreateInbox(context.Background(), accountID, domainID, "hermes", "Hermes")
	if err != nil {
		t.Fatal(err)
	}
	return box
}

// ingestDialMX stages one raw message and ingests it through the dialmx path,
// returning the source label snapshotted on the resulting activity row.
func ingestDialMX(t *testing.T, svc *app.Service, box model.Inbox, receiverURL string) string {
	t.Helper()
	raw := "From: Sender <sender@outside.test>\r\nTo: " + box.Address + "\r\nSubject: antler\r\nMessage-ID: <antler@test>\r\nDate: Mon, 07 Sep 2026 10:00:00 +0000\r\n\r\nbody"
	in := app.MXIngestInput{
		Recipients:    []string{box.Address},
		EnvelopeFrom:  "sender@outside.test",
		RawPath:       stageMX(t, svc, raw),
		Size:          int64(len(raw)),
		ContentDigest: mxwire.BodyDigest([]byte(raw)),
		TrustedAuth:   true,
		ReceiverURL:   receiverURL,
	}
	if _, err := svc.IngestDialMX(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	return lastInboundSource(t, svc.Store, box)
}

// lastInboundSource returns the source label of the newest received row for an
// inbox, read through the domain activity log.
func lastInboundSource(t *testing.T, st *store.Store, box model.Inbox) string {
	t.Helper()
	entries, err := st.ListDomainReceivingLog(context.Background(), box.AccountID, box.DomainID, 50, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Kind == "received" && e.InboxID == box.ID {
			return e.Source
		}
	}
	t.Fatalf("no received row for inbox %s", box.ID)
	return ""
}
