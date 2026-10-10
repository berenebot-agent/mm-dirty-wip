package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/imap"
	"github.com/dellarb/mailmoose/internal/transport/smtp"
)

// This file implements the real remote (standalone IMAP/SMTP) backend behind the
// common mailbox boundary. It is the seam the router dispatches to for a
// BackendRemote inbox: it resolves the inbox's encrypted credentials, opens a
// bounded IMAP session, and answers message/folder/thread/search/read/raw/
// attachment/move/label/state operations against the live server, caching only
// header/thread/folder metadata in the store (never a body).
//
// The IMAP protocol lives entirely in internal/transport/imap. This file never
// speaks IMAP itself; it maps model/app values onto the adapter's public API.

// RemoteCredentialAAD is the additional-authenticated-data binding for a
// standalone inbox's encrypted remote credentials. It binds the ciphertext to its
// owning account and inbox, so a blob copied to another row fails to decrypt.
func RemoteCredentialAAD(accountID, inboxID string) string {
	return "remote_cred:" + accountID + ":" + inboxID
}

// RemoteSecrets is the decrypted secret material of a standalone inbox.
type RemoteSecrets struct {
	IMAPPassword string
	SMTPPassword string
}

// RemoteResolved is a fully resolved remote connector: the non-secret description
// (from the inbox row) plus the decrypted secrets. It is the input to a session.
type RemoteResolved struct {
	Inbox   model.Inbox
	Secrets RemoteSecrets
}

// remoteDialer opens an authenticated IMAP session for a resolved connector. It
// is a field on RemoteMailboxService so a deterministic in-memory server can be
// substituted in tests; production uses imap.Dial.
type RemoteDialer func(ctx context.Context, cfg imap.Config) (RemoteSession, error)

// remoteSession is the subset of the IMAP adapter the remote backend uses. It is
// an interface so a test can install a fake without a live server, while the
// production implementation is the real *imap.Adapter.
type RemoteSession interface {
	DiscoverFolders(ctx context.Context, explicitRoot string) ([]imap.RemoteFolder, imap.RootScope, error)
	Search(ctx context.Context, folder string, q imap.SearchQuery) (imap.SearchResult, error)
	ListHeaders(ctx context.Context, folder string, uids []uint32, max int) ([]imap.MessageHeader, uint32, error)
	FetchHeader(ctx context.Context, loc imap.Locator) (imap.MessageHeader, error)
	FetchRawMIME(ctx context.Context, loc imap.Locator, w io.Writer) error
	FetchBodyPart(ctx context.Context, loc imap.Locator, part []int, w io.Writer) error
	SetFlags(ctx context.Context, loc imap.Locator, add, remove []string) ([]string, error)
	MoveMessage(ctx context.Context, loc imap.Locator, destFolder string) (imap.MoveResult, error)
	// DeleteMessage marks a message \Deleted and expunges it by UID. It is
	// UID-targeted (never a blanket EXPUNGE) and requires UIDPLUS; when the
	// server lacks it the adapter returns an unsupported error rather than
	// guessing.
	DeleteMessage(ctx context.Context, loc imap.Locator) error
	AppendReader(ctx context.Context, folder string, r io.Reader, size int64, flags []string, date time.Time) (imap.AppendResult, error)
	CreateFolder(ctx context.Context, path string) error
	RenameFolder(ctx context.Context, oldPath, newPath string) error
	DeleteFolder(ctx context.Context, path string) error
	EnsureFolderExists(ctx context.Context, path string) (uint32, error)
	FindByMessageID(ctx context.Context, folder, messageID string) (imap.Locator, error)
	FindByHeader(ctx context.Context, folder, key, value string) ([]imap.Locator, error)
	Close() error
}

// RemoteMailboxService is the common application surface for a standalone inbox
// backed by a live remote server. It owns credential resolution, session opening,
// metadata reconciliation and the live operations. The local/remote routing
// decision is the MailboxRouter's; this service performs the remote side.
type RemoteMailboxService struct {
	Service *Service
	// dial opens a session. When nil, imap.Dial is used. It is injectable so a
	// test can point the backend at an in-memory IMAP4rev2 server.
	dial RemoteDialer
}

// NewRemoteMailboxService builds the remote surface over the app service.
func NewRemoteMailboxService(s *Service) *RemoteMailboxService {
	return &RemoteMailboxService{Service: s, dial: defaultRemoteDialer}
}

// defaultRemoteDialer dials a live IMAP session.
func defaultRemoteDialer(ctx context.Context, cfg imap.Config) (RemoteSession, error) {
	a, err := imap.Dial(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// SetRemoteDialer installs a session dialer (tests). It never replaces a live
// dialer in production; cmd/server never calls it.
func (m *RemoteMailboxService) SetRemoteDialer(d RemoteDialer) {
	if d != nil {
		m.dial = d
	}
}

// ResolveRemote loads a standalone inbox and decrypts its remote secrets. It
// returns ErrRemoteNotBound when no credentials are configured so callers can
// report a connector that has not been set up rather than a connection failure.
func (m *RemoteMailboxService) ResolveRemote(ctx context.Context, accountID, inboxID string) (RemoteResolved, error) {
	inbox, err := m.Service.Store.GetInboxInternal(ctx, accountID, inboxID)
	if err != nil {
		return RemoteResolved{}, mapStoreError(err)
	}
	if inbox.Kind != model.InboxKindStandalone {
		return RemoteResolved{}, model.NewMailboxError(model.ErrKindUnsupported, "not a standalone inbox", false, ErrRemoteNotBound)
	}
	if inbox.Remote == nil || !inbox.RemoteConfigured {
		return RemoteResolved{}, model.NewMailboxError(model.ErrKindUnavailable, "remote connector is not configured", false, ErrRemoteNotBound)
	}
	creds, err := m.Service.Store.GetRemoteCredentials(ctx, accountID, inboxID)
	if err != nil {
		return RemoteResolved{}, mapStoreError(err)
	}
	secrets := RemoteSecrets{}
	if strings.TrimSpace(creds.EncryptedIMAP) != "" {
		plain, derr := m.Service.DecryptSecretAAD(RemoteCredentialAAD(accountID, inboxID), creds.EncryptedIMAP)
		if derr != nil {
			return RemoteResolved{}, model.NewMailboxError(model.ErrKindInternal, "remote credentials could not be decrypted", false, derr)
		}
		secrets.IMAPPassword = string(plain)
	}
	if strings.TrimSpace(creds.EncryptedSMTP) != "" {
		plain, derr := m.Service.DecryptSecretAAD(RemoteCredentialAAD(accountID, inboxID), creds.EncryptedSMTP)
		if derr != nil {
			return RemoteResolved{}, model.NewMailboxError(model.ErrKindInternal, "remote credentials could not be decrypted", false, derr)
		}
		secrets.SMTPPassword = string(plain)
	}
	return RemoteResolved{Inbox: inbox, Secrets: secrets}, nil
}

// imapConfig builds the adapter Config for a resolved connector, applying the
// deployment's destination policy and bounded dial timeouts.
func (m *RemoteMailboxService) imapConfig(r RemoteResolved) imap.Config {
	rc := r.Inbox.Remote
	security := imap.SecurityFromModel(rc.Security)
	return imap.Config{
		Host:          rc.Host,
		Port:          rc.Port,
		Username:      rc.Username,
		Password:      r.Secrets.IMAPPassword,
		Security:      security,
		AllowPlain:    security == imap.SecurityPlain,
		RequirePublic: m.Service.Config.RequirePublicOutbound(),
	}
}

// open resolves and dials a live session. The caller must Close it. The returned
// inbox is the resolved standalone inbox.
func (m *RemoteMailboxService) open(ctx context.Context, accountID, inboxID string) (RemoteSession, RemoteResolved, error) {
	r, err := m.ResolveRemote(ctx, accountID, inboxID)
	if err != nil {
		return nil, RemoteResolved{}, err
	}
	if r.Secrets.IMAPPassword == "" {
		return nil, RemoteResolved{}, model.NewMailboxError(model.ErrKindUnavailable, "remote connector has no password configured", false, ErrRemoteNotBound)
	}
	sess, err := m.dial(ctx, m.imapConfig(r))
	if err != nil {
		return nil, RemoteResolved{}, normalizeRemoteError(err)
	}
	return sess, r, nil
}

// ConfigureStandaloneRemote validates and persists a standalone inbox's remote
// binding: the non-secret description on the inbox row, and the encrypted secrets
// bound to the account/inbox AAD. A blank secret field retains its stored value so
// an operator can change only the host or username; a non-secret field is a whole
// value (never merged). It requires Owner on the inbox (or an account admin).
func (m *RemoteMailboxService) ConfigureStandaloneRemote(ctx context.Context, p model.Principal, inboxID string, in store.StandaloneRemoteUpdate) (model.Inbox, error) {
	if !p.CanOwn(inboxID) && !p.Admin {
		return model.Inbox{}, model.NewMailboxError(model.ErrKindForbidden, "not permitted", false, store.ErrForbidden)
	}
	inbox, err := m.Service.Store.GetInboxInternal(ctx, p.AccountID, inboxID)
	if err != nil {
		return model.Inbox{}, mapStoreError(err)
	}
	if inbox.Kind != model.InboxKindStandalone {
		return model.Inbox{}, model.NewMailboxError(model.ErrKindUnsupported, "not a standalone inbox", false, ErrRemoteNotBound)
	}
	// Only touch the non-secret remote description when the caller actually
	// supplies a non-secret field. A secrets-only configure call must not require
	// (or blank) the host.
	if hasRemoteNonSecretFields(in) {
		if _, err := m.Service.Store.UpdateStandaloneRemote(ctx, p.AccountID, inboxID, in); err != nil {
			return model.Inbox{}, mapStoreError(err)
		}
	}
	// Encrypt the supplied secrets (blank retains the stored ciphertext).
	existing, cerr := m.Service.Store.GetRemoteCredentials(ctx, p.AccountID, inboxID)
	if cerr != nil {
		return model.Inbox{}, mapStoreError(cerr)
	}
	encIMAP := existing.EncryptedIMAP
	if strings.TrimSpace(in.IMAPPassword) != "" {
		encIMAP, err = m.Service.EncryptSecretAAD(RemoteCredentialAAD(p.AccountID, inboxID), []byte(in.IMAPPassword))
		if err != nil {
			return model.Inbox{}, model.NewMailboxError(model.ErrKindInternal, "", false, err)
		}
	}
	encSMTP := existing.EncryptedSMTP
	if strings.TrimSpace(in.SMTPPassword) != "" {
		encSMTP, err = m.Service.EncryptSecretAAD(RemoteCredentialAAD(p.AccountID, inboxID), []byte(in.SMTPPassword))
		if err != nil {
			return model.Inbox{}, model.NewMailboxError(model.ErrKindInternal, "", false, err)
		}
	}
	if err := m.Service.Store.SaveRemoteCredentials(ctx, p.AccountID, inboxID, encIMAP, encSMTP, store.ConfigVersion{Revision: existing.Revision}); err != nil {
		return model.Inbox{}, mapStoreError(err)
	}
	return m.Service.Store.GetInboxInternal(ctx, p.AccountID, inboxID)
}

// hasRemoteNonSecretFields reports whether a remote update supplies any non-secret
// field (host/port/username/security/smtp/namespace). It gates the store update so
// a secrets-only configure call never blanks the existing binding.
func hasRemoteNonSecretFields(in store.StandaloneRemoteUpdate) bool {
	return strings.TrimSpace(in.Host) != "" || in.Port != 0 || strings.TrimSpace(in.Username) != "" ||
		strings.TrimSpace(in.Security) != "" || in.SMTPHost != "" || in.SMTPPort != 0 ||
		strings.TrimSpace(in.SMTPUsername) != "" || strings.TrimSpace(in.SMTPSecurity) != "" ||
		in.ClearSMTP || strings.TrimSpace(in.Namespace) != ""
}

// TestStandaloneRemote verifies that the supplied (or stored) remote binding can
// authenticate and that the selected root resolves a folder scope, without
// mutating any mailbox state. A blank secret field uses the stored value, so an
// operator can test a configuration before saving it. It is the "test service
// method" the configure flow calls.
func (m *RemoteMailboxService) TestStandaloneRemote(ctx context.Context, p model.Principal, inboxID string, in store.StandaloneRemoteUpdate) (imap.RootScope, error) {
	if !p.CanRead(inboxID) && !p.Admin {
		return imap.RootScope{}, model.NewMailboxError(model.ErrKindForbidden, "not permitted", false, store.ErrForbidden)
	}
	inbox, err := m.Service.Store.GetInboxInternal(ctx, p.AccountID, inboxID)
	if err != nil {
		return imap.RootScope{}, mapStoreError(err)
	}
	if inbox.Kind != model.InboxKindStandalone {
		return imap.RootScope{}, model.NewMailboxError(model.ErrKindUnsupported, "not a standalone inbox", false, ErrRemoteNotBound)
	}
	resolved := RemoteResolved{Inbox: inbox}
	if in.Host != "" {
		rc := &model.RemoteConnection{
			Host:     in.Host,
			Port:     in.Port,
			Username: in.Username,
			Security: in.Security,
		}
		if in.SMTPHost != "" || in.SMTPUsername != "" || in.SMTPPort != 0 {
			rc.SMTP = &model.RemoteSMTP{Host: in.SMTPHost, Port: in.SMTPPort, Username: in.SMTPUsername, Security: in.SMTPSecurity}
		}
		resolved.Inbox.Remote = rc
	}
	if in.Username != "" && resolved.Inbox.Remote != nil {
		resolved.Inbox.Remote.Username = in.Username
	}
	existing, cerr := m.Service.Store.GetRemoteCredentials(ctx, p.AccountID, inboxID)
	if cerr != nil {
		return imap.RootScope{}, mapStoreError(cerr)
	}
	if strings.TrimSpace(in.IMAPPassword) != "" {
		resolved.Secrets.IMAPPassword = in.IMAPPassword
	} else if existing.EncryptedIMAP != "" {
		plain, derr := m.Service.DecryptSecretAAD(RemoteCredentialAAD(p.AccountID, inboxID), existing.EncryptedIMAP)
		if derr != nil {
			return imap.RootScope{}, model.NewMailboxError(model.ErrKindInternal, "stored credentials could not be decrypted", false, derr)
		}
		resolved.Secrets.IMAPPassword = string(plain)
	}
	if resolved.Inbox.Remote == nil || resolved.Inbox.Remote.Host == "" {
		return imap.RootScope{}, model.NewMailboxError(model.ErrKindInvalid, "remote host is required", false, nil)
	}
	if resolved.Secrets.IMAPPassword == "" {
		return imap.RootScope{}, model.NewMailboxError(model.ErrKindInvalid, "remote password is required", false, nil)
	}
	dialCtx, cancel := context.WithTimeout(ctx, remoteDialTimeout)
	defer cancel()
	sess, err := m.dial(dialCtx, m.imapConfig(resolved))
	if err != nil {
		return imap.RootScope{}, normalizeRemoteError(err)
	}
	defer sess.Close()
	_, scope, err := sess.DiscoverFolders(dialCtx, inbox.Namespace)
	if err != nil {
		return imap.RootScope{}, normalizeRemoteError(err)
	}
	// Validate the optional SMTP binding too, without sending any test mail: a
	// connection + AUTH check only. A failure is reported so the operator learns
	// the outbound side is unusable before saving.
	if err := m.verifyStandaloneSMTP(ctx, inbox, in, existing); err != nil {
		return imap.RootScope{}, err
	}
	return scope, nil
}

// verifyStandaloneSMTP validates an optional SMTP binding without sending test
// mail (connect + TLS/STARTTLS + AUTH only). A nil/absent SMTP binding is valid
// (outbound simply is not configured). A blank password uses the supplied or
// stored SMTP secret, so a configuration can be tested before saving.
func (m *RemoteMailboxService) verifyStandaloneSMTP(ctx context.Context, inbox model.Inbox, in store.StandaloneRemoteUpdate, existing store.RemoteCredentials) error {
	smtpConn := inbox.Remote
	if smtpConn == nil || smtpConn.SMTP == nil {
		// The caller may be testing an SMTP block not yet saved; use the input.
		if in.SMTPHost == "" {
			return nil
		}
	}
	host := strings.TrimSpace(in.SMTPHost)
	port := in.SMTPPort
	username := strings.TrimSpace(in.SMTPUsername)
	security := strings.TrimSpace(in.SMTPSecurity)
	if host == "" && smtpConn != nil && smtpConn.SMTP != nil {
		host, port, username, security = smtpConn.SMTP.Host, smtpConn.SMTP.Port, smtpConn.SMTP.Username, smtpConn.SMTP.Security
	}
	if host == "" {
		return nil
	}
	password := strings.TrimSpace(in.SMTPPassword)
	if password == "" {
		// Prefer a stored SMTP secret; fall back to the IMAP secret, which many
		// providers share for both protocols.
		if strings.TrimSpace(existing.EncryptedSMTP) != "" {
			if plain, derr := m.Service.DecryptSecretAAD(RemoteCredentialAAD(inbox.AccountID, inbox.ID), existing.EncryptedSMTP); derr == nil {
				password = string(plain)
			}
		} else if strings.TrimSpace(existing.EncryptedIMAP) != "" {
			if plain, derr := m.Service.DecryptSecretAAD(RemoteCredentialAAD(inbox.AccountID, inbox.ID), existing.EncryptedIMAP); derr == nil {
				password = string(plain)
			}
		}
	}
	sec := strings.TrimSpace(security)
	cfg := smtp.Config{Host: host, Port: port, Username: username, Password: password, Security: sec}
	testCtx, cancel := context.WithTimeout(ctx, remoteDialTimeout)
	defer cancel()
	if err := smtp.Verify(testCtx, cfg, m.Service.Config.RequirePublicOutbound()); err != nil {
		return model.NewMailboxError(model.ErrKindUnavailable, "the outbound SMTP server could not be validated", true, err)
	}
	return nil
}

// normalizeRemoteError converts an error from the adapter (already a
// *model.MailboxError in most cases) into the common classification. An unknown
// error becomes an internal mailbox error.
func normalizeRemoteError(err error) error {
	if err == nil {
		return nil
	}
	var mb *model.MailboxError
	if errors.As(err, &mb) {
		return err
	}
	if errors.Is(err, imap.ErrNotFound) {
		return model.NewMailboxError(model.ErrKindNotFound, "remote message not found", false, err)
	}
	if errors.Is(err, imap.ErrNotConnected) {
		return model.NewMailboxError(model.ErrKindUnavailable, "remote connection is closed", true, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return model.NewMailboxError(model.ErrKindUnavailable, "remote server timed out", true, err)
	}
	return model.NewMailboxError(model.ErrKindInternal, "", false, err)
}

// remoteDialTimeout bounds a single session dial from the app layer, in addition
// to the adapter's own dial timeout.
const remoteDialTimeout = 25e9 // 25s

// remoteBodyLimit resolves the maximum number of bytes of a remote body the
// service will buffer to a temporary file. It reuses the operator's configured
// message-size cap so a remote body cannot exceed the local maximum.
func (m *RemoteMailboxService) remoteBodyLimit() int64 {
	limit := m.Service.Config.MaxMessageBytes
	if limit <= 0 {
		limit = 30 << 20
	}
	return limit
}

// RemoteStandaloneSender resolves a standalone inbox's own sending configuration
// for the store's sending seam. The store calls ResolveInboxSendingConfig when a
// message's target is a standalone inbox (no managed domain). It builds an
// in-memory store.DomainSendingConfig whose encrypted payload is bound to the
// account/inbox AAD, so the existing DecryptDomainSendingConfig path decrypts it
// unchanged. It never resolves a standalone inbox against a non-existent domain.
//
// The SMTP provider is "smtp": the built-in generic SMTP outbound adapter. When the
// inbox has no SMTP binding, it is store.ErrNoProvider so the message is queued and
// held rather than sent through a connector that does not exist.
type RemoteStandaloneSender struct {
	Service *Service
}

var _ store.StandaloneSenderResolver = (*RemoteStandaloneSender)(nil)

// ResolveInboxSendingConfig builds the standalone inbox's SMTP sending
// configuration. A missing SMTP binding is ErrNoProvider.
func (r *RemoteStandaloneSender) ResolveInboxSendingConfig(ctx context.Context, accountID, inboxID string) (store.DomainSendingConfig, error) {
	inbox, err := r.Service.Store.GetInboxInternal(ctx, accountID, inboxID)
	if err != nil {
		return store.DomainSendingConfig{}, mapStoreError(err)
	}
	if inbox.Kind != model.InboxKindStandalone {
		return store.DomainSendingConfig{}, store.ErrNoProvider
	}
	if inbox.Remote == nil || inbox.Remote.SMTP == nil || strings.TrimSpace(inbox.Remote.SMTP.Host) == "" {
		return store.DomainSendingConfig{}, store.ErrNoProvider
	}
	creds, cerr := r.Service.Store.GetRemoteCredentials(ctx, accountID, inboxID)
	if cerr != nil {
		return store.DomainSendingConfig{}, mapStoreError(cerr)
	}
	password := ""
	if strings.TrimSpace(creds.EncryptedSMTP) != "" {
		plain, derr := r.Service.DecryptSecretAAD(RemoteCredentialAAD(accountID, inboxID), creds.EncryptedSMTP)
		if derr != nil {
			return store.DomainSendingConfig{}, model.NewMailboxError(model.ErrKindInternal, "remote SMTP credentials could not be decrypted", false, derr)
		}
		password = string(plain)
	}
	smtp := inbox.Remote.SMTP
	cfg := map[string]any{
		"host":     smtp.Host,
		"port":     smtp.Port,
		"username": smtp.Username,
		"password": password,
		"security": smtp.Security,
	}
	enc, eerr := r.Service.encryptConfig(configAAD(accountID, inboxID), cfg)
	if eerr != nil {
		return store.DomainSendingConfig{}, model.NewMailboxError(model.ErrKindInternal, "", false, eerr)
	}
	return store.DomainSendingConfig{
		ID:              "remote:" + inboxID,
		AccountID:       accountID,
		DomainID:        inboxID,
		Provider:        "smtp",
		EncryptedConfig: enc,
	}, nil
}

// InstallRemoteBridges wires the remote integration onto the app Service and the
// store: the RemoteDraft handoff publisher, the standalone sending resolver and
// the remote session dialer. It is called once at startup (cmd/server) and by
// tests. It never installs a global; each bridge lives on the Service or Store it
// was handed.
func (m *RemoteMailboxService) InstallRemoteBridges() {
	m.Service.SetHandoffPublisher(&RemoteHandoffPublisher{Service: m})
	m.Service.Store.SetStandaloneSenderResolver(&RemoteStandaloneSender{Service: m.Service})
	// The remote forwarder lets the demand-based webhook/Hermes workers fetch a
	// detected remote arrival's raw MIME without speaking IMAP.
	m.Service.RemoteForwarder = m
	// The remote message resolver lets a reply/forward target a remote message
	// whose body lives only on the server.
	m.Service.RemoteMessages = m
}

// CopySentCopies drives the durable sent-copy queue once. cmd/server's worker
// calls it; tests call it directly.
func (m *RemoteMailboxService) CopySentCopies(ctx context.Context) { m.CopyPendingSentCopies(ctx) }

// Compile-time assertion that the production IMAP adapter satisfies the remote
// session surface the app layer depends on.
var _ RemoteSession = (*imap.Adapter)(nil)
