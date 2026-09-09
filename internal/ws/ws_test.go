package ws

import (
	"bufio"
	"io"
	"net"
	"testing"
	"time"
)

// A write after an idle period longer than the write timeout must succeed: the
// deadline set for an earlier frame must not leak into later frames (the reader
// writes pongs without setting one, which used to fail and drop the socket).
func TestWriteFrameRefreshesDeadline(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go func() { _, _ = io.Copy(io.Discard, c2) }()
	conn := &Conn{c: c1, r: bufio.NewReader(c1), writeTimeout: 50 * time.Millisecond}
	if err := conn.writeFrameLocked(opText, []byte("a")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if err := conn.writeFrameLocked(opPong, []byte("p")); err != nil {
		t.Fatalf("write after idle failed: %v", err)
	}
}
