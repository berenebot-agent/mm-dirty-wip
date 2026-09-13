package httpapp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gatehouse-mail/internal/app"
	"gatehouse-mail/internal/mxwire"
)

// mxReplayCache bounds request-ID reuse for the signature-skew window. A
// request ID is bound to the authenticated request fingerprint; conflicting
// reuse is rejected, while an identical retry is allowed to reach the idempotent
// service path and return its recorded outcome.
type mxReplayCache struct {
	mu  sync.Mutex
	m   map[string]string
	seq int
}

func newMXReplayCache() *mxReplayCache { return &mxReplayCache{m: map[string]string{}} }

// check records requestID->fingerprint and reports whether this is a conflicting
// reuse. Identical fingerprints (the same signed request retried) are allowed.
func (c *mxReplayCache) check(requestID, fingerprint string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	if c.seq%1024 == 0 {
		c.sweepLocked()
	}
	if prev, ok := c.m[requestID]; ok {
		return prev != fingerprint
	}
	c.m[requestID] = fingerprint
	return false
}

func (c *mxReplayCache) sweepLocked() {
	// Bounded: the cache is small and the skew window is minutes. Keep the map
	// from growing without limit under a request-ID flood by clearing it when it
	// gets large; IDs are only meaningful within the skew window anyway.
	if len(c.m) > 1<<16 {
		c.m = map[string]string{}
	}
}

// registerMX mounts the authenticated MX edge endpoints on the inbound
// listener. It is a no-op when MX receiving is disabled, so an app-only
// deployment exposes no MX routes.
func (s *Server) registerMX(m *http.ServeMux) {
	if !s.Service.Config.MXReceiveEnabled {
		return
	}
	if s.mxReplay == nil {
		s.mxReplay = newMXReplayCache()
	}
	m.HandleFunc("POST "+mxwire.PathResolve, s.mxResolve)
	m.HandleFunc("POST "+mxwire.PathIngest, s.mxIngest)
}

// mxEnvelope is the authenticated request header carrying the signature.
const mxSignatureHeader = "X-Gatehouse-MX-Signature"

func (s *Server) mxKey(keyID string) ([]byte, bool) {
	secret, ok := s.Service.Config.MXEdgeKeys[keyID]
	return []byte(secret), ok
}

// verifyMXRequest reads the bounded metadata field, verifies the HMAC over the
// exact transmitted metadata bytes and, when present, the body bytes, and
// enforces replay/version rules. It returns the metadata bytes and the body.
// The body is bounded by maxBody.
func (s *Server) verifyMXRequest(w http.ResponseWriter, r *http.Request, maxBody int64) ([]byte, []byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		writeError(w, 400, "request read failed")
		return nil, nil, false
	}
	if int64(len(body)) > maxBody {
		writeError(w, 413, "request too large")
		return nil, nil, false
	}
	var env struct {
		Metadata string `json:"metadata"`
		Body     string `json:"body"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		writeError(w, 400, "invalid envelope")
		return nil, nil, false
	}
	metaBytes := []byte(env.Metadata)
	if len(metaBytes) == 0 || len(metaBytes) > mxwire.MaxMetadataBytes {
		writeError(w, 400, "invalid metadata length")
		return nil, nil, false
	}
	var meta struct {
		Version   string `json:"version"`
		KeyID     string `json:"key_id"`
		Timestamp int64  `json:"timestamp"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		writeError(w, 400, "invalid metadata")
		return nil, nil, false
	}
	bodyBytes := []byte(env.Body)
	key, ok := s.mxKey(meta.KeyID)
	if !ok {
		writeError(w, 401, "unauthorized")
		return nil, nil, false
	}
	sig := strings.TrimSpace(r.Header.Get(mxSignatureHeader))
	now := time.Now()
	if err := mxwire.Verify(key, meta.Version, meta.KeyID, meta.KeyID, meta.Timestamp, meta.RequestID, r.Method, r.URL.Path, metaBytes, bodyBytes, sig, now, s.Service.Config.MXSignatureSkew); err != nil {
		if errors.Is(err, mxwire.ErrVersion) || errors.Is(err, mxwire.ErrSkew) {
			writeError(w, 400, err.Error())
			return nil, nil, false
		}
		// A bad signature is a temporary condition for the edge (retryable), but
		// also warrants an operator alarm. Do not leak which check failed.
		writeError(w, 401, "unauthorized")
		return nil, nil, false
	}
	fp := requestFingerprint(r.Method, r.URL.Path, metaBytes, bodyBytes)
	if s.mxReplay.check(meta.RequestID, fp) {
		writeError(w, 409, "replayed request id")
		return nil, nil, false
	}
	return metaBytes, bodyBytes, true
}

func requestFingerprint(method, path string, meta, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(meta)
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Server) mxResolve(w http.ResponseWriter, r *http.Request) {
	const maxBody = mxwire.MaxMetadataBytes + 64<<10
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	metaBytes, bodyBytes, ok := s.verifyMXRequest(w, r, maxBody)
	if !ok {
		return
	}
	var meta mxwire.ResolveRequest
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		writeError(w, 400, "invalid metadata")
		return
	}
	var rb mxwire.ResolveBody
	if err := json.Unmarshal(bodyBytes, &rb); err != nil {
		writeError(w, 400, "invalid recipient list")
		return
	}
	recipients := rb.Recipients
	if len(recipients) > mxwire.MaxResolveRecipients {
		writeError(w, 400, "too many recipients")
		return
	}
	results := s.Service.ResolveMXRecipients(r.Context(), recipients)
	out := mxwire.ResolveResponse{Version: mxwire.ProtocolVersion, MachineCode: mxwire.CodeOK}
	for _, res := range results {
		out.Results = append(out.Results, mxwire.ResolveRecipient{
			Recipient: res.Recipient, Accept: res.Accept, Domain: res.Domain, Code: string(res.Code), Temporary: res.Temporary,
		})
	}
	writeJSON(w, 200, out)
}

func (s *Server) mxIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	bodyCap := s.Service.Config.MaxMessageBytes
	if bodyCap <= 0 {
		bodyCap = mxwire.DefaultMaxBodyBytes
	}
	// Envelope overhead + metadata sits above the body cap.
	metaBytes, bodyBytes, ok := s.verifyMXRequest(w, r, bodyCap+mxwire.MaxMetadataBytes+64<<10)
	if !ok {
		return
	}
	var meta mxwire.IngestMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		writeError(w, 400, "invalid metadata")
		return
	}
	if int64(len(bodyBytes)) != meta.Size {
		writeError(w, 400, "size mismatch")
		return
	}
	// Re-check the declared digest over the actual body bytes before any trusted
	// side effect, so a metadata/body mismatch cannot be ingested.
	if got := mxwire.BodyDigest(bodyBytes); got != meta.ContentDigest {
		writeError(w, 400, "content digest mismatch")
		return
	}
	tmp, err := s.stageMXBody(bodyBytes)
	if err != nil {
		writeError(w, 500, "staging failed")
		return
	}
	defer removeFile(tmp)
	res, err := s.Service.IngestMX(r.Context(), app.MXIngestInput{
		Recipient:           meta.Recipient,
		EnvelopeFrom:        meta.EnvelopeFrom,
		RawPath:             tmp,
		Size:                meta.Size,
		DeliveryFingerprint: meta.DeliveryFingerprint,
		AuthResults:         meta.AuthResults,
		TrustedAuth:         true,
		ProviderMessageID:   meta.ProviderMessageID,
	})
	if err != nil {
		if errors.Is(err, app.ErrMXDisabled) {
			writeError(w, 404, "mx disabled")
			return
		}
		writeError(w, 500, "ingest failed")
		return
	}
	writeJSON(w, 200, mxwire.IngestResponse{
		Version: mxwire.ProtocolVersion, Disposition: res.Disposition, MachineCode: res.Code,
		MessageID: res.MessageID, Reason: res.Reason, Duplicate: res.Duplicate,
	})
}

// stageMXBody writes the already-buffered original MIME to a 0600 temp file
// under the messages staging tree, matching the provider adapters' staging
// contract. The file lives under DATA_DIR so the shared delivery primitive's
// single-recipient os.Rename stays on one filesystem.
func (s *Server) stageMXBody(body []byte) (string, error) {
	dir := filepath.Join(s.Service.Config.DataDir, "messages", ".tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "mx-*.eml")
	if err != nil {
		return "", err
	}
	if _, err = f.Write(body); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err = f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func removeFile(path string) { _ = os.Remove(path) }
