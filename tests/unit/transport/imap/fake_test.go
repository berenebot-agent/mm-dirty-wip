package imap_test

import (
	"crypto/tls"
	"net"
	"sync"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// fakeServer is a deterministic in-memory IMAP4rev2 server built on the
// library's own imapmemserver, wrapped so a test can inject specific protocol
// behaviours (custom namespace prefix, fixed UIDVALIDITY, missing APPENDUID,
// non-empty folder delete refusal).
type fakeServer struct {
	t      *testing.T
	mem    *imapmemserver.Server
	user   *imapmemserver.User
	server *imapserver.Server
	ln     net.Listener
	addr   string

	mu        sync.Mutex
	overrides sessionOverrides
	closeOnce sync.Once
}

type sessionOverrides struct {
	namespacePrefix  *string
	namespaceDelim   *rune
	fixedUIDValidity uint32
	appendNoUID      bool
	deleteNonEmpty   bool
	selectStarted    chan struct{}
	selectRelease    chan struct{}
	selectCount      int
}

type serverConfig struct {
	caps     imap.CapSet
	username string
	password string
	tls      *tls.Config
	insecure bool
}

// defaultCaps advertises IMAP4rev2 (which implies NAMESPACE, UIDPLUS, MOVE,
// IDLE, LIST-STATUS) plus SPECIAL-USE, which the server does not fold into
// IMAP4rev2.
func defaultCaps() imap.CapSet {
	return imap.CapSet{
		imap.CapIMAP4rev1:  {},
		imap.CapIMAP4rev2:  {},
		imap.CapSpecialUse: {},
	}
}

func newFakeServer(t *testing.T, cfg serverConfig) *fakeServer {
	t.Helper()
	if cfg.username == "" {
		cfg.username = "user@example.com"
	}
	if cfg.password == "" {
		cfg.password = "secret"
	}
	if cfg.caps == nil {
		cfg.caps = defaultCaps()
	}

	fs := &fakeServer{t: t}
	fs.mem = imapmemserver.New()
	fs.user = imapmemserver.NewUser(cfg.username, cfg.password)
	fs.mem.AddUser(fs.user)

	fs.server = imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &wrappedSession{
				UserSession: imapmemserver.NewUserSession(fs.user),
				fs:          fs,
				username:    cfg.username,
				password:    cfg.password,
			}, nil, nil
		},
		Caps:         cfg.caps,
		TLSConfig:    cfg.tls,
		InsecureAuth: cfg.insecure || cfg.tls == nil,
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if cfg.tls != nil {
		ln = tls.NewListener(ln, cfg.tls)
	}
	fs.ln = ln
	fs.addr = ln.Addr().String()
	go func() { _ = fs.server.Serve(ln) }()
	t.Cleanup(fs.Close)
	return fs
}

func newFakeTLSServer(t *testing.T, cfg serverConfig) *fakeServer {
	t.Helper()
	cert := selfSignedCert(t)
	cfg.tls = &tls.Config{Certificates: []tls.Certificate{cert}}
	return newFakeServer(t, cfg)
}

func (fs *fakeServer) Addr() string { return fs.addr }

func (fs *fakeServer) HostPort() (string, int) {
	host, portStr, _ := net.SplitHostPort(fs.addr)
	port := 0
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	return host, port
}

func (fs *fakeServer) AddMailbox(name string) {
	fs.t.Helper()
	if err := fs.user.Create(name, nil); err != nil {
		fs.t.Fatalf("create mailbox %q: %v", name, err)
	}
}

func (fs *fakeServer) setNamespace(prefix string, delim rune) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.overrides.namespacePrefix = &prefix
	fs.overrides.namespaceDelim = &delim
}

func (fs *fakeServer) setUIDValidity(v uint32) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.overrides.fixedUIDValidity = v
}

func (fs *fakeServer) setAppendNoUID(v bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.overrides.appendNoUID = v
}

func (fs *fakeServer) setDeleteNonEmpty(v bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.overrides.deleteNonEmpty = v
}

func (fs *fakeServer) overridesSnapshot() sessionOverrides {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.overrides
}

func (fs *fakeServer) Close() {
	fs.closeOnce.Do(func() {
		if fs.server != nil {
			_ = fs.server.Close()
		}
		if fs.ln != nil {
			_ = fs.ln.Close()
		}
	})
}

// wrappedSession embeds the in-memory user session and overrides the methods a
// test needs to control.
type wrappedSession struct {
	*imapmemserver.UserSession
	fs       *fakeServer
	username string
	password string
}

var _ imapserver.SessionIMAP4rev2 = (*wrappedSession)(nil)

func (s *wrappedSession) Login(username, password string) error {
	if username != s.username || password != s.password {
		return imapserver.ErrAuthFailed
	}
	s.UserSession = imapmemserver.NewUserSession(s.fs.user)
	return nil
}

func (s *wrappedSession) Namespace() (*imap.NamespaceData, error) {
	o := s.fs.overridesSnapshot()
	prefix := ""
	delim := '/'
	if o.namespacePrefix != nil {
		prefix = *o.namespacePrefix
	}
	if o.namespaceDelim != nil {
		delim = *o.namespaceDelim
	}
	return &imap.NamespaceData{
		Personal: []imap.NamespaceDescriptor{{Prefix: prefix, Delim: delim}},
	}, nil
}

func (s *wrappedSession) Select(name string, options *imap.SelectOptions) (*imap.SelectData, error) {
	s.fs.mu.Lock()
	s.fs.overrides.selectCount++
	started, release := s.fs.overrides.selectStarted, s.fs.overrides.selectRelease
	s.fs.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}
	data, err := s.UserSession.Select(name, options)
	if err != nil {
		return nil, err
	}
	if o := s.fs.overridesSnapshot(); o.fixedUIDValidity != 0 {
		data.UIDValidity = o.fixedUIDValidity
	}
	return data, nil
}

func (s *wrappedSession) Append(mailbox string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	data, err := s.UserSession.Append(mailbox, r, options)
	if err != nil {
		return nil, err
	}
	if o := s.fs.overridesSnapshot(); o.appendNoUID {
		return nil, nil
	}
	return data, nil
}

func (s *wrappedSession) Delete(name string) error {
	if o := s.fs.overridesSnapshot(); o.deleteNonEmpty {
		return &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeHasChildren,
			Text: "Mailbox has children",
		}
	}
	return s.UserSession.Delete(name)
}
