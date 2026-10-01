package netutil_test

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"sync"
	"testing"
)

// loopbackDNS is a minimal UDP DNS server used to make netutil's resolver
// deterministic. It answers A and AAAA questions from a mutable record map so a
// test can change an answer between calls and prove dial-time re-resolution.
type loopbackDNS struct {
	conn *net.UDPConn

	mu      sync.Mutex
	a       map[string]net.IP
	aaaa    map[string]net.IP
	queries int
}

func newLoopbackDNS(t *testing.T) *loopbackDNS {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	s := &loopbackDNS{conn: conn, a: map[string]net.IP{}, aaaa: map[string]net.IP{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 512)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			reply := s.answer(buf[:n])
			if reply != nil {
				_, _ = conn.WriteToUDP(reply, addr)
			}
		}
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		<-done
	})
	return s
}

func (s *loopbackDNS) setA(host string, ip net.IP) {
	s.mu.Lock()
	s.a[strings.TrimSuffix(strings.ToLower(host), ".")] = ip
	s.mu.Unlock()
}

func (s *loopbackDNS) setAAAA(host string, ip net.IP) {
	s.mu.Lock()
	s.aaaa[strings.TrimSuffix(strings.ToLower(host), ".")] = ip
	s.mu.Unlock()
}

func (s *loopbackDNS) queryCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queries
}

// resolver returns a *net.Resolver that sends every query to this server. The
// Go resolver (PreferGo) is used so the custom Dial is honoured.
func (s *loopbackDNS) resolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", s.conn.LocalAddr().String())
		},
	}
}

// answer parses one query and returns an A or AAAA response, or nil when the
// host is unknown (the client then sees NXDOMAIN-empty).
func (s *loopbackDNS) answer(query []byte) []byte {
	if len(query) < 12 {
		return nil
	}
	name, qtype, ok := parseQuestion(query)
	if !ok {
		return nil
	}
	s.mu.Lock()
	s.queries++
	var ip net.IP
	switch qtype {
	case 1:
		ip = s.a[name]
	case 28:
		ip = s.aaaa[name]
	}
	s.mu.Unlock()

	resp := make([]byte, 0, 12+len(query)-12+16)
	resp = append(resp, query[:2]...) // transaction id
	// QR=1, RD=1, RA=1 (0x8180), no error.
	resp = append(resp, 0x81, 0x80)
	resp = append(resp, 0x00, 0x01) // QDCOUNT
	if ip == nil {
		resp = append(resp, 0x00, 0x00) // ANCOUNT
	} else {
		resp = append(resp, 0x00, 0x01) // ANCOUNT
	}
	resp = append(resp, 0x00, 0x00) // NSCOUNT
	resp = append(resp, 0x00, 0x00) // ARCOUNT
	// Echo the question section verbatim (QNAME + QTYPE + QCLASS).
	question, ok := questionBytes(query)
	if !ok {
		return nil
	}
	resp = append(resp, question...)
	if ip == nil {
		return resp
	}
	// Answer: pointer to the name at offset 12, then the address record.
	resp = append(resp, 0xc0, 0x0c)
	resp = append(resp, byte(qtype>>8), byte(qtype))
	resp = append(resp, 0x00, 0x01)             // CLASS IN
	resp = append(resp, 0x00, 0x00, 0x00, 0x3c) // TTL 60
	rdata := dnsRData(ip)
	resp = append(resp, byte(len(rdata)>>8), byte(len(rdata)))
	resp = append(resp, rdata...)
	return resp
}

func parseQuestion(q []byte) (name string, qtype uint16, ok bool) {
	name, rest, ok := parseName(q[12:])
	if !ok || len(rest) < 4 {
		return "", 0, false
	}
	return name, binary.BigEndian.Uint16(rest[0:2]), true
}

func questionBytes(q []byte) ([]byte, bool) {
	_, rest, ok := parseName(q[12:])
	if !ok || len(rest) < 4 {
		return nil, false
	}
	off := len(q) - len(rest)
	return q[12 : off+4], true
}

// parseName decodes a DNS name (no compression in queries) and returns the
// normalised name plus the bytes after it.
func parseName(b []byte) (string, []byte, bool) {
	var labels []string
	i := 0
	for {
		if i >= len(b) {
			return "", nil, false
		}
		l := int(b[i])
		i++
		if l == 0 {
			break
		}
		if l&0xc0 != 0 || i+l > len(b) {
			return "", nil, false
		}
		labels = append(labels, string(b[i:i+l]))
		i += l
	}
	return strings.ToLower(strings.Join(labels, ".")), b[i:], true
}

func dnsRData(ip net.IP) []byte {
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip.To16()
}
