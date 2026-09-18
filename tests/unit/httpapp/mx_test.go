package httpapp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

// testMXSecret is a fixed 32-byte (256-bit) hex secret shared by the core
// fixture and the request signer below.
const testMXSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

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
		MXReceiveEnabled: true, MXEdgeKeys: map[string]string{"edge": testMXSecret}, MXSignatureSkew: 10 * time.Minute,
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
	return svc, httpapp.New(svc, nil).InboundHandler(), u, d, b
}

// mxRequest builds a framed, signed MX request with the given metadata JSON and
// body.
func mxRequest(path, meta string, body []byte, keyID string) *http.Request {
	var m struct {
		Timestamp int64  `json:"timestamp"`
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal([]byte(meta), &m)
	sig := mxwire.Sign([]byte(testMXSecret), mxwire.ProtocolVersion, keyID, m.Timestamp, m.RequestID, "POST", path, mxwire.MetaDigest([]byte(meta)), mxwire.BodyDigest(body))
	var buf bytes.Buffer
	_ = mxwire.WritePrelude(&buf, []byte(meta))
	buf.Write(body)
	req := httptest.NewRequest("POST", path, bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", mxwire.IngestContentType)
	req.Header.Set("X-Gatehouse-MX-Signature", sig)
	return req
}

func TestMXResolveEndpoint(t *testing.T) {
	_, h, _, _, box := mxFixture(t)
	meta := `{"version":"mx-v1","key_id":"edge","timestamp":` +
		itoa(time.Now().Unix()) + `,"request_id":"r1","edge":"mx-1"}`
	body := []byte(`{"recipients":["` + box.Address + `","nobody@example.com"]}`)
	req := mxRequest(mxwire.PathResolve, meta, body, "edge")
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
		`,"request_id":"r2","edge":"mx-1","recipients":["` + box.Address + `"],"envelope_from":"s@outside.test","content_digest":"` + digest +
		`","size":` + itoa(int64(len(raw))) + `,"auth_results":{}}`
	req := mxRequest(mxwire.PathIngest, meta, raw, "edge")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("ingest %d %s", rr.Code, rr.Body.String())
	}
	var out mxwire.IngestResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.PerRecipient) != 1 || out.PerRecipient[0].MachineCode != mxwire.CodeOK ||
		out.PerRecipient[0].Disposition != mxwire.DispositionStored {
		t.Fatalf("unexpected %+v", out)
	}
	// Tampered body fails the digest recheck.
	bad := append([]byte{}, raw...)
	bad = append(bad, 'x')
	req2 := mxRequest(mxwire.PathIngest, meta, bad, "edge")
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code == 200 {
		t.Fatalf("digest mismatch accepted: %s", rr2.Body.String())
	}
}

// TestMXIngestRejectsBadSignatureBeforeStaging proves the HMAC is checked
// against the signed declared digest before any body byte is written, so an
// unauthenticated caller cannot force staging I/O.
func TestMXIngestRejectsBadSignatureBeforeStaging(t *testing.T) {
	svc, h, _, _, box := mxFixture(t)
	raw := []byte("From: s@outside.test\r\nTo: " + box.Address + "\r\nSubject: hi\r\n\r\nbody")
	digest := mxwire.BodyDigest(raw)
	meta := `{"version":"mx-v1","key_id":"edge","timestamp":` + itoa(time.Now().Unix()) +
		`,"request_id":"preauth","edge":"mx-1","recipients":["` + box.Address + `"],"envelope_from":"s@outside.test","content_digest":"` + digest +
		`","size":` + itoa(int64(len(raw))) + `,"auth_results":{}}`
	req := mxRequest(mxwire.PathIngest, meta, raw, "edge")
	req.Header.Set("X-Gatehouse-MX-Signature", "deadbeef")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("bad signature accepted: %d", rr.Code)
	}
	dir := filepath.Join(svc.Config.DataDir, "messages", ".tmp")
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unauthenticated request staged %d file(s) before signature verification", len(entries))
	}
}

func TestMXBadSignatureRejected(t *testing.T) {
	_, h, _, _, box := mxFixture(t)
	meta := `{"version":"mx-v1","key_id":"edge","timestamp":` + itoa(time.Now().Unix()) + `,"request_id":"r3"}`
	body := []byte(`{"recipients":["` + box.Address + `"]}`)
	req := mxRequest(mxwire.PathResolve, meta, body, "edge")
	req.Header.Set("X-Gatehouse-MX-Signature", "deadbeef")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("bad signature accepted: %d", rr.Code)
	}
}

// TestMXReplayConflictRejected verifies an identical signed retry is allowed
// through to the idempotent path, while the same request ID bound to different
// content is rejected.
func TestMXReplayConflictRejected(t *testing.T) {
	_, h, _, _, box := mxFixture(t)
	meta := `{"version":"mx-v1","key_id":"edge","timestamp":` + itoa(time.Now().Unix()) +
		`,"request_id":"replay-1","edge":"mx-1"}`
	body := []byte(`{"recipients":["` + box.Address + `"]}`)
	send := func(b []byte) int {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, mxRequest(mxwire.PathResolve, meta, b, "edge"))
		return rr.Code
	}
	if code := send(body); code != 200 {
		t.Fatalf("first resolve: %d", code)
	}
	// Identical retry is idempotent, not a conflict.
	if code := send(body); code != 200 {
		t.Fatalf("identical retry rejected: %d", code)
	}
	// Same request ID, different body: conflicting reuse.
	other := []byte(`{"recipients":["other@example.com"]}`)
	if code := send(other); code != 409 {
		t.Fatalf("conflicting replay accepted: %d", code)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
