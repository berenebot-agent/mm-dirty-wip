package receiver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dellarb/mailmoose/internal/mxagent"
	"github.com/dellarb/mailmoose/internal/mxwire"
)

// transaction is one SMTP session's delivery. It groups the recipients it
// resolves across one or more receiver connections into per-connection pending
// transaction groups, so a single message can be fanned out to several
// connections that each authenticate a different domain.
type transaction struct {
	r        *Receiver
	mu       sync.Mutex
	groups   map[*connection]*pending
	accepted map[string]*binding
	closed   bool
	// failed marks a transaction whose fan-out is no longer consistent. Once
	// set, no further group or recipient is admitted, so a partial failure can
	// never contaminate the next SMTP RCPT round.
	failed bool
}

func newTransaction(r *Receiver) *transaction {
	return &transaction{r: r, groups: map[*connection]*pending{}, accepted: map[string]*binding{}}
}

// group returns the pending group for a connection, creating it and accounting
// its domain quota on first use. The caller must hold t.mu.
func (t *transaction) group(b *binding) (*pending, error) {
	if p := t.groups[b.c]; p != nil {
		if e := t.r.countDomain(p, b.domain); e != nil {
			return nil, e
		}
		t.r.mu.Lock()
		p.bindings[b.domain] = b
		t.r.mu.Unlock()
		return p, nil
	}
	id := t.r.nextTx(b.c)
	p, e := t.r.slot(b.c, id, b.domain)
	if e != nil {
		return nil, e
	}
	t.r.mu.Lock()
	p.bindings[b.domain] = b
	p.expected = mxwire.FrameResolveResult
	p.ch = b.channel
	t.r.mu.Unlock()
	t.groups[b.c] = p
	return p, nil
}

func (t *transaction) Resolve(ctx context.Context, addresses []string) (mxwire.ResolveResponse, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := mxwire.ResolveResponse{Version: mxwire.V2Protocol}
	if t.closed || t.failed {
		return out, fmt.Errorf("transaction closed")
	}
	for _, address := range addresses {
		at := strings.LastIndex(address, "@")
		if at < 1 {
			out.Results = append(out.Results, mxwire.ResolveRecipient{Recipient: address})
			continue
		}
		d, e := mxwire.CanonicalDomain(address[at+1:])
		if e != nil {
			out.Results = append(out.Results, mxwire.ResolveRecipient{Recipient: address})
			continue
		}
		b, e := t.r.lookup(d)
		if e != nil {
			out.Results = append(out.Results, mxwire.ResolveRecipient{Recipient: address, Temporary: true})
			continue
		}
		p, e := t.group(b)
		if e != nil {
			t.failed = true
			return out, e
		}
		t.r.mu.Lock()
		p.bindings[d] = b
		p.ch = b.channel
		t.r.mu.Unlock()
		if e = t.r.send(b.c, mxwire.FrameResolve, p.tx, b.channel, mxwire.V2Resolve{Domain: d, Recipient: address}); e != nil {
			t.failed = true
			return out, e
		}
		select {
		case <-ctx.Done():
			t.r.release(b.c, p)
			t.failed = true
			return out, ctx.Err()
		case <-b.c.ctx.Done():
			t.r.release(b.c, p)
			t.failed = true
			return out, context.Canceled
		case f := <-p.result:
			t.r.mu.Lock()
			cancelled := p.cancelled
			t.r.mu.Unlock()
			if cancelled || f.Type == 0 {
				// Released by Close/revocation: do not report a result.
				t.failed = true
				return out, fmt.Errorf("transaction canceled")
			}
			var response mxwire.ResolveResponse
			if e = mxwire.DecodeFrame(f, &response); e != nil {
				t.failed = true
				return out, e
			}
			matched := false
			for _, rr := range response.Results {
				if strings.EqualFold(rr.Recipient, address) {
					rr.Domain = d
					out.Results = append(out.Results, rr)
					matched = true
					if rr.Accept {
						t.accepted[strings.ToLower(address)] = b
					}
					break
				}
			}
			if !matched {
				out.Results = append(out.Results, mxwire.ResolveRecipient{Recipient: address, Domain: d, Temporary: true})
			}
		}
	}
	return out, nil
}

// acceptedBinding returns the accepted binding for a domain on a connection.
func (t *transaction) acceptedBinding(c *connection, d string) *binding {
	for _, b := range t.accepted {
		if b.c == c && b.domain == d {
			return b
		}
	}
	return nil
}

// live reports whether a pinned binding may still carry DATA. A replaced
// binding is still live: it lost the registry to a newer binding but its own
// unexpired pinned DATA was already accepted. A revoked or expired binding is
// not.
func (t *transaction) live(b *binding) bool {
	t.r.mu.Lock()
	defer t.r.mu.Unlock()
	return (b.state == bindActive || b.state == bindReplaced) && !b.c.closing && time.Now().Before(b.expires)
}

func (t *transaction) Ingest(ctx context.Context, meta mxwire.IngestMetadata, body io.Reader, size int64, digest string) (mxwire.IngestResponse, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := mxwire.IngestResponse{Version: mxwire.V2Protocol}
	if t.closed || t.failed {
		return out, fmt.Errorf("transaction closed")
	}
	if size < 1 {
		t.failed = true
		return out, fmt.Errorf("invalid staged size")
	}
	// Order connections deterministically so fan-out is reproducible and a
	// retry groups recipients identically.
	groups := map[*connection][]string{}
	domains := map[*connection][]string{}
	for _, address := range meta.Recipients {
		b := t.accepted[strings.ToLower(address)]
		if b == nil || !t.live(b) {
			t.failed = true
			return out, fmt.Errorf("pinned destination unavailable")
		}
		groups[b.c] = append(groups[b.c], address)
		domains[b.c] = appendUnique(domains[b.c], b.domain)
	}
	conns := make([]*connection, 0, len(groups))
	for c := range groups {
		conns = append(conns, c)
	}
	sort.Slice(conns, func(i, j int) bool { return conns[i].id < conns[j].id })

	// One chunk buffer is reused for every group; the staged body is only ever
	// read one chunk at a time and never copied wholesale.
	buf := make([]byte, mxwire.ChunkSize)
	for _, c := range conns {
		recips := groups[c]
		p := t.groups[c]
		for _, d := range domains[c] {
			b := t.acceptedBinding(c, d)
			if b == nil || !t.live(b) {
				t.failed = true
				_ = c.writer.write(mxwire.Frame{Type: mxwire.FrameCancel, TxID: p.tx})
				t.r.release(c, p)
				return out, fmt.Errorf("pinned destination revoked or expired")
			}
		}
		t.r.mu.Lock()
		p.expected = mxwire.FrameIngestResult
		p.ch = 0
		p.domains = domains[c]
		for _, d := range domains[c] {
			if b := t.acceptedBinding(c, d); b != nil {
				p.bindings[d] = b
			}
		}
		t.r.mu.Unlock()
		md := meta
		md.Recipients = recips
		md.Size = size
		md.ContentDigest = digest
		if e := t.r.send(c, mxwire.FrameIngestStart, p.tx, 0, mxwire.V2IngestStart{Domains: domains[c], Metadata: md}); e != nil {
			t.failed = true
			t.r.release(c, p)
			return out, e
		}
		if e := t.streamBody(c, p, body, size, digest, buf); e != nil {
			t.failed = true
			_ = c.writer.write(mxwire.Frame{Type: mxwire.FrameCancel, TxID: p.tx})
			t.r.release(c, p)
			return out, e
		}
		if e := t.r.send(c, mxwire.FrameIngestEnd, p.tx, 0, mxwire.V2IngestEnd{Size: size, ContentDigest: digest}); e != nil {
			t.failed = true
			t.r.release(c, p)
			return out, e
		}
		timer, cancel := context.WithTimeout(ctx, t.r.cfg.IngestTimeout)
		select {
		case <-timer.Done():
			cancel()
			_ = c.writer.write(mxwire.Frame{Type: mxwire.FrameCancel, TxID: p.tx})
			t.r.release(c, p)
			t.failed = true
			return out, timer.Err()
		case <-c.ctx.Done():
			cancel()
			t.r.release(c, p)
			t.failed = true
			return out, context.Canceled
		case f := <-p.result:
			cancel()
			t.r.mu.Lock()
			cancelled := p.cancelled
			t.r.mu.Unlock()
			if cancelled || f.Type == 0 {
				t.failed = true
				return out, fmt.Errorf("transaction canceled")
			}
			var response mxwire.IngestResponse
			if e := mxwire.DecodeFrame(f, &response); e != nil {
				t.failed = true
				return out, e
			}
			if e := validateIngest(response, recips); e != nil {
				t.failed = true
				return out, e
			}
			out.PerRecipient = append(out.PerRecipient, response.PerRecipient...)
		}
	}
	return out, nil
}

// streamBody rewrites the staged body once, chunk by chunk, verifying the exact
// size and digest. The caller supplies a reusable buffer, so fan-out across
// groups allocates once.
func (t *transaction) streamBody(c *connection, p *pending, body io.Reader, size int64, digest string, buf []byte) error {
	if _, e := seekStart(body); e != nil {
		return e
	}
	h := sha256.New()
	body = io.LimitReader(body, size)
	var nbytes int64
	for seq := uint32(0); nbytes < size; seq++ {
		n, e := io.ReadFull(body, buf)
		if e != nil && e != io.ErrUnexpectedEOF {
			return e
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
		nbytes += int64(n)
		_, _ = h.Write(buf[:n])
		if e = c.writer.write(mxwire.ChunkFrame(p.tx, 0, seq, buf[:n])); e != nil {
			return e
		}
		if e == io.ErrUnexpectedEOF {
			break
		}
	}
	if nbytes != size || hex.EncodeToString(h.Sum(nil)) != digest {
		return fmt.Errorf("staged message integrity mismatch")
	}
	return nil
}

// seekStart rewinds a staged body. mxagent hands the receiver a *bytes.Reader,
// which already implements io.Seeker; io.Reader is the only declared contract.
func seekStart(body io.Reader) (int64, error) {
	s, ok := body.(io.Seeker)
	if !ok {
		return 0, fmt.Errorf("seekable stage required")
	}
	return s.Seek(0, io.SeekStart)
}

// validateIngest enforces the durable result contract: exactly one result per
// accepted recipient, only OK/duplicate machine codes, and a durable
// disposition for every OK result.
func validateIngest(response mxwire.IngestResponse, recips []string) error {
	want := map[string]bool{}
	for _, a := range recips {
		want[strings.ToLower(a)] = true
	}
	seen := map[string]bool{}
	for _, rr := range response.PerRecipient {
		k := strings.ToLower(rr.Recipient)
		if !want[k] || seen[k] {
			return fmt.Errorf("invalid result set")
		}
		seen[k] = true
		if rr.MachineCode != mxwire.CodeOK && rr.MachineCode != mxwire.CodeDuplicate {
			continue
		}
		if !validDisposition(rr.Disposition) {
			return fmt.Errorf("invalid durable disposition")
		}
	}
	if len(seen) != len(want) {
		return fmt.Errorf("incomplete result set")
	}
	return nil
}

func validDisposition(d mxwire.Disposition) bool {
	switch d {
	case mxwire.DispositionStored, mxwire.DispositionSpam, mxwire.DispositionBlocked, mxwire.DispositionControl:
		return true
	}
	return false
}

// Close cancels every pending group and releases its accounting exactly once.
// It never holds r.mu while writing: the cancel frame is written first, then the
// group is released, and any subsequent method call is rejected by the closed
// flag.
func (t *transaction) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	for c, p := range t.groups {
		_ = c.writer.write(mxwire.Frame{Type: mxwire.FrameCancel, TxID: p.tx})
		t.r.release(c, p)
	}
	t.groups = nil
	t.accepted = nil
	return nil
}

func appendUnique(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}

var _ mxagent.Delivery = (*transaction)(nil)
