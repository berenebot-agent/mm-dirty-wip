package mxwire

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"
)

const (
	V2Protocol      = "mx-v2"
	SessionPath     = "/mx/v2/session"
	AuthLifetime    = 5 * time.Minute
	MaxFramePayload = 64 << 10
	ChunkSize       = 32 << 10
	FrameHeaderSize = 24
)

type FrameType uint8

const (
	FrameHello FrameType = iota + 1
	FrameReady
	FrameDomainAuth
	FrameChallenge
	FrameChallengeResponse
	FrameAuthResult
	FrameDomainRevoked
	FrameDomainUnregister
	FramePing
	FramePong
	FrameResolve
	FrameResolveResult
	FrameIngestStart
	FrameIngestChunk
	FrameIngestEnd
	FrameIngestResult
	FrameCancel
)

// Frame has a fixed network-byte-order header: version, type, reserved flags,
// payload length, connection-scoped transaction id, and domain channel id.
// Unknown types/flags are fatal: neither peer may guess transaction semantics.
type Frame struct {
	Type      FrameType
	TxID      uint64
	ChannelID uint64
	Payload   []byte
}

func validFrame(f Frame) error {
	if f.Type < FrameHello || f.Type > FrameCancel || len(f.Payload) > MaxFramePayload {
		return fmt.Errorf("mxwire: invalid frame type or length")
	}
	transaction := f.Type >= FrameResolve
	if transaction != (f.TxID != 0) {
		return fmt.Errorf("mxwire: invalid transaction id")
	}
	if !transaction && f.Type != FrameHello && f.Type != FrameReady && f.Type != FramePing && f.Type != FramePong && f.ChannelID == 0 {
		return fmt.Errorf("mxwire: missing domain channel")
	}
	return nil
}

func ReadFrame(r io.Reader) (Frame, error) {
	var header [FrameHeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}
	if header[0] != 2 || binary.BigEndian.Uint16(header[2:4]) != 0 {
		return Frame{}, fmt.Errorf("mxwire: unsupported frame version or flags")
	}
	n := binary.BigEndian.Uint32(header[4:8])
	if n > MaxFramePayload {
		return Frame{}, fmt.Errorf("mxwire: frame too large")
	}
	f := Frame{Type: FrameType(header[1]), TxID: binary.BigEndian.Uint64(header[8:16]), ChannelID: binary.BigEndian.Uint64(header[16:24])}
	if err := validFrame(f); err != nil {
		return Frame{}, err
	}
	f.Payload = make([]byte, int(n))
	_, err := io.ReadFull(r, f.Payload)
	return f, err
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func WriteFrame(w io.Writer, f Frame) error {
	if err := validFrame(f); err != nil {
		return err
	}
	var header [FrameHeaderSize]byte
	header[0], header[1] = 2, byte(f.Type)
	binary.BigEndian.PutUint32(header[4:8], uint32(len(f.Payload)))
	binary.BigEndian.PutUint64(header[8:16], f.TxID)
	binary.BigEndian.PutUint64(header[16:24], f.ChannelID)
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	return writeAll(w, f.Payload)
}

func JSONFrame(kind FrameType, tx, channel uint64, value any) (Frame, error) {
	b, err := json.Marshal(value)
	f := Frame{Type: kind, TxID: tx, ChannelID: channel, Payload: b}
	if err != nil {
		return f, err
	}
	if len(b) > MaxMetadataBytes {
		return f, fmt.Errorf("mxwire: metadata too large")
	}
	return f, validFrame(f)
}

func DecodeFrame(f Frame, value any) error {
	if len(f.Payload) > MaxMetadataBytes {
		return fmt.Errorf("mxwire: metadata too large")
	}
	d := json.NewDecoder(bytes.NewReader(f.Payload))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return fmt.Errorf("mxwire: invalid metadata: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("mxwire: trailing metadata")
	}
	return nil
}

func ChunkFrame(tx, channel uint64, seq uint32, data []byte) Frame {
	b := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(b[:4], seq)
	copy(b[4:], data)
	return Frame{Type: FrameIngestChunk, TxID: tx, ChannelID: channel, Payload: b}
}

func DecodeChunk(f Frame) (uint32, []byte, error) {
	if f.Type != FrameIngestChunk || len(f.Payload) < 5 || len(f.Payload) > ChunkSize+4 {
		return 0, nil, fmt.Errorf("mxwire: invalid chunk")
	}
	return binary.BigEndian.Uint32(f.Payload[:4]), f.Payload[4:], nil
}

type Hello struct {
	Version  string `json:"version"`
	Instance string `json:"instance"`
}

type Ready struct {
	Version         string `json:"version"`
	ReceiverID      string `json:"receiver_id"`
	ConnectionID    string `json:"connection_id"`
	SMTPHostname    string `json:"smtp_hostname"`
	MaxMessageBytes int64  `json:"max_message_bytes"`
}

type DomainAuth struct {
	Domain string `json:"domain"`
	KeyID  string `json:"key_id"`
}

type Challenge struct {
	Domain       string `json:"domain"`
	KeyID        string `json:"key_id"`
	ReceiverID   string `json:"receiver_id"`
	ConnectionID string `json:"connection_id"`
	Nonce        string `json:"nonce"`
}

type ChallengeResponse struct {
	Domain    string `json:"domain"`
	KeyID     string `json:"key_id"`
	Nonce     string `json:"nonce"`
	Signature string `json:"signature"`
}

type AuthResult struct {
	Domain    string    `json:"domain"`
	KeyID     string    `json:"key_id"`
	Reason    string    `json:"reason,omitempty"`
	Accepted  bool      `json:"accepted"`
	ExpiresAt time.Time `json:"expires_at"`
}

type DomainNotice struct {
	Domain string `json:"domain"`
	Reason string `json:"reason"`
}

type V2Resolve struct {
	Domain    string `json:"domain"`
	Recipient string `json:"recipient"`
}

type V2IngestStart struct {
	Domains  []string       `json:"domains"`
	Metadata IngestMetadata `json:"metadata"`
}

type V2IngestEnd struct {
	Size          int64  `json:"size"`
	ContentDigest string `json:"content_digest"`
}

// CanonicalDomain intentionally accepts DNS A-labels only. International names
// must be entered as punycode; SMTPUTF8 and address literals are not supported.
func CanonicalDomain(raw string) (string, error) {
	domain := strings.TrimSuffix(strings.ToLower(raw), ".")
	if len(domain) > 253 || !strings.Contains(domain, ".") || net.ParseIP(domain) != nil {
		return "", fmt.Errorf("mxwire: invalid domain")
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("mxwire: invalid domain label")
		}
		for _, ch := range []byte(label) {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return "", fmt.Errorf("mxwire: invalid domain character")
			}
		}
	}
	return domain, nil
}

func validKeyID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, ch := range []byte(id) {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return false
		}
	}
	return true
}

func DomainTXT(keyID string, public ed25519.PublicKey) string {
	return "v=MM1; k=ed25519; id=" + keyID + "; p=" + base64.StdEncoding.EncodeToString(public)
}

// ParseDomainTXT accepts one unambiguous MM1 record. DNS resolver libraries
// join multiple character strings within a TXT RR; separate RRs stay separate.
// Overlap rotation is deliberately not part of v1: ambiguous MM1 RRs fail closed.
func ParseDomainTXT(records []string, keyID string) (ed25519.PublicKey, error) {
	if !validKeyID(keyID) || len(records) > 32 {
		return nil, fmt.Errorf("mxwire: invalid TXT key selection")
	}
	var found ed25519.PublicKey
	matched := 0
	for _, raw := range records {
		if len(raw) > 1024 {
			return nil, fmt.Errorf("mxwire: oversized TXT")
		}
		fields := map[string]string{}
		bad := false
		for _, part := range strings.Split(raw, ";") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			k, v, ok := strings.Cut(part, "=")
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if !ok || fields[k] != "" || k == "" || v == "" {
				bad = true
			}
			fields[k] = v
		}
		if fields["v"] != "MM1" {
			continue
		}
		matched++
		if bad || len(fields) != 4 || fields["k"] != "ed25519" || fields["id"] != keyID {
			return nil, fmt.Errorf("mxwire: invalid MM1 TXT")
		}
		pub, err := base64.StdEncoding.Strict().DecodeString(fields["p"])
		if err != nil || len(pub) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("mxwire: invalid TXT public key")
		}
		found = ed25519.PublicKey(pub)
	}
	if matched != 1 {
		return nil, fmt.Errorf("mxwire: missing or ambiguous MM1 TXT")
	}
	return found, nil
}

// AuthTranscript returns the SHA256 of five length-prefixed UTF-8 strings, then
// the length-prefixed decoded nonce. Length prefixes are uint32 big endian.
// Ed25519 signs this digest as a message (not the Ed25519ph variant).
func AuthTranscript(c Challenge) ([]byte, error) {
	domain, err := CanonicalDomain(c.Domain)
	if err != nil || domain != c.Domain || !validKeyID(c.KeyID) {
		return nil, fmt.Errorf("mxwire: invalid authentication identity")
	}
	if len(c.ReceiverID) == 0 || len(c.ReceiverID) > 128 || len(c.ConnectionID) == 0 || len(c.ConnectionID) > 128 {
		return nil, fmt.Errorf("mxwire: invalid session binding")
	}
	nonce, err := base64.RawURLEncoding.Strict().DecodeString(c.Nonce)
	if err != nil || len(nonce) != 32 {
		return nil, fmt.Errorf("mxwire: invalid nonce")
	}
	h := sha256.New()
	for _, field := range [][]byte{[]byte("mailmoose-mx-v2"), []byte(c.ReceiverID), []byte(c.ConnectionID), []byte(domain), []byte(c.KeyID), nonce} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		h.Write(length[:])
		h.Write(field)
	}
	return h.Sum(nil), nil
}

func SignChallenge(private ed25519.PrivateKey, c Challenge) (string, error) {
	if len(private) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("mxwire: invalid signing key")
	}
	transcript, err := AuthTranscript(c)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(private, transcript)), nil
}

func VerifyChallenge(public ed25519.PublicKey, c Challenge, signature string) bool {
	if len(public) != ed25519.PublicKeySize {
		return false
	}
	transcript, err := AuthTranscript(c)
	if err != nil {
		return false
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(signature)
	return err == nil && len(sig) == ed25519.SignatureSize && ed25519.Verify(public, transcript, sig)
}

// ReceiverURL normalizes an operator-supplied HTTPS base URL. It is never a
// redirect target: dial clients must reject redirects and HTTP/1 fallback.
func ReceiverURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("mxwire: receiver must be an HTTPS base URL")
	}
	u.Path, u.RawPath = "", ""
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}
