package mxagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gatehouse-mail/internal/mxwire"
)

// CoreClient talks to the core's authenticated MX endpoints. Every request
// carries an HMAC-SHA256 signature over a bounded JSON envelope whose exact
// transmitted metadata and body bytes are hashed.
type CoreClient struct {
	base    string
	keyID   string
	secret  []byte
	edge    string
	http    *http.Client
	timeout time.Duration
}

func NewCoreClient(cfg Config) *CoreClient {
	return &CoreClient{
		base:    cfg.IngestURL,
		keyID:   cfg.KeyID,
		secret:  []byte(cfg.Secret),
		edge:    cfg.EdgeName,
		http:    &http.Client{Timeout: 3 * time.Minute},
		timeout: 3 * time.Minute,
	}
}

type envelope struct {
	Metadata string `json:"metadata"`
	Body     string `json:"body"`
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// post signs and sends one request. metaBytes and bodyBytes are the exact bytes
// that will be transmitted in the envelope, so the signature matches byte for
// byte on the server.
func (c *CoreClient) post(ctx context.Context, path string, metaBytes, bodyBytes []byte) ([]byte, int, error) {
	var meta mxMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, 0, err
	}
	sig := mxwire.Sign(c.secret, meta.Version, c.keyID, meta.Timestamp, meta.RequestID, http.MethodPost, path, metaBytes, bodyBytes)
	payload, err := json.Marshal(envelope{Metadata: string(metaBytes), Body: string(bodyBytes)})
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(mxSignatureHeader, sig)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return b, resp.StatusCode, nil
}

// mxMeta is the shared prefix of every signed metadata blob.
type mxMeta struct {
	Version   string `json:"version"`
	KeyID     string `json:"key_id"`
	Timestamp int64  `json:"timestamp"`
	RequestID string `json:"request_id"`
}

const mxSignatureHeader = "X-Gatehouse-MX-Signature"

// Resolve asks the core for routing decisions for the given recipients.
func (c *CoreClient) Resolve(ctx context.Context, recipients []string) (mxwire.ResolveResponse, error) {
	meta := mxwire.ResolveRequest{
		Version: mxwire.ProtocolVersion, KeyID: c.keyID, Timestamp: time.Now().Unix(),
		RequestID: newRequestID(), Edge: c.edge,
	}
	body := mxwire.ResolveBody{Recipients: recipients}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return mxwire.ResolveResponse{}, err
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return mxwire.ResolveResponse{}, err
	}
	b, status, err := c.post(ctx, mxwire.PathResolve, metaBytes, bodyBytes)
	if err != nil {
		return mxwire.ResolveResponse{}, err
	}
	if status != http.StatusOK {
		return mxwire.ResolveResponse{}, fmt.Errorf("resolve: core returned %d", status)
	}
	var out mxwire.ResolveResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return mxwire.ResolveResponse{}, err
	}
	return out, nil
}

// Ingest hands one recipient's original MIME to the core. body is the raw
// message; the metadata carries the digest and normalized evidence. A duplicate
// returns the recorded disposition (MachineCode duplicate).
func (c *CoreClient) Ingest(ctx context.Context, meta mxwire.IngestMetadata, body []byte) (mxwire.IngestResponse, error) {
	meta.Version = mxwire.ProtocolVersion
	meta.KeyID = c.keyID
	meta.Timestamp = time.Now().Unix()
	if meta.RequestID == "" {
		meta.RequestID = newRequestID()
	}
	if meta.Edge == "" {
		meta.Edge = c.edge
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return mxwire.IngestResponse{}, err
	}
	b, status, err := c.post(ctx, mxwire.PathIngest, metaBytes, body)
	if err != nil {
		return mxwire.IngestResponse{}, err
	}
	if status != http.StatusOK {
		return mxwire.IngestResponse{}, fmt.Errorf("ingest: core returned %d: %s", status, strings.TrimSpace(string(b)))
	}
	var out mxwire.IngestResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return mxwire.IngestResponse{}, err
	}
	return out, nil
}
