// Package ws provides the small RFC 6455 server surface used by the Hermes
// Relay adapter. It intentionally implements only server-side JSON/text frames.
package ws

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	opContinue = 0x0
	opText     = 0x1
	opBinary   = 0x2
	opClose    = 0x8
	opPing     = 0x9
	opPong     = 0xA
	maxMessage = 2 << 20
)

type Upgrader struct{ CheckOrigin func(*http.Request) bool }

type Conn struct {
	c      net.Conn
	r      *bufio.Reader
	wmu    sync.Mutex
	closed bool
}

func tokenContains(v, want string) bool {
	for _, p := range strings.Split(v, ",") {
		if strings.EqualFold(strings.TrimSpace(p), want) {
			return true
		}
	}
	return false
}
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func (u Upgrader) Upgrade(w http.ResponseWriter, r *http.Request, responseHeader http.Header) (*Conn, error) {
	if r.Method != http.MethodGet || !tokenContains(r.Header.Get("Connection"), "upgrade") || !strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket") || r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "invalid websocket upgrade", http.StatusBadRequest)
		return nil, errors.New("invalid websocket upgrade")
	}
	check := u.CheckOrigin
	if check == nil {
		check = sameOrigin
	}
	if !check(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return nil, errors.New("origin not allowed")
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(raw) != 16 {
		http.Error(w, "invalid websocket key", http.StatusBadRequest)
		return nil, errors.New("invalid websocket key")
	}
	h, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket unavailable", http.StatusInternalServerError)
		return nil, errors.New("hijacking unavailable")
	}
	nc, brw, err := h.Hijack()
	if err != nil {
		return nil, err
	}
	acceptRaw := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(acceptRaw[:])
	var b strings.Builder
	b.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ")
	b.WriteString(accept)
	b.WriteString("\r\n")
	for k, vs := range responseHeader {
		for _, v := range vs {
			if strings.ContainsAny(v, "\r\n") {
				continue
			}
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")
	if _, err = brw.WriteString(b.String()); err == nil {
		err = brw.Flush()
	}
	if err != nil {
		nc.Close()
		return nil, err
	}
	return &Conn{c: nc, r: brw.Reader}, nil
}

func (c *Conn) SetReadDeadline(t time.Time) error  { return c.c.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.c.SetWriteDeadline(t) }
func (c *Conn) Close() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	_ = c.writeFrameLocked(opClose, []byte{0x03, 0xE8})
	return c.c.Close()
}
func (c *Conn) WriteJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	// The relay contract is newline-delimited JSON: every frame must be
	// newline-terminated or the gateway's reader never dispatches it.
	return c.writeFrame(opText, append(b, '\n'))
}
func (c *Conn) ReadJSON(v any) error {
	_, b, err := c.readMessage()
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (c *Conn) writeFrame(op byte, p []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.writeFrameLocked(op, p)
}
func (c *Conn) writeFrameLocked(op byte, p []byte) error {
	if c.closed && op != opClose {
		return net.ErrClosed
	}
	hdr := make([]byte, 10)
	hdr[0] = 0x80 | op
	n := 2
	l := len(p)
	switch {
	case l < 126:
		hdr[1] = byte(l)
	case l <= 65535:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(l))
		n = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(l))
		n = 10
	}
	if _, err := c.c.Write(hdr[:n]); err != nil {
		return err
	}
	if l > 0 {
		_, err := c.c.Write(p)
		return err
	}
	return nil
}

type frame struct {
	fin  bool
	op   byte
	data []byte
}

func (c *Conn) readFrame() (frame, error) {
	var h [2]byte
	if _, err := io.ReadFull(c.r, h[:]); err != nil {
		return frame{}, err
	}
	fin := h[0]&0x80 != 0
	if h[0]&0x70 != 0 {
		return frame{}, errors.New("websocket: unsupported RSV bits")
	}
	op := h[0] & 0x0f
	masked := h[1]&0x80 != 0
	if !masked {
		return frame{}, errors.New("websocket: client frame is not masked")
	}
	ln := uint64(h[1] & 0x7f)
	if ln == 126 {
		var x [2]byte
		if _, err := io.ReadFull(c.r, x[:]); err != nil {
			return frame{}, err
		}
		ln = uint64(binary.BigEndian.Uint16(x[:]))
	} else if ln == 127 {
		var x [8]byte
		if _, err := io.ReadFull(c.r, x[:]); err != nil {
			return frame{}, err
		}
		ln = binary.BigEndian.Uint64(x[:])
		if ln>>63 != 0 {
			return frame{}, errors.New("websocket: invalid length")
		}
	}
	if op >= 8 && (!fin || ln > 125) {
		return frame{}, errors.New("websocket: invalid control frame")
	}
	if ln > maxMessage {
		return frame{}, errors.New("websocket: frame too large")
	}
	var mask [4]byte
	if _, err := io.ReadFull(c.r, mask[:]); err != nil {
		return frame{}, err
	}
	p := make([]byte, int(ln))
	if _, err := io.ReadFull(c.r, p); err != nil {
		return frame{}, err
	}
	for i := range p {
		p[i] ^= mask[i&3]
	}
	return frame{fin: fin, op: op, data: p}, nil
}
func (c *Conn) readMessage() (byte, []byte, error) {
	var op byte
	var out []byte
	fragmented := false
	for {
		f, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch f.op {
		case opPing:
			if err := c.writeFrame(opPong, f.data); err != nil {
				return 0, nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			_ = c.writeFrame(opClose, []byte{0x03, 0xE8})
			return 0, nil, io.EOF
		case opText, opBinary:
			if fragmented {
				return 0, nil, errors.New("websocket: new data frame during fragmented message")
			}
			op = f.op
			out = append(out, f.data...)
			if len(out) > maxMessage {
				return 0, nil, errors.New("websocket: message too large")
			}
			if f.fin {
				return op, out, nil
			}
			fragmented = true
		case opContinue:
			if !fragmented {
				return 0, nil, errors.New("websocket: unexpected continuation")
			}
			out = append(out, f.data...)
			if len(out) > maxMessage {
				return 0, nil, errors.New("websocket: message too large")
			}
			if f.fin {
				return op, out, nil
			}
		default:
			return 0, nil, errors.New("websocket: unsupported opcode")
		}
	}
}
