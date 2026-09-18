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

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/mxwire"
)

// mxReplayCache bounds request-ID reuse for the signature-skew window. A
// request ID is bound to the authenticated request fingerprint; conflicting
// reuse is rejected, while an identical retry is allowed to reach the idempotent
// service path and return its recorded outcome.
type mxReplayCache struct {
	mu  sync.Mutex
	m   map[string]replayEntry
	seq int
}

type replayEntry struct {
	fingerprint string
	seenAt      time.Time
}

func newMXReplayCache() *mxReplayCache { return &mxReplayCache{m: map[string]replayEntry{}} }

// check records requestID->fingerprint and reports whether this is a conflicting
// reuse. Identical fingerprints (the same signed request retried) are allowed,
// including when the prior entry is older than the skew window.
func (c *mxReplayCache) check(requestID, fingerprint string, now time.Time, skew time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	if c.seq%1024 == 0 {
		c.sweepLocked(now, skew)
	}
	if prev, ok := c.m[requestID]; ok {
		return prev.fingerprint != fingerprint
	}
	c.m[requestID] = replayEntry{fingerprint: fingerprint, seenAt: now}
	return false
}

// sweepLocked drops entries older than the skew window (they can no longer be
// replayed) and keeps the map from growing without limit under a request-ID
// flood.
func (c *mxReplayCache) sweepLocked(now time.Time, skew time.Duration) {
	if skew <= 0 {
		skew = mxwire.MaxSignatureSkew
	}
	for id, e := range c.m {
		if now.Sub(e.seenAt) > skew {
			delete(c.m, id)
		}
	}
	if len(c.m) > 1<<16 {
		c.m = map[string]replayEntry{}
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
const mxSignatureHeader = "X-MailMoose-MX-Signature"

func (s *Server) mxKey(keyID string) ([]byte, bool) {
	secret, ok := s.Service.Config.MXEdgeKeys[keyID]
	return []byte(secret), ok
}

// mxSignedMeta is the shared signed prefix every MX request carries.
type mxSignedMeta struct {
	Version   string `json:"version"`
	KeyID     string `json:"key_id"`
	Timestamp int64  `json:"timestamp"`
	RequestID string `json:"request_id"`
}

// verifyMXResolve reads and verifies a bounded resolve request (a framed
// metadata prelude plus a small JSON body).
func (s *Server) verifyMXResolve(w http.ResponseWriter, r *http.Request) ([]byte, []byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, mxwire.MaxMetadataBytes+64<<10))
	if err != nil {
		writeError(w, 400, "request read failed")
		return nil, nil, false
	}
	metaBytes, bodyBytes, err := mxwire.SplitPreludeBytes(body)
	if err != nil {
		writeError(w, 400, err.Error())
		return nil, nil, false
	}
	if !s.verifyMXSignature(w, r, metaBytes, mxwire.MetaDigest(metaBytes), mxwire.BodyDigest(bodyBytes)) {
		return nil, nil, false
	}
	return metaBytes, bodyBytes, true
}

// verifyMXSignature validates version/skew/key/signature and the replay rule
// over the supplied digests. It writes the error response itself.
func (s *Server) verifyMXSignature(w http.ResponseWriter, r *http.Request, metaBytes []byte, metaDigest, bodyDigest string) bool {
	var meta mxSignedMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		writeError(w, 400, "invalid metadata")
		return false
	}
	key, ok := s.mxKey(meta.KeyID)
	if !ok {
		writeError(w, 401, "unauthorized")
		return false
	}
	sig := strings.TrimSpace(r.Header.Get(mxSignatureHeader))
	now := time.Now()
	if err := mxwire.Verify(key, meta.Version, meta.KeyID, meta.Timestamp, meta.RequestID, r.Method, r.URL.Path, metaDigest, bodyDigest, sig, now, s.Service.Config.MXSignatureSkew); err != nil {
		if errors.Is(err, mxwire.ErrVersion) || errors.Is(err, mxwire.ErrSkew) {
			writeError(w, 400, err.Error())
			return false
		}
		// A bad signature is a temporary condition for the edge (retryable), but
		// also warrants an operator alarm. Do not leak which check failed.
		writeError(w, 401, "unauthorized")
		return false
	}
	fp := requestFingerprint(r.Method, r.URL.Path, metaBytes, bodyDigest)
	if s.mxReplay.check(meta.RequestID, fp, now, s.Service.Config.MXSignatureSkew) {
		writeError(w, 409, "replayed request id")
		return false
	}
	return true
}

func requestFingerprint(method, path string, meta []byte, bodyDigest string) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(meta)
	h.Write([]byte{0})
	h.Write([]byte(bodyDigest))
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Server) mxResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	// Share the single bounded ingress budget with provider webhooks so MX is
	// not an unbounded alternate path.
	select {
	case s.inboundSem <- struct{}{}:
		defer func() { <-s.inboundSem }()
	case <-r.Context().Done():
		writeError(w, 499, "request cancelled")
		return
	}
	_, bodyBytes, ok := s.verifyMXResolve(w, r)
	if !ok {
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
	// Share the single bounded ingress budget with provider webhooks so MX is
	// not an unbounded alternate path.
	select {
	case s.inboundSem <- struct{}{}:
		defer func() { <-s.inboundSem }()
	case <-r.Context().Done():
		writeError(w, 499, "request cancelled")
		return
	}
	bodyCap := s.Service.Config.MaxMessageBytes
	if bodyCap <= 0 {
		bodyCap = mxwire.DefaultMaxBodyBytes
	}
	// Read the bounded metadata prelude first. The declared ContentDigest is
	// part of the signed canonical string, so the HMAC can be verified before a
	// single byte of the body is written to disk. This keeps unauthenticated
	// callers from forcing staging writes: only a request carrying a valid
	// signature and replay identity proceeds to the body.
	metaBytes, err := mxwire.ReadPrelude(r.Body)
	if err != nil {
		writeError(w, 400, "invalid metadata prelude")
		return
	}
	var meta mxwire.IngestMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		writeError(w, 400, "invalid metadata")
		return
	}
	if len(meta.Recipients) == 0 {
		writeError(w, 400, "no recipients")
		return
	}
	// Verify against the digest the edge declared and signed; the body is then
	// streamed and checked against it, so a mismatch is still caught after.
	if !s.verifyMXSignature(w, r, metaBytes, mxwire.MetaDigest(metaBytes), meta.ContentDigest) {
		return
	}
	tmp, size, digest, ok := s.stageMXStream(w, r, bodyCap)
	if !ok {
		return
	}
	defer removeFile(tmp)
	if size != meta.Size {
		writeError(w, 400, "size mismatch")
		return
	}
	if digest != meta.ContentDigest {
		writeError(w, 400, "content digest mismatch")
		return
	}
	res, err := s.Service.IngestMX(r.Context(), app.MXIngestInput{
		Recipients:        meta.Recipients,
		EnvelopeFrom:      meta.EnvelopeFrom,
		RawPath:           tmp,
		Size:              meta.Size,
		ContentDigest:     digest,
		AuthResults:       meta.AuthResults,
		TrustedAuth:       true,
		ProviderMessageID: meta.ProviderMessageID,
	})
	if err != nil {
		if errors.Is(err, app.ErrMXDisabled) {
			writeError(w, 404, "mx disabled")
			return
		}
		s.Log.Warn("mx ingest failed", "recipients", meta.Recipients, "from", meta.EnvelopeFrom, "edge", meta.Edge, "error", err)
		writeError(w, 500, "ingest failed")
		return
	}
	code := aggregateIngestCode(res.PerRecipient)
	if code == mxwire.CodeOK {
		s.Log.Info("mx ingest accepted", "recipients", meta.Recipients, "from", meta.EnvelopeFrom, "edge", meta.Edge, "size", meta.Size, "message_id", res.MessageID)
	} else {
		reasons := make([]string, 0, len(res.PerRecipient))
		for _, rr := range res.PerRecipient {
			reasons = append(reasons, rr.Recipient+"="+string(rr.MachineCode))
		}
		s.Log.Warn("mx ingest rejected", "recipients", meta.Recipients, "from", meta.EnvelopeFrom, "edge", meta.Edge, "codes", reasons)
	}
	writeJSON(w, 200, mxwire.IngestResponse{
		Version:      mxwire.ProtocolVersion,
		MachineCode:  code,
		MessageID:    res.MessageID,
		PerRecipient: res.PerRecipient,
	})
}

// aggregateIngestCode summarizes a fan-out result: a quota failure dominates, a
// transient/unknown failure makes the whole request temporary, otherwise OK.
func aggregateIngestCode(results []mxwire.RecipientIngestResult) mxwire.MachineCode {
	code := mxwire.CodeOK
	for _, r := range results {
		switch r.MachineCode {
		case mxwire.CodeOK, mxwire.CodeDuplicate:
		case mxwire.CodeQuota:
			if code != mxwire.CodeTempFail {
				code = mxwire.CodeQuota
			}
		default:
			code = mxwire.CodeTempFail
		}
	}
	return code
}

// stageMXStream writes the post-prelude request body to a 0600 temp file under
// the messages staging tree, bounded by maxBytes, returning its path, size and
// hex SHA-256. It mirrors the provider adapters' staging contract so the shared
// delivery primitive's os.Rename stays on one filesystem.
func (s *Server) stageMXStream(w http.ResponseWriter, r *http.Request, maxBytes int64) (string, int64, string, bool) {
	dir := filepath.Join(s.Service.Config.DataDir, "messages", ".tmp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeError(w, 500, "staging failed")
		return "", 0, "", false
	}
	f, err := os.CreateTemp(dir, "mx-*.eml")
	if err != nil {
		writeError(w, 500, "staging failed")
		return "", 0, "", false
	}
	path := f.Name()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r.Body, maxBytes+1))
	cerr := f.Close()
	if err != nil || cerr != nil {
		os.Remove(path)
		writeError(w, 400, "request read failed")
		return "", 0, "", false
	}
	if n > maxBytes {
		os.Remove(path)
		writeError(w, 413, "request too large")
		return "", 0, "", false
	}
	return path, n, hex.EncodeToString(h.Sum(nil)), true
}

func removeFile(path string) { _ = os.Remove(path) }
