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
		if e := t.resolveOne(ctx, address, &out); e != nil {
			return out, e
		}
	}
	return out, nil
}

// resolveOne routes a single recipient. It always emits exactly one routing
// record through a deferred completion, so every path — early rejection,
// timeout, protocol error or a core decision — reports the actual selected
// authority and core identity, never a guessed default.
func (t *transaction) resolveOne(ctx context.Context, address string, out *mxwire.ResolveResponse) (retErr error) {
	started := time.Now()
	rc := &resolveCompletion{recipient: address, selected: "none", outcome: "rejected"}
	defer rc.finish(t, ctx, started)

	at := strings.LastIndex(address, "@")
	if at < 1 {
		rc.reason = "invalid_address"
		out.Results = append(out.Results, mxwire.ResolveRecipient{Recipient: address})
		return nil
	}
	d, e := mxwire.CanonicalDomain(address[at+1:])
	if e != nil {
		rc.domain = address[at+1:]
		rc.reason = "invalid_domain"
		out.Results = append(out.Results, mxwire.ResolveRecipient{Recipient: address})
		return nil
	}
	rc.domain = d
	b, e := t.r.lookup(d)
	if e != nil {
		rc.selected, rc.reason = "no_binding", "unavailable"
		out.Results = append(out.Results, mxwire.ResolveRecipient{Recipient: address, Temporary: true})
		return nil
	}
	rc.selected = "core"
	rc.coreConnectionID, rc.keyID, rc.transportID = b.c.id, b.keyID, b.c.transportID
	p, e := t.group(b)
	if e != nil {
		rc.selected, rc.reason = "core", "transaction_limit"
		t.failed = true
		return e
	}
	t.r.mu.Lock()
	p.bindings[d] = b
	p.ch = b.channel
	t.r.mu.Unlock()
	rc.wireTxID, rc.channel = p.tx, b.channel
	if e = t.r.send(b.c, mxwire.FrameResolve, p.tx, b.channel, mxwire.V2Resolve{Domain: d, Recipient: address}); e != nil {
		rc.reason = "send_failed"
		t.failed = true
		return e
	}
	select {
	case <-ctx.Done():
		t.r.release(b.c, p)
		t.failed = true
		rc.outcome, rc.reason = "timeout", "resolve_timeout"
		return ctx.Err()
	case <-b.c.ctx.Done():
		t.r.release(b.c, p)
		t.failed = true
		rc.reason = "connection_gone"
		return context.Canceled
	case f := <-p.result:
		t.r.mu.Lock()
		cancelled := p.cancelled
		t.r.mu.Unlock()
		if cancelled || f.Type == 0 {
			// Released by Close/revocation: do not report a result.
			t.failed = true
			rc.reason = "release"
			return fmt.Errorf("transaction canceled")
		}
		var response mxwire.ResolveResponse
		if e = mxwire.DecodeFrame(f, &response); e != nil {
			t.failed = true
			rc.reason = "bad_response"
			return e
		}
		for _, rr := range response.Results {
			if strings.EqualFold(rr.Recipient, address) {
				rr.Domain = d
				out.Results = append(out.Results, rr)
				if rr.Accept {
					t.accepted[strings.ToLower(address)] = b
					rc.outcome = "accepted"
				} else if rr.Temporary {
					rc.outcome, rc.reason = "temporary", "routing"
				} else {
					rc.reason = "unknown"
				}
				return nil
			}
		}
		out.Results = append(out.Results, mxwire.ResolveRecipient{Recipient: address, Domain: d, Temporary: true})
		rc.outcome, rc.reason = "temporary", "no_result"
		return nil
	}
}

// resolveCompletion accumulates the actual per-recipient routing facts so the
// deferred finish can log one routing event with the real values.
type resolveCompletion struct {
	recipient, domain         string
	selected, outcome, reason string
	coreConnectionID, keyID   string
	transportID               string
	wireTxID, channel         uint64
}

func (rc *resolveCompletion) finish(t *transaction, ctx context.Context, started time.Time) {
	log := t.r.log
	if log == nil {
		return
	}
	args := txnLog(ctx,
		"recipient", rc.recipient,
		"domain", rc.domain,
		"selected", rc.selected,
		"outcome", rc.outcome,
		"duration_ms", time.Since(started).Milliseconds(),
	)
	if rc.coreConnectionID != "" {
		args = append(args, "core_connection_id", rc.coreConnectionID)
	}
	if rc.transportID != "" {
		args = append(args, "transport_id", rc.transportID)
	}
	if rc.keyID != "" {
		args = append(args, "key_id", rc.keyID)
	}
	if rc.channel != 0 {
		args = append(args, "channel", rc.channel)
	}
	if rc.wireTxID != 0 {
		args = append(args, "wire_transaction_id", rc.wireTxID)
	}
	if rc.reason != "" {
		args = append(args, "reason", rc.reason)
	}
	log.Info(eventResolve, args...)
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
		started := time.Now()
		t.r.log.Info(eventHandoff, txnLog(ctx,
			"handoff_id", handoffID(c.id, p.tx), "core_connection_id", c.id,
			"wire_transaction_id", p.tx, "phase", "start", "outcome", "pending",
			"recipients", recips, "domains", domains[c], "size", size, "digest", digest)...)
		// A deferred completion guarantees the terminal handoff record is
		// emitted on every path, carrying the actual bytes streamed and the
		// right outcome for the path taken.
		hc := &handoffCompletion{
			recipients: recips,
			domains:    domains[c],
			size:       size,
			digest:     digest,
		}
		hErr := func() error {
			defer hc.finish(t, ctx, c, p, started)
			for _, d := range domains[c] {
				b := t.acceptedBinding(c, d)
				if b == nil || !t.live(b) {
					t.failed = true
					_ = c.writer.write(mxwire.Frame{Type: mxwire.FrameCancel, TxID: p.tx})
					t.r.release(c, p)
					hc.phase, hc.outcome, hc.reason = "preflight", "fail", "pinned_unavailable"
					return fmt.Errorf("pinned destination revoked or expired")
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
				hc.phase, hc.outcome, hc.reason = "start", "fail", "start_send_failed"
				return e
			}
			n, e := t.streamBody(c, p, body, size, digest, buf)
			hc.streamed = n
			if e != nil {
				t.failed = true
				_ = c.writer.write(mxwire.Frame{Type: mxwire.FrameCancel, TxID: p.tx})
				t.r.release(c, p)
				hc.phase, hc.outcome, hc.reason = "stream", "fail", boundedReason(e)
				return e
			}
			if e := t.r.send(c, mxwire.FrameIngestEnd, p.tx, 0, mxwire.V2IngestEnd{Size: size, ContentDigest: digest}); e != nil {
				t.failed = true
				t.r.release(c, p)
				hc.phase, hc.outcome, hc.reason = "end", "unknown", "end_send_failed"
				return e
			}
			// From here the core may have durably committed the message, so a
			// wait/protocol failure is classified "unknown", never "fail".
			// The failed transaction still closes; a new SMTP retry deduplicates.
			timer, cancel := context.WithTimeout(ctx, t.r.cfg.IngestTimeout)
			select {
			case <-timer.Done():
				t.failed = true
				cancel()
				_ = c.writer.write(mxwire.Frame{Type: mxwire.FrameCancel, TxID: p.tx})
				t.r.release(c, p)
				hc.phase, hc.outcome, hc.reason = "await", "unknown", "timeout"
				return timer.Err()
			case <-c.ctx.Done():
				t.failed = true
				cancel()
				t.r.release(c, p)
				hc.phase, hc.outcome, hc.reason = "await", "unknown", "connection_gone"
				return context.Canceled
			case f := <-p.result:
				cancel()
				t.r.mu.Lock()
				cancelled := p.cancelled
				t.r.mu.Unlock()
				if cancelled || f.Type == 0 {
					t.failed = true
					hc.phase, hc.outcome, hc.reason = "await", "unknown", "release"
					return fmt.Errorf("transaction canceled")
				}
				var response mxwire.IngestResponse
				if e := mxwire.DecodeFrame(f, &response); e != nil {
					t.failed = true
					hc.phase, hc.outcome, hc.reason = "result", "unknown", "bad_response"
					return e
				}
				if e := validateIngest(response, recips); e != nil {
					t.failed = true
					hc.phase, hc.outcome, hc.reason = "result", "unknown", "invalid_result"
					return e
				}
				for _, rr := range response.PerRecipient {
					t.logHandoffResult(ctx, c, p.tx, rr)
				}
				// A valid response is "acknowledged", not "ok": a quota or
				// transient per-recipient code is still an acknowledgement.
				hc.phase, hc.outcome = "result", "acknowledged"
				hc.acked = len(response.PerRecipient)
				out.PerRecipient = append(out.PerRecipient, response.PerRecipient...)
				return nil
			}
		}()
		if hErr != nil {
			return out, hErr
		}
	}
	return out, nil
}

// handoffCompletion accumulates the actual per-connection handoff facts so the
// deferred finish emits exactly one terminal handoff record with the real
// outcome, byte count and identity on every path.
type handoffCompletion struct {
	recipients, domains []string
	size, streamed      int64
	digest              string
	phase               string
	outcome, reason     string
	acked               int
}

func (hc *handoffCompletion) finish(t *transaction, ctx context.Context, c *connection, p *pending, started time.Time) {
	log := t.r.log
	if log == nil {
		return
	}
	if hc.outcome == "" {
		hc.outcome = "fail"
	}
	// On a failure before any bytes were streamed, report the bytes actually
	// written (hc.streamed), never the full intended size that was not sent.
	bytes := hc.streamed
	if hc.outcome == "acknowledged" {
		bytes = hc.size
	}
	args := txnLog(ctx,
		"handoff_id", handoffID(c.id, p.tx),
		"core_connection_id", c.id,
		"wire_transaction_id", p.tx,
		"phase", hc.phase,
		"outcome", hc.outcome,
		"recipients", hc.recipients,
		"domains", hc.domains,
		"bytes", bytes,
		"digest", hc.digest,
		"duration_ms", time.Since(started).Milliseconds(),
	)
	if hc.acked > 0 {
		args = append(args, "acked", hc.acked)
	}
	if hc.reason != "" {
		args = append(args, "reason", hc.reason)
	}
	log.Info(eventHandoff, args...)
}

// logHandoffResult records the durable per-recipient outcome the core reported,
// keyed by the full recipient address so a reader can correlate a decision to
// the exact recipient.
func (t *transaction) logHandoffResult(ctx context.Context, c *connection, tx uint64, rr mxwire.RecipientIngestResult) {
	log := t.r.log
	if log == nil {
		return
	}
	args := txnLog(ctx,
		"recipient", rr.Recipient,
		"code", string(rr.MachineCode),
		"disposition", string(rr.Disposition),
		"duplicate", rr.Duplicate,
	)
	if c != nil {
		args = append(args,
			"handoff_id", handoffID(c.id, tx),
			"core_connection_id", c.id,
			"wire_transaction_id", tx,
		)
	}
	if rr.MessageID != "" {
		args = append(args, "message_id", rr.MessageID)
	}
	if rr.Reason != "" {
		args = append(args, "reason", boundedField(rr.Reason))
	}
	log.Info(eventHandoffResult, args...)
}

// boundedField truncates a provider-supplied field to a bounded length so a
// log record can never be inflated by it.
func boundedField(s string) string {
	if len(s) > 128 {
		return s[:128]
	}
	return s
}

// streamBody rewrites the staged body once, chunk by chunk, verifying the exact
// size and digest. The caller supplies a reusable buffer, so fan-out across
// groups allocates once. It returns the number of bytes actually written, so a
// failed handoff never claims the full intended size was streamed.
func (t *transaction) streamBody(c *connection, p *pending, body io.Reader, size int64, digest string, buf []byte) (int64, error) {
	if _, e := seekStart(body); e != nil {
		return 0, e
	}
	h := sha256.New()
	body = io.LimitReader(body, size)
	var nbytes int64
	for seq := uint32(0); nbytes < size; seq++ {
		n, e := io.ReadFull(body, buf)
		if e != nil && e != io.ErrUnexpectedEOF {
			return nbytes, e
		}
		if n == 0 {
			return nbytes, io.ErrUnexpectedEOF
		}
		_, _ = h.Write(buf[:n])
		if e = c.writer.write(mxwire.ChunkFrame(p.tx, 0, seq, buf[:n])); e != nil {
			return nbytes, e
		}
		nbytes += int64(n)
		if e == io.ErrUnexpectedEOF {
			break
		}
	}
	if nbytes != size || hex.EncodeToString(h.Sum(nil)) != digest {
		return nbytes, fmt.Errorf("staged message integrity mismatch")
	}
	return nbytes, nil
}

// handoffID is the stable correlation key for one core connection's wire
// transaction. The core connection id is unique per session and the wire
// transaction id is unique per connection, so their pair identifies a handoff.
func handoffID(coreConn string, wireTx uint64) string {
	return fmt.Sprintf("%s:%d", coreConn, wireTx)
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
