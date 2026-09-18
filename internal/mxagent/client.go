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

	"github.com/dellarb/mailmoose/internal/mxwire"
)

// CoreClient talks to the core's authenticated MX endpoints. Every request
// carries an HMAC-SHA256 signature over the metadata and body digests, so both
// ends can stream a large message through its hash without buffering it.
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

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

const mxSignatureHeader = "X-MailMoose-MX-Signature"

// post signs and sends one framed request: a 4-byte metadata length, the
// metadata JSON and then the raw body stream. metaBytes and bodyDigest are the
// exact transmitted metadata and the SHA-256 of the transmitted body, so the
// signature matches byte for byte on the server. bodyLen is the body size.
func (c *CoreClient) post(ctx context.Context, path string, metaBytes []byte, body io.Reader, bodyLen int64, bodyDigest string) ([]byte, int, error) {
	var meta mxMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, 0, err
	}
	sig := mxwire.Sign(c.secret, meta.Version, c.keyID, meta.Timestamp, meta.RequestID, http.MethodPost, path, mxwire.MetaDigest(metaBytes), bodyDigest)

	var prelude bytes.Buffer
	if err := mxwire.WritePrelude(&prelude, metaBytes); err != nil {
		return nil, 0, err
	}
	payload := io.MultiReader(bytes.NewReader(prelude.Bytes()), body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, payload)
	if err != nil {
		return nil, 0, err
	}
	// A known length avoids chunked transfer encoding through reverse proxies.
	req.ContentLength = int64(prelude.Len()) + bodyLen
	req.Header.Set("Content-Type", mxwire.IngestContentType)
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

// Resolve asks the core for routing decisions for the given recipients. The
// request body is tiny and framed like ingest for one code path.
func (c *CoreClient) Resolve(ctx context.Context, recipients []string) (mxwire.ResolveResponse, error) {
	meta := mxwire.ResolveRequest{
		Version: mxwire.ProtocolVersion, KeyID: c.keyID, Timestamp: time.Now().Unix(),
		RequestID: newRequestID(), Edge: c.edge,
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return mxwire.ResolveResponse{}, err
	}
	bodyBytes, err := json.Marshal(mxwire.ResolveBody{Recipients: recipients})
	if err != nil {
		return mxwire.ResolveResponse{}, err
	}
	b, status, err := c.post(ctx, mxwire.PathResolve, metaBytes, bytes.NewReader(bodyBytes), int64(len(bodyBytes)), mxwire.BodyDigest(bodyBytes))
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

// Ingest streams one staged message body to the core for the whole accepted
// recipient set. The core fans out internally; the response carries one result
// per recipient. body is read once from its current position.
func (c *CoreClient) Ingest(ctx context.Context, meta mxwire.IngestMetadata, body io.Reader, size int64, digest string) (mxwire.IngestResponse, error) {
	meta.Version = mxwire.ProtocolVersion
	meta.KeyID = c.keyID
	meta.Timestamp = time.Now().Unix()
	if meta.RequestID == "" {
		meta.RequestID = newRequestID()
	}
	if meta.Edge == "" {
		meta.Edge = c.edge
	}
	meta.Size = size
	meta.ContentDigest = digest
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return mxwire.IngestResponse{}, err
	}
	b, status, err := c.post(ctx, mxwire.PathIngest, metaBytes, body, size, digest)
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
