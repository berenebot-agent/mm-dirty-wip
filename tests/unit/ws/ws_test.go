package ws_test

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gatehouse-mail/internal/ws"
)

// TestWriteJSONFramesAcrossIdle writes two JSON frames separated by an idle
// period and reads both back. The write path must not leave a stale per-frame
// deadline behind that fails a later write after the socket has been idle.
func TestWriteJSONFramesAcrossIdle(t *testing.T) {
	up := ws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	errCh := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			errCh <- err
			return
		}
		defer c.Close()
		if err := c.WriteJSON(map[string]string{"n": "one"}); err != nil {
			errCh <- err
			return
		}
		time.Sleep(80 * time.Millisecond)
		errCh <- c.WriteJSON(map[string]string{"n": "two"})
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := "GET / HTTP/1.1\r\nHost: " + addr + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	for _, want := range []string{"one", "two"} {
		var h [2]byte
		if _, err := io.ReadFull(br, h[:]); err != nil {
			t.Fatalf("read frame header: %v", err)
		}
		ln := int(h[1] & 0x7f)
		if ln == 126 {
			var x [2]byte
			if _, err := io.ReadFull(br, x[:]); err != nil {
				t.Fatal(err)
			}
			ln = int(binary.BigEndian.Uint16(x[:]))
		}
		p := make([]byte, ln)
		if _, err := io.ReadFull(br, p); err != nil {
			t.Fatalf("read frame payload: %v", err)
		}
		var got map[string]string
		if err := json.Unmarshal(p, &got); err != nil {
			t.Fatal(err)
		}
		if got["n"] != want {
			t.Fatalf("frame = %v, want n=%s", got, want)
		}
	}
	if err := <-errCh; err != nil {
		t.Fatalf("server write: %v", err)
	}
}
