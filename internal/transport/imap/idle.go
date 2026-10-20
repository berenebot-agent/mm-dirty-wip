package imap

import (
	"context"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// Notification is a change signal observed while watching a folder. It is
// deliberately coarse: the caller re-fetches the folder's headers after any
// signal rather than trusting a precise delta.
type Notification struct {
	Folder         string
	NumMessages    uint32
	HasNumMessages bool
	// Kind is "idle" for a server-pushed signal or "poll" for a polling tick.
	Kind string
}

// IdleCapable reports whether the server supports IDLE.
func (a *Adapter) IdleCapable() bool { return a.caps.Idle }

// notifySink receives unilateral mailbox updates from the imapclient handler.
// The handler runs on an arbitrary goroutine inside the client, so it must never
// block; it drops a signal when the buffer is full.
type notifySink struct {
	ch chan Notification
}

func newNotifySink(buffer int) *notifySink {
	if buffer <= 0 {
		buffer = 16
	}
	return &notifySink{ch: make(chan Notification, buffer)}
}

func (s *notifySink) mailbox(data *imapclient.UnilateralDataMailbox) {
	if data == nil {
		return
	}
	n := Notification{Kind: "idle"}
	if data.NumMessages != nil {
		n.NumMessages = *data.NumMessages
		n.HasNumMessages = true
	}
	select {
	case s.ch <- n:
	default:
	}
}

// Watch starts a realtime watch on folder using IDLE. It selects the folder
// read-only, issues IDLE and delivers a Notification for each server-pushed
// mailbox update. Because IDLE blocks the connection, callers should hold a
// dedicated Adapter per watched folder.
//
// When the server does not advertise IDLE, Watch returns an unsupported error;
// the caller can fall back to Poll.
//
// The returned stop function halts the watch and closes the channels; it is safe
// to call more than once. The error channel is closed when the watch ends,
// carrying the terminal error (nil for a clean stop).
func (a *Adapter) Watch(ctx context.Context, folder string, buffer int) (<-chan Notification, func(), <-chan error, error) {
	if !a.caps.Idle {
		return nil, nil, nil, Unsupported("the server does not advertise IDLE")
	}
	if _, err := a.selectReadOnly(ctx, folder); err != nil {
		return nil, nil, nil, err
	}

	if buffer <= 0 {
		buffer = 8
	}
	out := make(chan Notification, buffer)
	errCh := make(chan error, 1)

	a.mu.Lock()
	sink := a.sink
	conn := a.conn
	a.mu.Unlock()
	if conn == nil {
		close(out)
		errCh <- wrapErr(ErrNotConnected)
		close(errCh)
		return nil, nil, nil, wrapErr(ErrNotConnected)
	}
	if sink == nil {
		close(out)
		errCh <- Unsupported("this adapter has no notification handler")
		close(errCh)
		return nil, nil, nil, Unsupported("this adapter has no notification handler")
	}

	stopCh := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(stopCh) }) }

	go a.idleLoop(ctx, folder, sink, out, errCh, stopCh)
	return out, stop, errCh, nil
}

// idleLoop runs IDLE and forwards server mailbox updates.
func (a *Adapter) idleLoop(ctx context.Context, folder string, sink *notifySink, out chan<- Notification, errCh chan<- error, stopCh <-chan struct{}) {
	defer close(out)
	defer close(errCh)

	a.mu.Lock()
	conn := a.conn
	a.mu.Unlock()
	if conn == nil {
		errCh <- wrapErr(ErrNotConnected)
		return
	}

	idle, err := conn.Idle()
	if err != nil {
		a.clearSelected()
		errCh <- wrapErr(err)
		return
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-stopCh:
		case <-done:
		}
		_ = idle.Close()
	}()

	for {
		select {
		case <-ctx.Done():
			close(done)
			a.clearSelected()
			errCh <- nil
			return
		case <-stopCh:
			close(done)
			a.clearSelected()
			errCh <- nil
			return
		case <-conn.Closed():
			close(done)
			a.clearSelected()
			errCh <- wrapErr(ErrNotConnected)
			return
		case n := <-sink.ch:
			n.Folder = folder
			select {
			case out <- n:
			case <-ctx.Done():
				close(done)
				a.clearSelected()
				errCh <- nil
				return
			case <-stopCh:
				close(done)
				a.clearSelected()
				errCh <- nil
				return
			}
		}
	}
}

// Poll is the polling fallback for a server without IDLE (or when the caller
// prefers it). It watches folder's message count on each tick and delivers a
// Notification whenever it changes. The returned stop function halts it; it is
// safe to call more than once.
func (a *Adapter) Poll(ctx context.Context, folder string, interval time.Duration, buffer int) (<-chan Notification, func(), <-chan error, error) {
	if interval <= 0 {
		interval = defaultPollInterval
	}
	if buffer <= 0 {
		buffer = 4
	}
	ch := make(chan Notification, buffer)
	errCh := make(chan error, 1)

	data, err := a.Status(ctx, folder)
	if err != nil {
		close(ch)
		errCh <- err
		close(errCh)
		return nil, nil, nil, err
	}
	last := numMessages(data)

	stopCh := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(stopCh) }) }

	go func() {
		defer close(ch)
		defer close(errCh)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				errCh <- nil
				return
			case <-stopCh:
				errCh <- nil
				return
			case <-ticker.C:
				// STATUS does not select the folder, so no state is disturbed and
				// no clearSelected is needed.
				d, err := a.Status(ctx, folder)
				if err != nil {
					errCh <- err
					return
				}
				if n := numMessages(d); n != last {
					last = n
					select {
					case ch <- Notification{Folder: folder, NumMessages: n, HasNumMessages: true, Kind: "poll"}:
					case <-ctx.Done():
						errCh <- nil
						return
					case <-stopCh:
						errCh <- nil
						return
					}
				}
			}
		}
	}()
	return ch, stop, errCh, nil
}

// numMessages returns a STATUS result's message count.
func numMessages(d MailboxStatus) uint32 {
	return d.NumMessages
}

const defaultPollInterval = 60 * time.Second

// MailboxStatus is the provider-neutral result of a STATUS read: the live
// message count, the next UID, the UIDVALIDITY and the unseen count. It is the
// cheap poll surface and never selects the folder.
type MailboxStatus struct {
	NumMessages uint32
	UIDNext     uint32
	UIDValidity uint32
	Unseen      uint32
	// HighestModSeq is the mailbox's highest modification sequence when the
	// server advertises CONDSTORE, else 0.
	HighestModSeq uint64
}

// Status returns a mailbox's live status without selecting it, using the IMAP
// STATUS command. Unlike EXAMINE it does not disturb the currently selected
// folder, and it is cheaper than a full SELECT on a large mailbox.
func (a *Adapter) Status(ctx context.Context, folder string) (MailboxStatus, error) {
	a.mu.Lock()
	conn := a.conn
	caps := a.caps
	a.mu.Unlock()
	if conn == nil {
		return MailboxStatus{}, wrapErr(ErrNotConnected)
	}
	opts := &imap.StatusOptions{NumMessages: true, UIDNext: true, UIDValidity: true, NumUnseen: true}
	if caps.CondStore {
		opts.HighestModSeq = true
	}
	cmd := conn.Status(folder, opts)
	data, err := cmd.Wait()
	if err != nil {
		return MailboxStatus{}, wrapErr(err)
	}
	st := MailboxStatus{UIDNext: uint32(data.UIDNext), UIDValidity: data.UIDValidity, HighestModSeq: data.HighestModSeq}
	if data.NumMessages != nil {
		st.NumMessages = *data.NumMessages
	}
	if data.NumUnseen != nil {
		st.Unseen = *data.NumUnseen
	}
	return st, nil
}

// selectReadOnly selects a folder read-only, leaving it selected.
func (a *Adapter) selectReadOnly(ctx context.Context, path string) (*imap.SelectData, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn == nil {
		return nil, wrapErr(ErrNotConnected)
	}
	data, err := a.conn.Select(path, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		a.selected = ""
		return nil, wrapErr(err)
	}
	a.selected = path
	return data, nil
}
