package httpapp

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/dellarb/mailmoose/internal/app"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// This file is the common mailbox access seam the HTTP/API layer uses to read
// any inbox — a managed-domain inbox backed by the local store, or a standalone
// inbox backed by a live remote IMAP server — behind one dispatch on the inbox
// kind. It never branches on a provider type and never speaks IMAP itself: for
// a standalone inbox it calls the app.RemoteMailboxService surface, for a domain
// inbox it calls the local store exactly as before.
//
// The dispatcher resolves the backend once from the inbox kind and returns a
// small, provider-neutral accessor. Callers (the /v1 read routes and the UI) use
// the accessor's methods and never inspect the inbox kind; a new backend only
// has to implement the same accessor.

// mailboxBackend is the resolved read surface of one inbox.
type mailboxBackend struct {
	srv    *Server
	inbox  model.Inbox
	remote *app.RemoteMailboxService
	// routed reports whether this is a standalone/remote inbox. A domain inbox
	// leaves it false and every method falls back to the local store.
	routed bool
	p      model.Principal
}

// resolveMailbox loads an inbox the principal may read and classifies it. It is
// the single place the local/remote dispatch decision is made for the HTTP
// layer. A nil error guarantees the principal holds at least Read on the inbox
// (or is an account Admin). The returned inbox is the full internal model with
// its capability surface attached for a standalone inbox.
func (s *Server) resolveMailbox(ctx context.Context, p model.Principal, inboxID string) (mailboxBackend, error) {
	inboxID = strings.TrimSpace(inboxID)
	if inboxID == "" {
		return mailboxBackend{}, model.NewMailboxError(model.ErrKindInvalid, "inbox is required", false, nil)
	}
	if !p.CanRead(inboxID) && !p.Admin {
		return mailboxBackend{}, model.NewMailboxError(model.ErrKindForbidden, "not permitted", false, store.ErrForbidden)
	}
	inbox, err := s.Service.Store.GetInboxInternal(ctx, p.AccountID, inboxID)
	if err != nil {
		return mailboxBackend{}, normalizeMailboxStoreError(err)
	}
	mb := mailboxBackend{srv: s, inbox: inbox, p: p}
	if inbox.Kind == model.InboxKindStandalone {
		caps := model.StandaloneCapabilities()
		caps.Outbound = inbox.Remote != nil && inbox.Remote.SMTP != nil
		inbox.Capabilities = &caps
		mb.inbox = inbox
		mb.remote = s.remoteMailbox()
		mb.routed = true
	} else {
		caps := model.DomainCapabilities()
		inbox.Capabilities = &caps
		mb.inbox = inbox
	}
	return mb, nil
}

// remoteMailbox returns the Server's RemoteMailboxService, building it once on
// first use. The HTTP layer owns this instance; a test may install one on the
// Server (SetRemoteMailbox) before serving, e.g. carrying an in-memory dialer.
// It is built over the app Service, so the remote backend shares the process's
// store, config and encryption.
func (s *Server) remoteMailbox() *app.RemoteMailboxService {
	s.remoteOnce.Do(func() {
		if s.remoteMailboxSvc == nil {
			s.remoteMailboxSvc = app.NewRemoteMailboxService(s.Service)
		}
	})
	return s.remoteMailboxSvc
}

// SetRemoteMailbox installs the RemoteMailboxService the HTTP layer dispatches
// remote operations to. cmd/server may install the shared instance; a test
// installs one carrying an in-memory dialer. It must be called before the
// Server serves requests.
func (s *Server) SetRemoteMailbox(rm *app.RemoteMailboxService) {
	if rm != nil {
		s.remoteOnce.Do(func() { s.remoteMailboxSvc = rm })
	}
}

// Kind reports the routing decision.
func (m mailboxBackend) Kind() app.MailboxBackendKind {
	if m.routed {
		return app.BackendRemote
	}
	return app.BackendLocal
}

// Remote reports the non-secret remote description of a standalone inbox.
func (m mailboxBackend) Remote() *model.RemoteConnection { return m.inbox.Remote }

// Capabilities reports the declared capability surface.
func (m mailboxBackend) Capabilities() model.Capabilities {
	if m.inbox.Capabilities != nil {
		return *m.inbox.Capabilities
	}
	if m.routed {
		return model.StandaloneCapabilities()
	}
	return model.DomainCapabilities()
}

// normalizeMailboxStoreError wraps a raw store error in the common mailbox
// classification so the HTTP layer answers with a stable status and a safe
// message. It mirrors the app package's mapping for the errors the store emits
// directly.
func normalizeMailboxStoreError(err error) error {
	if err == nil {
		return nil
	}
	var mb *model.MailboxError
	if errors.As(err, &mb) {
		return err
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return model.NewMailboxError(model.ErrKindNotFound, "not found", false, err)
	case errors.Is(err, store.ErrForbidden):
		return model.NewMailboxError(model.ErrKindForbidden, "not permitted", false, err)
	case errors.Is(err, store.ErrConflict):
		return model.NewMailboxError(model.ErrKindConflict, "conflict", false, err)
	default:
		return err
	}
}

// remoteOnly reports whether the inbox requires a configured remote connector
// before any data operation can succeed.
func (m mailboxBackend) remoteConfigured() bool {
	if !m.routed {
		return true
	}
	return m.inbox.RemoteConfigured && m.inbox.Remote != nil
}

// readableInboxes returns the account's inboxes the principal may read (all of
// them for an account Admin), each with its capability surface attached. It is
// the fan-out set for the account-wide common listing.
func (s *Server) readableInboxes(ctx context.Context, p model.Principal) ([]mailboxBackend, error) {
	boxes, err := s.Service.Store.ListInboxes(ctx, p)
	if err != nil {
		return nil, normalizeMailboxStoreError(err)
	}
	out := make([]mailboxBackend, 0, len(boxes))
	for _, box := range boxes {
		mb := mailboxBackend{srv: s, inbox: box, p: p}
		if box.Kind == model.InboxKindStandalone {
			caps := model.StandaloneCapabilities()
			caps.Outbound = box.Remote != nil && box.Remote.SMTP != nil
			box.Capabilities = &caps
			mb.inbox = box
			mb.remote = s.remoteMailbox()
			mb.routed = true
		} else {
			caps := model.DomainCapabilities()
			box.Capabilities = &caps
			mb.inbox = box
		}
		out = append(out, mb)
	}
	return out, nil
}

// resolveMessageAny resolves a message id against the local store first and,
// failing that, the authorized standalone inboxes' cached remote index. It
// returns the owning mailboxBackend and whether the message is remote. It returns
// store.ErrNotFound when the id matches neither.
func (s *Server) resolveMessageAny(ctx context.Context, p model.Principal, id string) (model.Message, mailboxBackend, bool, error) {
	m, err := s.Service.Store.GetMessage(ctx, p, id)
	if err == nil {
		mb, merr := s.resolveMailbox(ctx, p, m.InboxID)
		if merr != nil {
			return model.Message{}, mailboxBackend{}, false, merr
		}
		return m, mb, false, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return model.Message{}, mailboxBackend{}, false, err
	}
	boxes, berr := s.readableInboxes(ctx, p)
	if berr != nil {
		return model.Message{}, mailboxBackend{}, false, berr
	}
	var firstErr error
	for _, mb := range boxes {
		if !mb.routed || !mb.remoteConfigured() {
			continue
		}
		v, gerr := mb.remote.GetRemoteMessage(ctx, p, mb.inbox.ID, id)
		if gerr == nil {
			return remoteMessageToModel(v, &model.Folder{Path: v.FolderPath}), mb, true, nil
		}
		// A remote arrival event's entity id is the arrival id, not the cached
		// metadata id, and detection is deliberately independent of the metadata
		// index. Resolve an arrival id to its metadata row (materializing it from
		// the arrival's durable header when the reconcile has not yet run), so a
		// client can follow an event straight into the canonical read API.
		if isRemoteMiss(gerr) {
			if v, aok := s.remoteMessageFromArrival(ctx, p, mb, id); aok {
				return remoteMessageToModel(v, &model.Folder{Path: v.FolderPath}), mb, true, nil
			}
			continue
		}
		if firstErr == nil {
			firstErr = gerr
		}
	}
	if firstErr != nil {
		return model.Message{}, mailboxBackend{}, false, firstErr
	}
	return model.Message{}, mailboxBackend{}, false, store.ErrNotFound
}

// remoteMessageFromArrival resolves a remote arrival id to the cached metadata
// row of the message it refers to, creating the row from the arrival's durable
// header when the reconciliation index has not yet recorded it. It reports
// ok=false when id is not an arrival of this inbox.
func (s *Server) remoteMessageFromArrival(ctx context.Context, p model.Principal, mb mailboxBackend, id string) (app.RemoteMessageView, bool) {
	rec, err := s.Service.Store.RemoteMessageForArrival(ctx, p.AccountID, id)
	if err != nil || rec.InboxID != mb.inbox.ID {
		return app.RemoteMessageView{}, false
	}
	if !mb.remote.InScope(p.AccountID, rec.InboxID, rec.FolderPath) {
		return app.RemoteMessageView{}, false
	}
	return mb.remote.ViewOf(rec), true
}

// isRemoteMiss reports whether a remote lookup error means "not in this inbox", so
// the caller can try the next source rather than surfacing the failure.
func isRemoteMiss(err error) bool {
	if errors.Is(err, store.ErrNotFound) {
		return true
	}
	var mb *model.MailboxError
	if errors.As(err, &mb) {
		return mb.Kind == model.ErrKindNotFound
	}
	return false
}

// resolveThreadAny resolves a thread id to its messages across any authorized
// inbox. A local thread is read from the store; a remote thread key is resolved
// from a standalone inbox's cached thread index. inboxQuery, when non-empty,
// scopes the lookup to one inbox (and is enforced as a boundary). It returns the
// owning inbox id and whether the thread is remote. A remote-unavailable error is
// preserved rather than masked as a 404.
func (s *Server) resolveThreadAny(ctx context.Context, p model.Principal, threadID, inboxQuery string) (threadIDOut, inboxID string, msgs []model.Message, remote bool, err error) {
	threadID = strings.TrimSpace(threadID)
	if inboxQuery != "" {
		mb, merr := s.resolveMailbox(ctx, p, inboxQuery)
		if merr != nil {
			return "", "", nil, false, merr
		}
		if mb.routed {
			_, views, rerr := mb.remote.GetRemoteThread(ctx, p, mb.inbox.ID, threadID)
			if rerr != nil {
				return "", "", nil, true, rerr
			}
			return threadID, mb.inbox.ID, remoteViewsToMessages(views), true, nil
		}
		msgs, lerr := s.Service.Store.ListMessages(ctx, p, store.MessageFilter{InboxID: inboxQuery, ThreadID: threadID, Limit: 200})
		if lerr != nil {
			return "", "", nil, false, lerr
		}
		return threadID, inboxQuery, sanitizedMessages(msgs), false, nil
	}
	msgs, lerr := s.Service.Store.ListMessages(ctx, p, store.MessageFilter{ThreadID: threadID, Limit: 200})
	if lerr != nil {
		return "", "", nil, false, lerr
	}
	if len(msgs) > 0 {
		return threadID, msgs[0].InboxID, sanitizedMessages(msgs), false, nil
	}
	boxes, berr := s.readableInboxes(ctx, p)
	if berr != nil {
		return "", "", nil, false, berr
	}
	var firstErr error
	for _, mb := range boxes {
		if !mb.routed || !mb.remoteConfigured() {
			continue
		}
		view, views, rerr := mb.remote.GetRemoteThread(ctx, p, mb.inbox.ID, threadID)
		if rerr != nil {
			if isRemoteMiss(rerr) {
				continue
			}
			if firstErr == nil {
				firstErr = rerr
			}
			continue
		}
		return view.Key, mb.inbox.ID, remoteViewsToMessages(views), true, nil
	}
	if firstErr != nil {
		return "", "", nil, false, firstErr
	}
	return threadID, "", nil, false, store.ErrNotFound
}

// remoteViewsToMessages projects remote thread messages onto the common shape.
func remoteViewsToMessages(views []app.RemoteMessageView) []model.Message {
	out := make([]model.Message, 0, len(views))
	for _, v := range views {
		out = append(out, remoteMessageToModel(v, &model.Folder{Path: v.FolderPath}))
	}
	return out
}

// demandDetection drives an on-demand remote-arrival detection pass for the given
// standalone inboxes when a client is actively polling for events. It is the
// demand side of the demand-based fan-out: an arrival is detected and durably
// recorded only while a client is actually reading, and never mutates a message's
// read state (BODY.PEEK / header reads only). It is a no-op when no detection
// surface is installed or the inbox is not standalone/unconfigured.
func (s *Server) demandDetection(ctx context.Context, boxes []mailboxBackend) {
	if s.Service.RemoteDetection == nil {
		return
	}
	for _, mb := range boxes {
		if !mb.routed || !mb.remoteConfigured() {
			continue
		}
		s.Service.RemoteDetection.DetectInbox(ctx, mb.inbox)
	}
}

// mapMailboxError maps a normalized app mailbox error onto an HTTP response so
// a remote read failure is reported with the common classification and a safe
// message, never a raw provider string.
func mapMailboxError(w http.ResponseWriter, err error) {
	var mb *model.MailboxError
	if !errors.As(err, &mb) {
		mapStoreError(w, err)
		return
	}
	status := 400
	switch mb.Kind {
	case model.ErrKindNotFound:
		status = 404
	case model.ErrKindForbidden:
		status = 403
	case model.ErrKindConflict:
		status = 409
	case model.ErrKindQuota:
		status = 507
	case model.ErrKindUnavailable:
		status = 503
	case model.ErrKindAuth:
		status = 502
	case model.ErrKindUnsupported:
		status = 409
	case model.ErrKindInternal:
		status = 500
	}
	msg := mb.Message
	if msg == "" {
		msg = mb.Kind
	}
	writeError(w, status, msg)
}
