package httpapp_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/mxdial"
)

// TestDialMXRefusedReceiverIsReportedRejected proves the operator-facing status
// of a receiver that answers but refuses the session is a settled error, not a
// pending authorization. A receiver running an older core API answers 404 for
// the session path this core posts; before this the row read as a session still
// reconnecting for as long as the receiver stayed on the old build.
func TestDialMXRefusedReceiverIsReportedRejected(t *testing.T) {
	t.Parallel()
	svc, h, u, domain, _ := httpFixture(t)
	ctx := context.Background()

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the Hello like a real receiver, so the refusal answers a complete
		// attempt rather than resetting the stream mid-write.
		_, _ = mxwire.ReadFrame(r.Body)
		w.WriteHeader(http.StatusNotFound)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	if _, _, err := svc.SaveDomainReceivingConfig(ctx, u.AccountID, domain.ID, "dialmx", map[string]any{
		"service": mxdial.ServiceCustom, "receiver_urls": srv.URL, "enforcement": "moderate",
	}, false); err != nil {
		t.Fatalf("dialmx save: %v", err)
	}

	m := mxdial.New(svc.DialMXBackend(), mxdial.Config{
		DataDir:                  svc.Config.DataDir,
		AllowPrivateDestinations: true,
		ReconcileInterval:        20 * time.Millisecond,
		TLSConfig:                &tls.Config{RootCAs: pool, ServerName: parsed.Hostname()},
	})
	svc.DialMX = m
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go m.Run(runCtx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rows := m.Status(domain.Name); len(rows) > 0 && rows[0].State == mxdial.StatusRejected {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rows := m.Status(domain.Name); len(rows) == 0 || rows[0].State != mxdial.StatusRejected {
		t.Fatalf("receiver not observed rejected: %#v", m.Status(domain.Name))
	}

	_, key, err := svc.Store.CreateAPIKey(ctx, u.AccountID, "admin", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/domains/"+domain.ID+"/receiving", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("receiving GET = %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	statuses, _ := body["status"].([]any)
	if len(statuses) != 1 {
		t.Fatalf("want one status row: %v", body["status"])
	}
	row := statuses[0].(map[string]any)
	if row["state"] != mxdial.StatusRejected {
		t.Fatalf("state = %v, want %q", row["state"], mxdial.StatusRejected)
	}
	if row["reason"] != mxdial.ReasonRejected {
		t.Fatalf("reason = %v, want %q", row["reason"], mxdial.ReasonRejected)
	}
	// The status the receiver URL answered with is reported, so an operator (or a
	// caller) can name an endpoint's own 4xx/5xx instead of a bare "unreachable".
	if row["http_code"] != float64(http.StatusNotFound) {
		t.Fatalf("http_code = %v, want %d", row["http_code"], http.StatusNotFound)
	}
}
