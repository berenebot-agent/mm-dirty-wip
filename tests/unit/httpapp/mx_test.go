package httpapp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/config"
	"gatehouse-mail/internal/events"
	"gatehouse-mail/internal/httpapp"
	"gatehouse-mail/internal/model"
	"gatehouse-mail/internal/mxwire"
	"gatehouse-mail/internal/store"
)

// mxFixture builds an HTTP handler with MX enabled and the domain receiving
// provider set to mx.
func mxFixture(t *testing.T) (*app.Service, http.Handler, model.User, model.Domain, model.Inbox) {
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
	return svc, httpapp.New(svc, nil).Handler(), u, d, b
}

func mxEnvelope(t *testing.T, path, meta string, body []byte, keyID string) *http.Request {
	t.Helper()
	var m struct {
		Version   string `json:"version"`
		Timestamp int64  `json:"timestamp"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(meta), &m); err != nil {
		t.Fatal(err)
	}
	sig := mxwire.Sign([]byte("secret"), mxwire.ProtocolVersion, keyID, m.Timestamp, m.RequestID, "POST", path, []byte(meta), body)
	payload, err := json.Marshal(map[string]string{"metadata": meta, "body": string(body)})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", path, strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gatehouse-MX-Signature", sig)
	return req
}

func TestMXResolveEndpoint(t *testing.T) {
	_, h, _, _, box := mxFixture(t)
	meta := `{"version":"mx-v1","key_id":"edge","timestamp":` +
		itoa(time.Now().Unix()) + `,"request_id":"r1","edge":"mx-1"}`
	body := []byte(`{"recipients":["` + box.Address + `","nobody@example.com"]}`)
	req := mxEnvelope(t, mxwire.PathResolve, meta, body, "edge")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("resolve %d %s", rr.Code, rr.Body.String())
	}
	var out mxwire.ResolveResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 {
		t.Fatalf("results %d", len(out.Results))
	}
	if !out.Results[0].Accept || out.Results[1].Accept {
		t.Fatalf("unexpected accept flags %+v", out.Results)
	}
}

func TestMXIngestEndpointAndDigestMismatch(t *testing.T) {
	_, h, _, _, box := mxFixture(t)
	raw := []byte("From: s@outside.test\r\nTo: " + box.Address + "\r\nSubject: hi\r\n\r\nbody")
	digest := mxwire.BodyDigest(raw)
	meta := `{"version":"mx-v1","key_id":"edge","timestamp":` + itoa(time.Now().Unix()) +
		`,"request_id":"r2","edge":"mx-1","recipient":"` + box.Address + `","envelope_from":"s@outside.test","content_digest":"` + digest +
		`","size":` + itoa(int64(len(raw))) + `,"delivery_fingerprint":"` + mxwire.DeliveryFingerprint("s@outside.test", box.Address, digest) + `","auth_results":{}}`
	req := mxEnvelope(t, mxwire.PathIngest, meta, raw, "edge")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("ingest %d %s", rr.Code, rr.Body.String())
	}
	var out mxwire.IngestResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Disposition != mxwire.DispositionStored || out.MachineCode != mxwire.CodeOK {
		t.Fatalf("unexpected %+v", out)
	}
	// Tampered body fails the digest recheck.
	bad := append([]byte{}, raw...)
	bad = append(bad, 'x')
	req2 := mxEnvelope(t, mxwire.PathIngest, meta, bad, "edge")
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code == 200 {
		t.Fatalf("digest mismatch accepted: %s", rr2.Body.String())
	}
}

func TestMXBadSignatureRejected(t *testing.T) {
	_, h, _, _, box := mxFixture(t)
	meta := `{"version":"mx-v1","key_id":"edge","timestamp":` + itoa(time.Now().Unix()) + `,"request_id":"r3"}`
	body := []byte(`{"recipients":["` + box.Address + `"]}`)
	req := mxEnvelope(t, mxwire.PathResolve, meta, body, "edge")
	req.Header.Set("X-Gatehouse-MX-Signature", "deadbeef")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("bad signature accepted: %d", rr.Code)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
