package app

import (
	"context"
	"errors"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
)

// Mailbox router errors. They are returned as *model.MailboxError so callers get
// a stable, transport-independent classification (see model.ErrKind*).
var (
	// ErrRemoteNotBound is returned when a remote operation is requested for a
	// standalone inbox that has no configured remote connector yet.
	ErrRemoteNotBound = errors.New("standalone inbox has no remote connector configured")
	// ErrLocalOnly is returned when a local store operation is requested for an
	// inbox that is not backed by the local store (a remote-only inbox).
	ErrLocalOnly = errors.New("operation is local-only")
)

// MailboxBackendKind names where a mailbox's data lives. It is the routing
// decision the common service boundary makes for every mailbox operation.
type MailboxBackendKind string

const (
	// BackendLocal is the local SQLite store: messages are persisted and their
	// bodies archived under /data. Domain inboxes are always local.
	BackendLocal MailboxBackendKind = "local"
	// BackendRemote is a remote IMAP account: headers/thread metadata are cached
	// locally but bodies and attachments are fetched live and never archived.
	BackendRemote MailboxBackendKind = "remote"
)

// MailboxRoute describes how an inbox's operations are dispatched. It is the
// stable contract between the workflow/API layer and the local/remote backends.
type MailboxRoute struct {
	Inbox        model.Inbox
	Backend      MailboxBackendKind
	Capabilities model.Capabilities
	// Remote is the non-secret remote description, set for BackendRemote.
	Remote *model.RemoteConnection
}

// MailboxRouter resolves an inbox to its backend and capability surface. It is
// intentionally thin: it performs no I/O beyond a store read and holds no remote
// session. The remote adapter (a later wave) is registered onto it, so workflow
// code depends only on this boundary and never on an IMAP type.
type MailboxRouter struct {
	store *store.Store
}

// NewMailboxRouter builds a router over a store.
func NewMailboxRouter(st *store.Store) *MailboxRouter { return &MailboxRouter{store: st} }

// Route resolves a principal-authorized inbox to its backend. Local inboxes
// (domain) are always BackendLocal; a standalone inbox is BackendRemote.
func (r *MailboxRouter) Route(ctx context.Context, p model.Principal, inboxID string) (MailboxRoute, error) {
	inbox, err := r.store.GetInbox(ctx, p, inboxID)
	if err != nil {
		return MailboxRoute{}, mapStoreError(err)
	}
	return r.routeInbox(inbox), nil
}

// RouteInternal resolves an inbox without a principal check, for workflow code
// that already holds an authorized account/inbox pair.
func (r *MailboxRouter) RouteInternal(ctx context.Context, accountID, inboxID string) (MailboxRoute, error) {
	inbox, err := r.store.GetInboxInternal(ctx, accountID, inboxID)
	if err != nil {
		return MailboxRoute{}, mapStoreError(err)
	}
	return r.routeInbox(inbox), nil
}

func (r *MailboxRouter) routeInbox(inbox model.Inbox) MailboxRoute {
	if inbox.Kind == model.InboxKindStandalone {
		caps := model.StandaloneCapabilities()
		caps.Outbound = inbox.Remote != nil && inbox.Remote.SMTP != nil
		return MailboxRoute{Inbox: inbox, Backend: BackendRemote, Capabilities: caps, Remote: inbox.Remote}
	}
	return MailboxRoute{Inbox: inbox, Backend: BackendLocal, Capabilities: model.DomainCapabilities()}
}

// RemoteLocatorFor returns the remote locator of a cached remote message, or an
// empty locator and false for a local message.
func (r *MailboxRouter) RemoteLocatorFor(ctx context.Context, accountID, inboxID, messageID string) (model.RemoteLocator, bool, error) {
	m, err := r.store.GetRemoteMessage(ctx, accountID, inboxID, messageID)
	if errors.Is(err, store.ErrNotFound) {
		return model.RemoteLocator{}, false, nil
	}
	if err != nil {
		return model.RemoteLocator{}, false, err
	}
	return model.RemoteLocator{FolderPath: m.FolderPath, UIDValidity: m.UIDValidity, UID: m.UID, MessageID: m.RFCMessageID}, true, nil
}

// CapabilitiesOf reports an inbox's declared capability surface.
func (r *MailboxRouter) CapabilitiesOf(inbox model.Inbox) model.Capabilities {
	return r.routeInbox(inbox).Capabilities
}

// mapStoreError converts a store error to a normalized *model.MailboxError.
func mapStoreError(err error) error {
	if err == nil {
		return nil
	}
	var mb *model.MailboxError
	if errors.As(err, &mb) {
		return err
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return model.NewMailboxError(model.ErrKindNotFound, "mailbox not found", false, err)
	case errors.Is(err, store.ErrForbidden):
		return model.NewMailboxError(model.ErrKindForbidden, "not permitted", false, err)
	case errors.Is(err, store.ErrConflict):
		return model.NewMailboxError(model.ErrKindConflict, "conflicting change", false, err)
	case errors.Is(err, store.ErrQuota):
		return model.NewMailboxError(model.ErrKindQuota, "storage quota exceeded", false, err)
	case errors.Is(err, store.ErrStandaloneRequired):
		return model.NewMailboxError(model.ErrKindUnsupported, "operation requires a standalone inbox", false, err)
	default:
		return model.NewMailboxError(model.ErrKindInternal, "", false, err)
	}
}

// StandaloneMailboxService is the common application surface for a standalone
// inbox: create/list/get plus folder and remote-metadata reconciliation. The
// local/remote dispatch itself is the MailboxRouter's job; this type wires the
// authorization and validation the HTTP and workflow layers share.
type StandaloneMailboxService struct {
	Service *Service
	Router  *MailboxRouter
}

// NewStandaloneMailboxService builds the standalone surface over the app service.
func NewStandaloneMailboxService(s *Service) *StandaloneMailboxService {
	return &StandaloneMailboxService{Service: s, Router: NewMailboxRouter(s.Store)}
}

// CreateStandalone creates a standalone inbox for the principal's account.
// Only an account Admin may create one: creating a mailbox (and its remote
// connector) is an account-level action, matching the domain inbox creation
// rule. A mailbox Owner who is not an account Admin cannot create inboxes.
func (m *StandaloneMailboxService) CreateStandalone(ctx context.Context, p model.Principal, in store.StandaloneCreate) (model.Inbox, error) {
	if !p.Admin {
		return model.Inbox{}, model.NewMailboxError(model.ErrKindForbidden, "not permitted", false, store.ErrForbidden)
	}
	inbox, err := m.Service.Store.CreateStandaloneInbox(ctx, p.AccountID, in)
	if err != nil {
		return model.Inbox{}, mapStoreError(err)
	}
	return inbox, nil
}

// ListStandalone lists the principal's account standalone inboxes, filtered to
// the mailboxes they may read.
func (m *StandaloneMailboxService) ListStandalone(ctx context.Context, p model.Principal) ([]model.Inbox, error) {
	boxes, err := m.Service.Store.ListStandaloneInboxes(ctx, p.AccountID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	if p.Admin {
		return boxes, nil
	}
	out := boxes[:0]
	for _, b := range boxes {
		if p.CanRead(b.ID) {
			out = append(out, b)
		}
	}
	return out, nil
}

// GetStandalone fetches one standalone inbox the principal may read, with its
// capability surface attached.
func (m *StandaloneMailboxService) GetStandalone(ctx context.Context, p model.Principal, inboxID string) (model.Inbox, error) {
	route, err := m.Router.Route(ctx, p, inboxID)
	if err != nil {
		return model.Inbox{}, err
	}
	if route.Backend != BackendRemote {
		return model.Inbox{}, model.NewMailboxError(model.ErrKindUnsupported, "not a standalone inbox", false, ErrRemoteNotBound)
	}
	caps := route.Capabilities
	route.Inbox.Capabilities = &caps
	return route.Inbox, nil
}

// ReconcileFolders replaces a standalone inbox's folder set from a sync pass.
func (m *StandaloneMailboxService) ReconcileFolders(ctx context.Context, accountID, inboxID string, folders []store.FolderInput) ([]model.Folder, error) {
	foldersOut, err := m.Service.Store.UpsertFolders(ctx, accountID, inboxID, folders)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return foldersOut, nil
}

// ListFolders lists a standalone inbox's folders.
func (m *StandaloneMailboxService) ListFolders(ctx context.Context, accountID, inboxID string) ([]model.Folder, error) {
	folders, err := m.Service.Store.ListFolders(ctx, accountID, inboxID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return folders, nil
}

// MailboxBackend is the practical interface downstream code depends on for a
// mailbox's read surface, independent of whether the data is local or remote.
// It is the seam the remote adapter wave implements; the local backend already
// implements it over the store. Operations not yet implemented by a backend
// return an *model.MailboxError with ErrKindUnsupported so callers can degrade
// cleanly instead of type-asserting a provider.
//
// Scaffold vs upcoming: only the methods documented as "scaffolded" are wired
// today. The rest are declared so the API of both backends is fixed before the
// remote adapter exists; implementing them is the remote wave's job (and the
// local wave's job for any that the domain backend should answer locally).
type MailboxBackend interface {
	// Kind reports which backend this is.
	Kind() MailboxBackendKind
	// Capabilities reports the declared surface of the backing mailbox.
	Capabilities() model.Capabilities
	// ListFolders returns the mailbox's folders. Scaffolded for the local
	// backend (managed aliases); the remote backend implements it over IMAP
	// LIST during the adapter wave.
	ListFolders(ctx context.Context, accountID, inboxID string) (model.ListEnvelope[model.Folder], error)
}

// LocalBackend serves a domain inbox from the local SQLite store. It is the
// concrete backend for BackendLocal.
//
// Scaffolded today: Kind and Capabilities. ListFolders returns the inbox's
// managed aliases as folder-role entries (the domain model has no custom folder
// tree yet, so this is a best-effort mapping, not the remote folder surface).
type LocalBackend struct {
	store *store.Store
}

// NewLocalBackend builds the local backend over a store.
func NewLocalBackend(st *store.Store) *LocalBackend { return &LocalBackend{store: st} }

func (b *LocalBackend) Kind() MailboxBackendKind { return BackendLocal }

func (b *LocalBackend) Capabilities() model.Capabilities { return model.DomainCapabilities() }

// ListFolders maps a domain inbox's managed aliases to folder-role entries. The
// domain mailbox has no independent folder tree; this keeps the interface
// uniform without pretending the alias set is a remote folder hierarchy.
func (b *LocalBackend) ListFolders(ctx context.Context, accountID, inboxID string) (model.ListEnvelope[model.Folder], error) {
	inbox, err := b.store.GetInboxInternal(ctx, accountID, inboxID)
	if err != nil {
		return model.ListEnvelope[model.Folder]{}, mapStoreError(err)
	}
	items := []model.Folder{{
		InboxID:    inbox.ID,
		Path:       model.FolderRoleInbox,
		Name:       model.FolderRoleInbox,
		Role:       model.FolderRoleInbox,
		Selectable: true,
	}}
	return model.ListEnvelope[model.Folder]{
		Items:        items,
		Completeness: model.CompletenessComplete,
	}, nil
}

// RemoteBackend serves a standalone inbox from a remote IMAP/SMTP account. The
// adapter wave provides the session; until then a RemoteBackend can be built but
// ListFolders reads the locally cached folder set (populated by an earlier sync)
// and reports that the result is only as complete as the last sync.
//
// Scaffolded today: Kind, Capabilities and ListFolders over the cached
// inbox_folders table. Upcoming (adapter wave): Sync, OpenMessage (live body and
// attachment fetch by RemoteLocator), MoveMessage, AppendRemoteDraft, and the
// SMTP send path.
type RemoteBackend struct {
	store  *store.Store
	inbox  model.Inbox
	remote *model.RemoteConnection
}

// NewRemoteBackend builds a remote backend for a resolved standalone inbox.
func NewRemoteBackend(st *store.Store, inbox model.Inbox) *RemoteBackend {
	caps := model.StandaloneCapabilities()
	caps.Outbound = inbox.Remote != nil && inbox.Remote.SMTP != nil
	return &RemoteBackend{store: st, inbox: inbox, remote: inbox.Remote}
}

func (b *RemoteBackend) Kind() MailboxBackendKind { return BackendRemote }

func (b *RemoteBackend) Capabilities() model.Capabilities {
	caps := model.StandaloneCapabilities()
	caps.Outbound = b.remote != nil && b.remote.SMTP != nil
	return caps
}

// ListFolders returns the cached folder set of the standalone inbox. It reports
// CompletenessUnknown because the cache reflects the last sync, not a live
// enumeration; the adapter wave replaces this with a live LIST.
func (b *RemoteBackend) ListFolders(ctx context.Context, accountID, inboxID string) (model.ListEnvelope[model.Folder], error) {
	folders, err := b.store.ListFolders(ctx, accountID, inboxID)
	if err != nil {
		return model.ListEnvelope[model.Folder]{}, mapStoreError(err)
	}
	return model.ListEnvelope[model.Folder]{
		Items:        folders,
		Completeness: model.CompletenessUnknown,
	}, nil
}

// BackendFor resolves the concrete backend for a routed mailbox. The remote
// adapter wave swaps the RemoteBackend for a live-session implementation behind
// this same factory.
func (r *MailboxRouter) BackendFor(route MailboxRoute) MailboxBackend {
	if route.Backend == BackendRemote {
		return NewRemoteBackend(r.store, route.Inbox)
	}
	return NewLocalBackend(r.store)
}
