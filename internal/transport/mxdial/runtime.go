package mxdial

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dellarb/mailmoose/internal/mxwire"
	"github.com/dellarb/mailmoose/internal/transport/netutil"
)

// Domain is one configured receiving domain the core is willing to speak for.
// The receiver proves control of the CORE signing key, not of the receiver
// domain: the dialer signs a challenge with Domain.PrivateKey and the receiver
// verifies it against the MM1 TXT record published for the core domain.
//
// ContactEmail and SetupID are optional Antler MX registration metadata. They
// are operational metadata only, never a credential: domain authority remains
// the DNS-anchored proof. Custom receivers leave both empty.
type Domain struct {
	AccountID    string
	ID           string
	Name         string
	KeyID        string
	PrivateKey   ed25519.PrivateKey
	ReceiverURLs []string
	ContactEmail string
	SetupID      string
}

// Status is the observable per-domain state for one receiver URL.
type Status struct {
	ReceiverURL  string
	KeyID        string
	State        string
	Reason       string
	SMTPHostname string
	ExpiresAt    time.Time
}

// Status state vocabulary, shared by the per-domain rows and the single-mode
// connection row. "connecting" is the only in-progress state and means an
// authentication exchange is genuinely outstanding; a physical session that
// cannot be established at all is reported as its own state so the UI can tell
// a receiver that is down from one that is still authorizing this domain.
const (
	// StatusActive: the receiver granted this domain's key over a live session.
	StatusActive = "ready"
	// StatusConnecting: an authentication is outstanding or awaiting retry.
	StatusConnecting = "connecting"
	// StatusRejected: the receiver answered and refused, or answered something
	// this core could not trust. The receiver is reachable.
	StatusRejected = "rejected"
	// StatusUnreachable: no session could be established at all (DNS/TCP/TLS/
	// HTTP handshake failure). Nothing about this domain's authorization is
	// known, so it must never be presented as "connecting".
	StatusUnreachable = "unreachable"
	// StatusUnavailable: the receiver terminated the binding from its side.
	StatusUnavailable = "unavailable"
	// StatusDeferred: the receiver's advertised capacity is full for now.
	StatusDeferred = "deferred"
	// StatusDisconnected: an established session ended. Retrying.
	StatusDisconnected = "disconnected"
)

// Backend is the application surface shared by private bearer sessions and
// shared sessions with DNS-authenticated domain channels.
type Backend interface {
	Domains(context.Context) ([]Domain, error)
	Resolve(context.Context, string, []string) (mxwire.ResolveResponse, error)
	// Ingest persists one staged message. receiverURL is the canonical base URL
	// of the receiver session the message arrived on, so the core can label the
	// activity-log source with the concrete receiver.
	Ingest(ctx context.Context, domains []string, meta mxwire.IngestMetadata, path, receiverURL string) (mxwire.IngestResponse, error)
}

// Config bounds the manager. Zero values select the documented defaults.
type Config struct {
	ReceiverURL        string
	CoreKey            string
	DataDir            string
	MaxMessageBytes    int64
	MaxTransactions    int
	TLSConfig          *tls.Config
	ReconcileInterval  time.Duration
	TransactionTimeout time.Duration
	// AuthRetryInterval overrides the base rejected-authentication cooldown so
	// deterministic tests need not wait the production window.
	AuthRetryInterval time.Duration
	// MaxDomainsPerShard is the local fallback cap on distinct domains sharing
	// one shared-mode session when a receiver does not advertise Ready.MaxDomains.
	// Zero selects mxwire.MaxAdvertisedDomains. It never applies to single mode.
	MaxDomainsPerShard int
	// MaxShardsPerReceiver caps how many stable shards the core opens to one
	// receiver URL in shared mode. Zero means derive from the domain count and
	// the per-shard cap, up to hardMaxShards.
	MaxShardsPerReceiver int
	// MaxAuthInflight bounds concurrent domain authentications across every
	// session. Zero selects defaultMaxAuthInflight. It paces authentication so a
	// receiver is never flooded by a burst of DomainAuth frames.
	MaxAuthInflight int
	// AllowPrivateDestinations applies only to this manager's receiver connections.
	// Private installation receivers deliberately allow loopback/LAN destinations.
	AllowPrivateDestinations bool
}

const (
	defaultMaxTransactions = 32
	maxRecipientsPerTx     = mxwire.MaxResolveRecipients
	maxDomainsPerIngest    = mxwire.MaxResolveRecipients
	ingestBackendTimeout   = 2 * time.Minute
	resolveBackendTimeout  = 30 * time.Second
	domainsBackendTimeout  = 5 * time.Second
	authRetryMin           = 5 * time.Second
	authRetryMax           = 30 * time.Second
	readyTimeout           = 5 * time.Second
	writeTimeout           = 10 * time.Second
	sessionIdleTimeout     = 90 * time.Second
	keepaliveInterval      = 30 * time.Second
	maxBackoff             = 60 * time.Second
	minStableLifetime      = 30 * time.Second
	frameQueueDepth        = 64
	defaultMaxAuthInflight = 8
	hardMaxShards          = 8
	shardKeySep            = "\x1f"
)

// Manager dials every configured receiver URL with a dedicated HTTP/2 session,
// reconciles the configured domain set, and exposes per-domain status. It never
// listens: the shared application's inbound HTTP listener is owned elsewhere.
type Manager struct {
	backend Backend
	cfg     Config
	wake    chan struct{}

	// mu guards only the session map, the domain-ownership map, the per-receiver
	// statuses and the advertised receiver caps. It is never held across a
	// session call, a network operation, or a backend call.
	mu         sync.Mutex
	runCtx     context.Context
	started    bool
	sessions   map[string]*session // key: shardKey(receiver URL, shard index)
	owner      map[string]*session // key: statusKey(domain, receiver URL) -> owning shard
	status     map[string]Status   // key: domain + "\x00" + receiver URL
	connection Status              // single-mode connection readiness, guarded by mu
	// readyCaps records the advertised Ready limits per receiver URL, learned
	// once a shard completes its handshake. It lets a later reconcile resize the
	// shard count to what the receiver actually admits.
	readyCaps map[string]mxwire.Ready
	// assign is the persisted stable shard assignment: receiver URL -> domain ->
	// shard index. It is what makes sharding capacity-packed and churn-free: an
	// existing domain keeps its shard until it is removed, and a new domain is
	// placed in the lowest-index shard with spare capacity.
	assign   map[string]map[string]int
	stopping bool
	ownWG    sync.WaitGroup

	// slots is the single global transaction cap shared across every
	// connection.
	slots chan struct{}
	// authSem is the global authentication pacing budget shared across every
	// session, so many shards cannot burst DomainAuth frames at one receiver.
	authSem chan struct{}
}

func New(backend Backend, cfg Config) *Manager {
	if cfg.MaxMessageBytes <= 0 {
		cfg.MaxMessageBytes = mxwire.DefaultMaxBodyBytes
	}
	if cfg.MaxTransactions <= 0 {
		cfg.MaxTransactions = defaultMaxTransactions
	}
	if cfg.ReconcileInterval <= 0 {
		cfg.ReconcileInterval = 2 * time.Second
	}
	if cfg.TransactionTimeout <= 0 {
		cfg.TransactionTimeout = 3 * time.Minute
	}
	if cfg.AuthRetryInterval <= 0 {
		cfg.AuthRetryInterval = authRetryMin
	}
	if cfg.MaxDomainsPerShard <= 0 {
		cfg.MaxDomainsPerShard = mxwire.MaxAdvertisedDomains
	}
	if cfg.MaxShardsPerReceiver < 0 {
		cfg.MaxShardsPerReceiver = 0
	}
	if cfg.MaxAuthInflight <= 0 {
		cfg.MaxAuthInflight = defaultMaxAuthInflight
	}
	if cfg.TLSConfig != nil {
		// Never permit a caller to disable certificate verification on the
		// production dialer; a custom root pool is the supported override.
		cfg.TLSConfig = cfg.TLSConfig.Clone()
		cfg.TLSConfig.InsecureSkipVerify = false
	}
	return &Manager{
		backend:   backend,
		cfg:       cfg,
		wake:      make(chan struct{}, 1),
		sessions:  map[string]*session{},
		owner:     map[string]*session{},
		status:    map[string]Status{},
		readyCaps: map[string]mxwire.Ready{},
		assign:    map[string]map[string]int{},
		runCtx:    context.Background(),
		slots:     make(chan struct{}, cfg.MaxTransactions),
		authSem:   make(chan struct{}, cfg.MaxAuthInflight),
	}
}

// Wake requests an immediate reconcile pass. It never blocks.
func (m *Manager) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// ConnectionStatus reports the private receiver's actual handshake state.
func (m *Manager) ConnectionStatus() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connection.State == "" {
		return Status{ReceiverURL: m.cfg.ReceiverURL, State: StatusConnecting}
	}
	return m.connection
}

func (s *session) connectionStatus(state, reason string) {
	if !s.single() {
		return
	}
	s.m.mu.Lock()
	s.m.connection = Status{ReceiverURL: s.url, State: state, Reason: reason, SMTPHostname: s.ready.SMTPHostname}
	s.m.mu.Unlock()
}

// Status returns the current per-receiver status for a canonical domain.
//
// A receiver the domain is configured for but which has no live row is reported
// explicitly instead of being omitted: a caller that only ever saw live rows
// could not tell "no session has been attempted yet" from "the receiver is
// unreachable", and would have to invent a state for the gap. A single-mode
// manager knows the real connection state and reports it; a shared-mode manager
// reports "connecting" until the receiver answers.
func (m *Manager) Status(domain string) []Status {
	d, err := mxwire.CanonicalDomain(domain)
	if err != nil {
		return nil
	}
	m.mu.Lock()
	var out []Status
	for k, v := range m.status {
		if strings.HasPrefix(k, d+"\x00") {
			out = append(out, v)
		}
	}
	connection := m.connection
	single := m.cfg.ReceiverURL != ""
	m.mu.Unlock()
	if single {
		if !statusListed(out, m.cfg.ReceiverURL) {
			out = append(out, connectionRow(connection, m.cfg.ReceiverURL))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ReceiverURL < out[j].ReceiverURL })
	return out
}

// statusListed reports whether a receiver URL already has a row.
func statusListed(rows []Status, url string) bool {
	for _, r := range rows {
		if r.ReceiverURL == url {
			return true
		}
	}
	return false
}

// connectionRow normalizes the single-mode connection row: an unset row means
// the dialer has not completed an attempt yet, which is "connecting" — never a
// fabricated ready or disconnected state.
func connectionRow(connection Status, url string) Status {
	if connection.State == "" {
		return Status{ReceiverURL: url, State: StatusConnecting}
	}
	return connection
}

// Run reconciles until ctx is canceled, then closes every session and waits for
// all of them to finish. It must be called at most once.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	runCtx, cancel := context.WithCancel(ctx)
	m.runCtx = runCtx
	m.mu.Unlock()
	defer cancel()

	// Sweep staging files left behind by a previous crash before serving. The
	// core shares messages/.tmp but only writes "in*.eml"; the "mxdial-*"
	// namespace is this manager's alone.
	m.sweepStaleTemp()

	t := time.NewTicker(m.cfg.ReconcileInterval)
	defer t.Stop()
	for {
		m.reconcile(runCtx)
		select {
		case <-runCtx.Done():
			m.stopAll()
			return
		case <-m.wake:
		case <-t.C:
		}
	}
}

// sweepStaleTemp removes mxdial staging files left by a previous process. A
// file is removed only when it is older than the grace window, so a second
// manager sharing the directory is not disrupted mid-write.
func (m *Manager) sweepStaleTemp() {
	root := filepath.Join(m.cfg.DataDir, "messages", ".tmp")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-sweepGrace)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "mxdial-") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(root, e.Name()))
	}
}

// sweepGrace is how old an mxdial staging file must be before the startup sweep
// removes it, so a concurrently running manager is never disrupted.
const sweepGrace = 10 * time.Minute

func (m *Manager) stopAll() {
	m.mu.Lock()
	m.stopping = true
	sessions := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = map[string]*session{}
	m.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
	m.ownWG.Wait()
}

// reconcile computes the desired URL->[shard]->domain set and starts, updates
// or stops sessions. It holds m.mu only for map bookkeeping and never calls
// into a session while holding it.
func (m *Manager) reconcile(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, domainsBackendTimeout)
	domains, err := m.backend.Domains(ctx)
	cancel()
	if err != nil {
		return
	}
	// perURL collects the domain set configured for each receiver URL.
	perURL := map[string]map[string]Domain{}
	if m.cfg.ReceiverURL != "" {
		perURL[m.cfg.ReceiverURL] = map[string]Domain{}
	}
	for _, d := range domains {
		name, e := mxwire.CanonicalDomain(d.Name)
		if e != nil || len(d.PrivateKey) != ed25519.PrivateKeySize || d.KeyID == "" {
			continue
		}
		d.Name = name
		for _, raw := range d.ReceiverURLs {
			u, e := mxwire.ReceiverURL(raw)
			if e != nil {
				continue
			}
			if perURL[u] == nil {
				perURL[u] = map[string]Domain{}
			}
			perURL[u][name] = d
		}
	}

	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return
	}
	// Build the desired shard -> domain assignment for every URL under the
	// current advertised caps, retaining existing assignments and packing only
	// new domains. Domains that cannot fit within the shard limit are deferred.
	desired := map[string]map[int]map[string]Domain{}
	deferred := map[string][]string{}
	for u, ds := range perURL {
		shards, over := m.packShardsLocked(u, ds)
		desired[u] = shards
		deferred[u] = over
	}

	var toClose []*session
	for key, s := range m.sessions {
		if _, ok := desired[s.url][s.shard]; !ok {
			delete(m.sessions, key)
			toClose = append(toClose, s)
		}
	}
	var toStart []*session
	type upd struct {
		s  *session
		ds map[string]Domain
	}
	var toUpdate []upd
	for u, shards := range desired {
		for shard, ds := range shards {
			key := shardKey(u, shard)
			if s := m.sessions[key]; s == nil {
				s := m.newSession(u, shard, ds)
				m.sessions[key] = s
				m.ownWG.Add(1)
				toStart = append(toStart, s)
			} else {
				toUpdate = append(toUpdate, upd{s, ds})
			}
		}
	}
	// Reassign status ownership to the shard that now owns each domain. A
	// domain that moved shards has its old status cleared so the new shard's
	// first write is never blocked by a stale owner.
	active := map[string]bool{}
	for u, shards := range desired {
		for _, ds := range shards {
			for name := range ds {
				active[statusKey(name, u)] = true
			}
		}
	}
	for key := range m.status {
		if !active[key] {
			delete(m.status, key)
		}
	}
	for key := range m.owner {
		if !active[key] {
			delete(m.owner, key)
		}
	}
	for u, shards := range desired {
		for shard, ds := range shards {
			s := m.sessions[shardKey(u, shard)]
			for name := range ds {
				k := statusKey(name, u)
				if m.owner[k] != s {
					m.owner[k] = s
					delete(m.status, k)
				}
			}
		}
	}
	// A domain that could not be placed because every shard is at the advertised
	// cap gets an explicit deferred status and is never folded into an oversized
	// shard. When capacity frees up it is packed on a later pass.
	for u, names := range deferred {
		for _, name := range names {
			m.status[statusKey(name, u)] = Status{ReceiverURL: u, State: "deferred", Reason: "domain_capacity"}
		}
	}
	m.mu.Unlock()

	for _, s := range toClose {
		s.close()
	}
	for _, s := range toStart {
		go s.run()
	}
	for _, u := range toUpdate {
		u.s.update(u.ds)
	}
}

// shardCapLocked returns the effective domain capacity of one shard for a URL,
// and the maximum number of shards it may use. Single mode is always one shard
// of unbounded capacity. It must be called with m.mu held.
func (m *Manager) shardCapLocked(url string) (perShard, limit int) {
	if m.cfg.ReceiverURL != "" {
		return 1 << 30, 1
	}
	perShard = m.cfg.MaxDomainsPerShard
	if adv := m.readyCaps[url].MaxDomains; adv > 0 && adv < perShard {
		perShard = adv
	}
	if perShard <= 0 {
		perShard = mxwire.MaxAdvertisedDomains
	}
	limit = hardMaxShards
	if m.cfg.MaxShardsPerReceiver > 0 && m.cfg.MaxShardsPerReceiver < limit {
		limit = m.cfg.MaxShardsPerReceiver
	}
	if limit < 1 {
		limit = 1
	}
	return perShard, limit
}

// packShardsLocked computes the stable, capacity-packed shard assignment for a
// URL. It must be called with m.mu held.
//
// Existing domains keep their shard, so adding a domain never re-authenticates
// the domains already placed. New domains are sorted deterministically and
// placed in the lowest-index shard with spare capacity; a new shard is opened
// only when every existing shard is full and the shard limit allows. A domain
// that cannot be placed returns in the over slice, never in an oversized shard.
//
// A shard is only rebalanced when a reduced advertised cap would leave it over
// capacity; in that case its overflow (highest channel order) is moved to the
// lowest shard with room. Shrinking never compacts healthy shards.
func (m *Manager) packShardsLocked(url string, ds map[string]Domain) (map[int]map[string]Domain, []string) {
	perShard, limit := m.shardCapLocked(url)
	assign := m.assign[url]
	if assign == nil {
		assign = map[string]int{}
		m.assign[url] = assign
	}
	// Prune assignments for domains no longer configured.
	for name := range assign {
		if _, ok := ds[name]; !ok {
			delete(assign, name)
		}
	}
	// Shard -> domains, from retained assignments, preserving insertion order by
	// sorting names so packing is deterministic.
	shards := map[int]map[string]Domain{}
	add := func(shard int, name string) {
		if shards[shard] == nil {
			shards[shard] = map[string]Domain{}
		}
		shards[shard][name] = ds[name]
	}
	var unassigned []string
	for name := range ds {
		if shard, ok := assign[name]; ok {
			add(shard, name)
			continue
		}
		unassigned = append(unassigned, name)
	}
	sort.Strings(unassigned)

	// Rebalance any shard left over capacity (by a reduced advertised cap) or
	// above the shard limit: move its lexicographically greatest names back to
	// the unassigned pool, to be repacked below. This only moves overflow; a
	// healthy, in-bounds shard keeps every domain it has.
	var over []string
	shardKeys := make([]int, 0, len(shards))
	for shard := range shards {
		shardKeys = append(shardKeys, shard)
	}
	sort.Ints(shardKeys)
	for _, shard := range shardKeys {
		members := shards[shard]
		keep := perShard
		if shard >= limit {
			keep = 0
		}
		if len(members) <= keep {
			continue
		}
		names := make([]string, 0, len(members))
		for name := range members {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names[keep:] {
			delete(members, name)
			delete(assign, name)
			unassigned = append(unassigned, name)
		}
	}
	sort.Strings(unassigned)

	// Place unassigned domains into the lowest-index shard with spare capacity,
	// opening new shards up to the limit.
	for _, name := range unassigned {
		placed := false
		for shard := 0; shard < limit; shard++ {
			if len(shards[shard]) < perShard {
				add(shard, name)
				assign[name] = shard
				placed = true
				break
			}
		}
		if !placed {
			over = append(over, name)
		}
	}
	// Drop empty shards so a shard that lost all its domains is closed, but do
	// not move domains between non-empty shards.
	for shard, members := range shards {
		if len(members) == 0 {
			delete(shards, shard)
		}
	}
	if len(shards) == 0 {
		shards[0] = map[string]Domain{}
	}
	return shards, over
}

func shardKey(url string, shard int) string {
	return url + shardKeySep + strconv.Itoa(shard)
}

func statusKey(domain, url string) string { return domain + "\x00" + url }

// setStatus records a domain's state, but only when s is still the shard that
// owns the domain for this receiver URL. A shard that lost a domain to another
// shard (or was torn down) can therefore never overwrite the current owner's
// status with a stale row.
func (m *Manager) setStatus(s *session, domain, state, reason, host string, expires time.Time, keyID string) {
	m.mu.Lock()
	if !m.stopping {
		k := statusKey(domain, s.url)
		if m.owner[k] == s {
			m.status[k] = Status{ReceiverURL: s.url, KeyID: keyID, State: state, Reason: reason, SMTPHostname: host, ExpiresAt: expires}
		}
	}
	m.mu.Unlock()
}

// clearStatus removes a domain's status row only when s currently owns it. A
// shard that lost the domain leaves the new owner's row untouched.
func (m *Manager) clearStatus(s *session, domain string) {
	m.mu.Lock()
	k := statusKey(domain, s.url)
	if m.owner[k] == s {
		delete(m.status, k)
	}
	m.mu.Unlock()
}

// recordReadyCaps stores a receiver's advertised Ready limits and wakes the
// manager so the shard count can be resized to the advertised domain cap.
func (m *Manager) recordReadyCaps(url string, ready mxwire.Ready) {
	m.mu.Lock()
	prev := m.readyCaps[url]
	changed := prev.MaxDomains != ready.MaxDomains
	m.readyCaps[url] = ready
	m.mu.Unlock()
	if changed {
		m.Wake()
	}
}

func keyFingerprint(d Domain) string {
	h := sha256.New()
	h.Write([]byte(d.KeyID))
	h.Write([]byte{0})
	h.Write(d.PrivateKey)
	h.Write([]byte{0})
	for _, u := range d.ReceiverURLs {
		h.Write([]byte(u))
		h.Write([]byte{0})
	}
	// Registration metadata is part of the fingerprint so a contact or setup
	// change forces a fresh DomainAuth carrying the new values.
	h.Write([]byte(d.ContactEmail))
	h.Write([]byte{0})
	h.Write([]byte(d.SetupID))
	h.Write([]byte{0})
	return hex.EncodeToString(h.Sum(nil))
}

func sameDomains(a, b map[string]Domain) bool {
	if len(a) != len(b) {
		return false
	}
	for name, da := range a {
		db, ok := b[name]
		if !ok || keyFingerprint(da) != keyFingerprint(db) {
			return false
		}
	}
	return true
}

// authState is the per-domain authentication state machine, replacing the
// former collection of independent booleans.
type authState uint8

const (
	authPending  authState = iota // DomainAuth sent, awaiting a challenge within deadline
	authActive                    // proof signed and accepted; authorized until expires
	authRejected                  // rejected; retryAt gates the next attempt
	authReplaced                  // granted elsewhere; parked, no new resolve
	authRevoked                   // authority withdrawn
)

// auth is one domain's authentication binding, owned exclusively by the
// connection loop goroutine.
type auth struct {
	domain     Domain
	channel    uint64
	generation string
	state      authState
	expires    time.Time // active authorization expiry
	deadline   time.Time // pending proof window
	retryAt    time.Time
	// proof* bind the challenge we actually signed: an AuthResult is only
	// trusted when the nonce and session identity match the local challenge.
	proofNonce string
	// holdsSlot records whether this auth holds one of the manager's bounded
	// authentication-pacing slots, so it is released exactly once.
	holdsSlot bool
}

// txState is the transaction state machine, owned by the loop.
type txState uint8

const (
	txResolving  txState = iota // accepting sequential recipient resolves; no ingest yet
	txStaging                   // chunks accumulating into the staging file
	txCommitting                // backend ingest in flight
	txCancelled
)

// tx is one receiver transaction, owned exclusively by the connection loop.
type tx struct {
	ctx    context.Context
	cancel context.CancelFunc
	start  time.Time

	domains    map[string]uint64 // domain -> pinned channel
	recipients map[string]map[string]bool
	accepted   map[string]bool

	path string
	file *os.File
	hash interface {
		Write([]byte) (int, error)
		Sum([]byte) []byte
	}
	size int64
	seq  uint32
	meta mxwire.IngestMetadata
	busy bool
	// resolveBusy forbids a second outstanding resolve for this transaction:
	// SMTP recipients are resolved one at a time per transaction.
	resolveBusy bool
	state       txState
}

// session owns one stable shard of one receiver URL. All auth and transaction
// maps are touched only by the run goroutine; no lock protects them.
type session struct {
	m     *Manager
	url   string
	shard int

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	updateCh chan map[string]Domain
	// wantMu guards the latest configuration delivered by reconcile.
	wantMu  sync.Mutex
	desired map[string]Domain

	// lifeMu guards the physical connection handles so close can tear them down
	// without racing connect.
	lifeMu        sync.Mutex
	writer        *io.PipeWriter
	requestCancel context.CancelFunc

	// outMu serializes frame writes on the current physical connection.
	outMu sync.Mutex

	// loop-owned state, reset at the start of every physical connection.
	domains map[string]Domain
	auths   map[string]*auth
	byChan  map[uint64]*auth
	nextCh  uint64
	ready   mxwire.Ready
	txs     map[uint64]*tx
	sawAuth bool // an auth reached active during this connection
	// authInflight is the number of initial authentications this session has
	// outstanding, bounded by authLimit so one shard cannot burst the receiver.
	authInflight int
	authLimit    int

	// jobs carries backend worker completions to the loop, including cancellation.
	jobs          chan jobResult
	connectionCtx context.Context
	// connWG tracks resolve/ingest workers for the current connection.
	connWG sync.WaitGroup
}

func (m *Manager) newSession(u string, shard int, initial map[string]Domain) *session {
	ctx, cancel := context.WithCancel(m.runCtx)
	return &session{
		m:        m,
		url:      u,
		shard:    shard,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		updateCh: make(chan map[string]Domain, 1),
		desired:  initial,
	}
}

// update queues a desired-configuration change onto the session loop. It never
// blocks and never touches the writer.
func (s *session) update(ds map[string]Domain) {
	s.wantMu.Lock()
	changed := !sameDomains(s.desired, ds)
	if changed {
		s.desired = ds
	}
	s.wantMu.Unlock()
	if !changed {
		return
	}
	select {
	case s.updateCh <- ds:
	default:
	}
}

func (s *session) close() {
	s.cancel()
	s.lifeMu.Lock()
	if s.requestCancel != nil {
		s.requestCancel()
	}
	if s.writer != nil {
		_ = s.writer.CloseWithError(io.ErrClosedPipe)
	}
	s.lifeMu.Unlock()
	<-s.done
}

func (m *Manager) closeSession(s *session) { m.ownWG.Done() }

// acquireAuthSlot takes one global authentication-pacing slot without blocking.
// A false result means the caller must defer the attempt to a later tick.
func (m *Manager) acquireAuthSlot() bool {
	select {
	case m.authSem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (m *Manager) releaseAuthSlot() {
	select {
	case <-m.authSem:
	default:
	}
}

func jitter(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	half := d / 2
	return half + time.Duration(time.Now().UnixNano()%int64(half+1))
}

// Failure reasons a receiver row can carry. They are stable tokens (the UI and
// the API surface them), not prose: boundStatusReason still truncates anything
// a receiver supplies.
const (
	// ReasonDisconnected means an established session ended. Retrying.
	ReasonDisconnected = "disconnected"
	// ReasonRejected means the receiver answered and refused this core's bearer
	// key. The receiver is reachable; the credential is the problem.
	ReasonRejected = "session_rejected"
	// ReasonDestinationNotAllowed means the outbound policy refused the receiver
	// destination (a private or non-routable address without an opt-in).
	ReasonDestinationNotAllowed = "destination_not_allowed"
	// ReasonUnreachable means no physical session could be established.
	ReasonUnreachable = "unreachable"
	// ReasonDNSFailure means the receiver hostname did not resolve.
	ReasonDNSFailure = "dns_failure"
	// ReasonTLSCertificate means the receiver's certificate could not be verified.
	ReasonTLSCertificate = "tls_certificate"
	// ReasonHTTP2Required means the receiver answered with HTTP/1.1.
	ReasonHTTP2Required = "http2_required"
	// ReasonIdleTimeout means an established session was closed after idling.
	ReasonIdleTimeout = "idle_timeout"
)

// ConnectionFailureReason classifies one physical session failure so an operator
// can see why the core could not reach a receiver. An error carrying a specific
// DNS or TLS text is reported as its own token; everything else that is not a
// protocol-shape error is a plain unreachable receiver.
func ConnectionFailureReason(err error) string {
	if err == nil {
		return ReasonUnreachable
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "dns_failure"),
		strings.Contains(msg, "name resolution"), strings.Contains(msg, "server misbehaving"):
		return ReasonDNSFailure
	case strings.Contains(msg, "not public-routable"), strings.Contains(msg, "resolved no addresses"):
		return ReasonDestinationNotAllowed
	case strings.Contains(msg, "x509"), strings.Contains(msg, "certificate"),
		strings.Contains(msg, "tls:"), strings.Contains(msg, "unrecognized name"),
		strings.Contains(msg, "handshake failure"):
		return ReasonTLSCertificate
	case errors.Is(err, errHTTP1):
		return ReasonHTTP2Required
	case errors.Is(err, errIdle):
		return ReasonIdleTimeout
	case errors.Is(err, ErrSessionRejected):
		return ReasonRejected
	}
	return ReasonUnreachable
}

// unreachableReason reports whether a failure token means the core could not talk
// to the receiver at all. A receiver that answered — a refused bearer, an
// HTTP/1.1 reply — is not unreachable, and neither is a destination the outbound
// policy refused before any dial: those are reachable-but-unusable or not
// attempted, and calling them unreachable would send the operator looking at the
// receiver's network instead of the credential or the policy.
func unreachableReason(reason string) bool {
	switch reason {
	case ReasonUnreachable, ReasonDNSFailure, ReasonTLSCertificate:
		return true
	}
	return false
}

// failureState maps one failed connection attempt onto the state a receiver row
// carries. A receiver that has never answered is unreachable; one that has been
// reached before is disconnected and retrying; one that answered but refused is
// disconnected, because the receiver itself is fine.
func failureState(reason string, everConnected bool) string {
	if !everConnected && unreachableReason(reason) {
		return StatusUnreachable
	}
	return StatusDisconnected
}

// backoffFor is the doubling backoff a repeatedly failing connection uses.
func backoffFor(attempt int) time.Duration {
	d := time.Second
	for i := 1; i < attempt && d < maxBackoff; i++ {
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// run owns the session lifetime: it reconnects until the session is closed.
func (s *session) run() {
	defer s.m.closeSession(s)
	defer close(s.done)

	// everConnected distinguishes a receiver that was reached at least once
	// from one that has never answered: only the latter is "unreachable".
	everConnected := false
	attempt := 0
	for s.ctx.Err() == nil {
		started := time.Now()
		s.sawAuth = false
		err := s.connect()
		s.connectionStatus(StatusDisconnected, ReasonDisconnected)
		reason := ""
		if err != nil {
			reason = ConnectionFailureReason(err)
		}
		live := s.configuredDomains()
		// Reachability is the handshake, not the domain set: a session that
		// was given domains it never got to talk about is still unreachable.
		handshake := s.ready.ReceiverID != ""
		if len(live) == 0 {
			// Single mode owns one connection row and no per-domain rows, so
			// the physical failure is reported there. A receiver that is down
			// must not be shown as still authorizing.
			s.connectionStatus(failureState(reason, everConnected), reason)
		}
		s.resetConnection()
		if s.ctx.Err() != nil {
			return
		}
		// Only a session that stayed authorized for a meaningful period resets
		// the backoff; a mere Ready handshake is not enough, or a flapping
		// receiver would storm.
		if s.sawAuth && time.Since(started) >= minStableLifetime {
			everConnected = true
			attempt = 0
		}
		if handshake {
			everConnected = true
		}
		for _, d := range live {
			s.m.setStatus(s, d, failureState(reason, everConnected), reason, "", time.Time{}, "")
		}
		wait := jitter(backoffFor(attempt))
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(wait):
		case <-s.updateCh:
		}
		attempt++
	}
}

func (s *session) trackedDomains() []string {
	out := make([]string, 0, len(s.domains))
	for d := range s.domains {
		out = append(out, d)
	}
	return out
}

// configuredDomains returns the domains this session is responsible for,
// whether or not a handshake ever completed. A session that cannot reach its
// receiver must still report that fact for the domains it was given: using the
// per-connection set instead would leave an unreachable receiver with no status
// row at all, and a caller then has to invent a state for it.
func (s *session) configuredDomains() []string {
	s.wantMu.Lock()
	defer s.wantMu.Unlock()
	out := make([]string, 0, len(s.desired))
	for d := range s.desired {
		out = append(out, d)
	}
	out = append(out, s.trackedDomains()...)
	return out
}

var (
	errHTTP1 = errors.New("HTTP/2 required")
	errIdle  = errors.New("session idle timeout")
	// ErrSessionRejected is exported so callers can attribute a refusal (the
	// receiver answered and said no) apart from a reachability failure.
	ErrSessionRejected = errors.New("session rejected")
)

// connect performs one logical session. The connection loop it runs owns all
// auth and transaction state for the connection's lifetime; backend workers
// report back through s.jobs.
func (s *session) connect() error {
	s.connectionStatus(StatusConnecting, "")
	pr, pw := io.Pipe()
	s.lifeMu.Lock()
	if s.ctx.Err() != nil {
		s.lifeMu.Unlock()
		_ = pw.CloseWithError(io.ErrClosedPipe)
		_ = pr.CloseWithError(io.ErrClosedPipe)
		return s.ctx.Err()
	}
	s.writer = pw
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.m.cfg.TLSConfig != nil {
		tlsCfg = s.m.cfg.TLSConfig.Clone()
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return netutil.DialContext(ctx, network, addr, !s.m.cfg.AllowPrivateDestinations)
		},
		TLSClientConfig:       tlsCfg,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: readyTimeout,
	}
	tr.Protocols = new(http.Protocols)
	tr.Protocols.SetHTTP2(true)
	if s.single() {
		tr.Protocols.SetUnencryptedHTTP2(true)
	}
	reqCtx, cancel := context.WithCancel(s.ctx)
	s.connectionCtx = reqCtx
	s.requestCancel = cancel
	s.lifeMu.Unlock()

	// Tear down every physical handle exactly once on return.
	defer func() {
		s.lifeMu.Lock()
		s.requestCancel = nil
		s.writer = nil
		s.lifeMu.Unlock()
		cancel()
		_ = pw.CloseWithError(io.ErrClosedPipe)
		_ = pr.CloseWithError(io.ErrClosedPipe)
		tr.CloseIdleConnections()
	}()

	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, s.url+mxwire.SessionPath, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if s.single() {
		req.Header.Set("Authorization", "Bearer "+s.m.cfg.CoreKey)
	}

	type doResult struct {
		resp *http.Response
		err  error
	}
	done := make(chan doResult, 1)
	go func() {
		resp, e := client.Do(req)
		done <- doResult{resp, e}
	}()

	hello, _ := mxwire.JSONFrame(mxwire.FrameHello, 0, 0, mxwire.Hello{Version: mxwire.V2Protocol, Instance: "gatehouse"})
	if err = s.write(hello); err != nil {
		// The transport may already have failed the dial, in which case it
		// closed the request body and the write reports only "closed pipe".
		// Prefer the transport's error: it names the real cause (a refused
		// connection, a refused destination), which is what the receiver row
		// must report.
		select {
		case r := <-done:
			if r.err != nil {
				return r.err
			}
			if r.resp != nil {
				_ = r.resp.Body.Close()
			}
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-time.After(readyTimeout):
		}
		return err
	}

	var resp *http.Response
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-reqCtx.Done():
		return reqCtx.Err()
	case <-time.After(readyTimeout):
		return errors.New("handshake timeout")
	case r := <-done:
		if r.err != nil {
			return r.err
		}
		resp = r.resp
	}
	if resp.ProtoMajor != 2 {
		return errHTTP1
	}
	if resp.StatusCode != http.StatusOK {
		return ErrSessionRejected
	}
	defer resp.Body.Close()

	// Fresh per-connection state, owned solely by this loop.
	s.wantMu.Lock()
	s.domains = map[string]Domain{}
	for name, d := range s.desired {
		s.domains[name] = d
	}
	s.wantMu.Unlock()
	s.auths = map[string]*auth{}
	s.byChan = map[uint64]*auth{}
	s.nextCh = 0
	s.authInflight = 0
	s.txs = map[uint64]*tx{}
	s.jobs = make(chan jobResult, s.m.cfg.MaxTransactions)

	ready, err := s.readReady(resp.Body)
	if err != nil {
		return err
	}
	s.ready = ready
	if s.single() && ready.Mode != "single" || !s.single() && ready.Mode == "single" {
		return errors.New("receiver mode mismatch")
	}
	// Bound this session's concurrent authentications by both the manager's
	// global pacing budget and the receiver's advertised limit, whichever is
	// tighter.
	s.authLimit = s.m.cfg.MaxAuthInflight
	if ready.MaxAuthInflight > 0 && ready.MaxAuthInflight < s.authLimit {
		s.authLimit = ready.MaxAuthInflight
	}
	// Advertised limits may resize the shard count; record them before the
	// first authenticate pass and let reconcile apply any change.
	s.m.recordReadyCaps(s.url, ready)
	if err = s.authenticateAll(); err != nil {
		return err
	}
	s.connectionStatus("ready", "")

	// Reader goroutine: bounded frame queue plus a terminal error result. It
	// never touches loop-owned state.
	type frameResult struct {
		frame mxwire.Frame
		err   error
	}
	frames := make(chan frameResult, frameQueueDepth)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			f, e := mxwire.ReadFrame(resp.Body)
			select {
			case frames <- frameResult{f, e}:
			case <-reqCtx.Done():
				return
			}
			if e != nil {
				return
			}
		}
	}()
	defer func() {
		cancel()
		_ = resp.Body.Close()
		<-readerDone
	}()

	ping := time.NewTicker(keepaliveInterval)
	defer ping.Stop()
	authTick := time.NewTicker(time.Second)
	defer authTick.Stop()
	idle := time.NewTimer(sessionIdleTimeout)
	defer idle.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-reqCtx.Done():
			return reqCtx.Err()
		case <-authTick.C:
			now := time.Now()
			for id, t := range s.txs {
				if !now.Before(t.start.Add(s.m.cfg.TransactionTimeout)) {
					s.cancelTx(id)
				}
			}
			if err = s.authenticateAll(); err != nil {
				return err
			}
		case <-ping.C:
			if err = s.writeJSON(mxwire.FramePing, 0, 0, struct{}{}); err != nil {
				return err
			}
		case <-idle.C:
			return errIdle
		case <-s.updateCh:
			// A configuration change while connected: fold it in, unregister
			// changed/removed domains, and re-auth. Does not tear the
			// connection.
			s.applyConfig()
			if err = s.authenticateAll(); err != nil {
				return err
			}
		case r := <-s.jobs:
			s.completeJob(r)
		case r := <-frames:
			if r.err != nil {
				return r.err
			}
			resetTimer(idle, sessionIdleTimeout)
			if err = s.handleFrame(r.frame); err != nil {
				return err
			}
		}
	}
}

func (s *session) readReady(body io.Reader) (mxwire.Ready, error) {
	type result struct {
		f   mxwire.Frame
		err error
	}
	ch := make(chan result, 1)
	go func() { f, e := mxwire.ReadFrame(body); ch <- result{f, e} }()
	select {
	case <-s.ctx.Done():
		return mxwire.Ready{}, s.ctx.Err()
	case <-time.After(readyTimeout):
		return mxwire.Ready{}, errors.New("ready timeout")
	case r := <-ch:
		if r.err != nil {
			return mxwire.Ready{}, r.err
		}
		if r.f.Type != mxwire.FrameReady || r.f.TxID != 0 || r.f.ChannelID != 0 {
			return mxwire.Ready{}, errors.New("invalid ready")
		}
		var ready mxwire.Ready
		if mxwire.DecodeFrame(r.f, &ready) != nil || ready.Version != mxwire.V2Protocol || ready.ReceiverID == "" || ready.ConnectionID == "" || ready.MaxMessageBytes <= 0 {
			return mxwire.Ready{}, errors.New("invalid ready")
		}
		if len(ready.ReceiverID) > 128 || len(ready.ConnectionID) > 128 {
			return mxwire.Ready{}, errors.New("invalid ready identity")
		}
		return ready, nil
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// resetConnection runs after connect returns: cancel every transaction, wait
// for backend workers to finish (so no file is deleted while in use), then
// discard connection state.
func (s *session) resetConnection() {
	s.releaseAllAuths()
	for _, t := range s.txs {
		t.cancel()
	}
	s.connWG.Wait()
	for _, t := range s.txs {
		t.cleanup(s)
	}
	s.domains = map[string]Domain{}
	s.auths = map[string]*auth{}
	s.byChan = map[uint64]*auth{}
	s.txs = map[uint64]*tx{}
}

func (s *session) handleFrame(f mxwire.Frame) error {
	switch f.Type {
	case mxwire.FrameChallenge:
		return s.challenge(f)
	case mxwire.FrameAuthResult:
		if err := s.authResult(f); err != nil {
			return err
		}
		// A completed authentication frees a pacing slot. Draining further
		// authentications immediately (rather than waiting for the 1s tick)
		// keeps a session with many domains authenticating in bounded batches
		// back-to-back.
		return s.authenticateAll()
	case mxwire.FrameDomainRevoked:
		return s.notice(f, true)
	case mxwire.FrameDomainUnregister:
		return s.notice(f, false)
	case mxwire.FramePing:
		if f.TxID != 0 || f.ChannelID != 0 {
			return errors.New("invalid ping")
		}
		return s.writeJSON(mxwire.FramePong, 0, 0, struct{}{})
	case mxwire.FramePong:
		return nil
	case mxwire.FrameResolve:
		return s.resolve(f)
	case mxwire.FrameIngestStart, mxwire.FrameIngestChunk, mxwire.FrameIngestEnd, mxwire.FrameCancel:
		return s.ingestFrame(f)
	default:
		return errors.New("unexpected frame")
	}
}

// applyConfig folds the latest desired configuration into the loop's domain and
// auth maps. A changed or removed domain is unregistered and its locally-bound
// transactions are aborted; nothing is inherited from the previous setting.
func (s *session) applyConfig() {
	s.wantMu.Lock()
	ds := s.desired
	s.wantMu.Unlock()
	if ds == nil {
		ds = map[string]Domain{}
	}
	var removed []string
	for name, prev := range s.domains {
		next, ok := ds[name]
		if ok && keyFingerprint(prev) == keyFingerprint(next) {
			continue
		}
		removed = append(removed, name)
	}
	s.domains = map[string]Domain{}
	for name, d := range ds {
		s.domains[name] = d
	}
	for _, name := range removed {
		if ad := s.auths[name]; ad != nil {
			_ = s.writeJSON(mxwire.FrameDomainUnregister, 0, ad.channel, mxwire.DomainNotice{Domain: name, Reason: "configuration_changed"})
		}
		s.m.clearStatus(s, name)
		s.retire(name)
		s.cancelDomain(name)
	}
}

// authenticateAll ensures every configured domain has an in-flight or live
// auth, re-issuing for unauthenticated, expired, rejected-after-retry, or
// superseded domains.
func (s *session) authenticateAll() error {
	if s.single() {
		s.sawAuth = true
		return nil
	}
	now := time.Now()
	var pending []*auth
	for name, d := range s.domains {
		if ad := s.auths[name]; ad != nil && ad.generation == keyFingerprint(d) {
			skip := false
			switch ad.state {
			case authActive:
				skip = now.Before(ad.expires)
			case authPending:
				skip = now.Before(ad.deadline)
			case authRejected:
				skip = now.Before(ad.retryAt)
			case authReplaced:
				// Granted to another connection: park the pin, never reclaim
				// via a fresh auth. Only a config change resets it.
				skip = true
			}
			if skip {
				continue
			}
		}
		// Authentication is paced: a session never has more than authLimit
		// authentications outstanding, and the manager never exceeds its global
		// budget. Anything that cannot be started now is retried on the next
		// tick, after an outstanding challenge completes.
		if s.authInflight >= s.authLimit || !s.m.acquireAuthSlot() {
			break
		}
		s.retire(name)
		s.nextCh++
		ad := &auth{
			domain:     d,
			channel:    s.nextCh,
			generation: keyFingerprint(d),
			state:      authPending,
			deadline:   now.Add(10 * time.Second),
			holdsSlot:  true,
		}
		s.authInflight++
		s.auths[name] = ad
		s.byChan[ad.channel] = ad
		pending = append(pending, ad)
	}
	for _, ad := range pending {
		if err := s.writeJSON(mxwire.FrameDomainAuth, 0, ad.channel, mxwire.DomainAuth{
			Domain:       ad.domain.Name,
			KeyID:        ad.domain.KeyID,
			ContactEmail: ad.domain.ContactEmail,
			SetupID:      ad.domain.SetupID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// retire removes a domain's auth binding and returns its pacing slot if it held
// one.
func (s *session) retire(name string) {
	if ad := s.auths[name]; ad != nil {
		delete(s.byChan, ad.channel)
		s.releaseAuth(ad)
	}
	delete(s.auths, name)
}

// releaseAuth returns one authentication-pacing slot exactly once.
func (s *session) releaseAuth(ad *auth) {
	if ad == nil || !ad.holdsSlot {
		return
	}
	ad.holdsSlot = false
	if s.authInflight > 0 {
		s.authInflight--
	}
	s.m.releaseAuthSlot()
}

// releaseAllAuths returns every pacing slot held by this connection's auths.
// It is called when a physical connection ends so slots never leak across a
// reconnect.
func (s *session) releaseAllAuths() {
	for _, ad := range s.auths {
		s.releaseAuth(ad)
	}
	s.authInflight = 0
}

// challenge answers a challenge. A revalidation reuses the same channel and
// records only a fresh proof deadline: it never overwrites a live
// authorization's expiry, so a renewal cannot shorten it.
func (s *session) challenge(f mxwire.Frame) error {
	if f.TxID != 0 {
		return errors.New("invalid challenge tx")
	}
	var c mxwire.Challenge
	if mxwire.DecodeFrame(f, &c) != nil {
		return errors.New("invalid challenge")
	}
	ad := s.byChan[f.ChannelID]
	if ad == nil {
		// Stale frame for a retired channel: ignore rather than tear down.
		return nil
	}
	if c.Domain != ad.domain.Name || c.KeyID != ad.domain.KeyID || c.ReceiverID != s.ready.ReceiverID || c.ConnectionID != s.ready.ConnectionID {
		return errors.New("unknown challenge channel")
	}
	sig, err := mxwire.SignChallenge(ad.domain.PrivateKey, c)
	if err != nil {
		return err
	}
	// Record the exact challenge we signed; only a matching AuthResult issued
	// after local signing may activate the binding.
	ad.proofNonce = c.Nonce
	ad.deadline = time.Now().Add(10 * time.Second)
	return s.writeJSON(mxwire.FrameChallengeResponse, 0, f.ChannelID, mxwire.ChallengeResponse{Domain: c.Domain, KeyID: c.KeyID, Nonce: c.Nonce, Signature: sig})
}

func (s *session) authResult(f mxwire.Frame) error {
	if f.TxID != 0 {
		return errors.New("invalid auth result tx")
	}
	var a mxwire.AuthResult
	if mxwire.DecodeFrame(f, &a) != nil {
		return errors.New("invalid auth result")
	}
	ad := s.byChan[f.ChannelID]
	if ad == nil {
		return nil
	}
	if a.Domain != ad.domain.Name || a.KeyID != ad.domain.KeyID {
		return errors.New("invalid auth identity")
	}
	// Every accepted or rejected result is terminal for the initial
	// authentication, so its pacing slot is returned here in all cases.
	s.releaseAuth(ad)
	now := time.Now()
	if !a.Accepted {
		ad.state = authRejected
		ad.proofNonce = ""
		ad.retryAt = now.Add(s.retryDelay(a.Reason))
		s.cancelDomain(a.Domain)
		s.m.setStatus(s, a.Domain, "rejected", authReason(a.Reason), s.ready.SMTPHostname, ad.expires, ad.domain.KeyID)
		return nil
	}
	// An accepted result is trusted only after we locally signed the matching
	// challenge for this exact identity and connection, and while the issued
	// expiry is sane.
	if ad.proofNonce == "" || !now.Before(ad.deadline) || !a.ExpiresAt.After(now) || a.ExpiresAt.After(now.Add(mxwire.AuthLifetime)) {
		ad.state = authRejected
		ad.proofNonce = ""
		ad.retryAt = now.Add(s.m.cfg.AuthRetryInterval)
		s.m.setStatus(s, a.Domain, "rejected", "authentication_failed", s.ready.SMTPHostname, time.Time{}, ad.domain.KeyID)
		return nil
	}
	// A renewal must not shorten a still-valid authorization window.
	if ad.state == authActive && now.Before(ad.expires) && a.ExpiresAt.Before(ad.expires) {
		ad.proofNonce = ""
		s.m.setStatus(s, a.Domain, "ready", "", s.ready.SMTPHostname, ad.expires, ad.domain.KeyID)
		return nil
	}
	ad.state = authActive
	ad.proofNonce = ""
	ad.expires = a.ExpiresAt
	s.sawAuth = true
	s.m.setStatus(s, a.Domain, "ready", "", s.ready.SMTPHostname, a.ExpiresAt, ad.domain.KeyID)
	return nil
}

func (s *session) retryDelay(reason string) time.Duration {
	base := s.m.cfg.AuthRetryInterval
	if reason == "dns_unavailable" || reason == "key_unavailable" || reason == "domain_limit" {
		if authRetryMax > base {
			return authRetryMax
		}
	}
	return base
}

func authReason(reason string) string {
	switch reason {
	case "dns_unavailable", "key_unavailable":
		return "key_unavailable"
	case "domain_limit":
		return "domain_limit"
	default:
		return "authentication_failed"
	}
}

func (s *session) notice(f mxwire.Frame, revoked bool) error {
	if f.TxID != 0 {
		return errors.New("invalid notice tx")
	}
	var n mxwire.DomainNotice
	if mxwire.DecodeFrame(f, &n) != nil {
		return errors.New("invalid domain notice")
	}
	ad := s.byChan[f.ChannelID]
	if ad == nil {
		return nil
	}
	if n.Domain != ad.domain.Name {
		return errors.New("invalid notice channel")
	}
	// A notice ends this binding regardless of state, so return any pacing slot
	// it still held.
	s.releaseAuth(ad)
	reason := n.Reason
	switch {
	case n.Reason == "replaced":
		// The receiver holds a newer binding from another connection. Park the
		// pin: no new resolve and no re-auth reclaim, but already-pinned
		// transactions may still ingest DATA until the old authorization
		// expires.
		ad.state = authReplaced
		ad.proofNonce = ""
	case revoked || n.Reason == "revoked" || n.Reason == "auth_expired":
		ad.state = authRevoked
		delete(s.byChan, f.ChannelID)
		reason = "revoked"
	default:
		ad.state = authRejected
		ad.proofNonce = ""
		ad.retryAt = time.Now().Add(authRetryMax)
		reason = "domain_unavailable"
	}
	if ad.state != authReplaced {
		s.cancelDomain(n.Domain)
	}
	s.m.setStatus(s, n.Domain, "unavailable", reason, "", ad.expires, ad.domain.KeyID)
	return nil
}

// resolve handles one recipient lookup. At most one resolve may be outstanding
// per transaction; different transactions may resolve concurrently.
func (s *session) resolve(f mxwire.Frame) error {
	if f.TxID == 0 || f.ChannelID == 0 {
		return errors.New("invalid resolve ids")
	}
	var q mxwire.V2Resolve
	if mxwire.DecodeFrame(f, &q) != nil {
		return errors.New("invalid resolve")
	}
	domain, err := mxwire.CanonicalDomain(q.Domain)
	if err != nil || domain != q.Domain {
		return errors.New("invalid resolve domain")
	}
	recipient := strings.ToLower(strings.TrimSpace(q.Recipient))
	parts := strings.Split(recipient, "@")
	if len(parts) != 2 || parts[1] != domain || len(parts[0]) == 0 {
		return errors.New("invalid recipient")
	}
	ad := s.byChan[f.ChannelID]
	if !s.authorized(ad, domain, f.ChannelID) {
		return s.writeJSON(mxwire.FrameResolveResult, f.TxID, f.ChannelID, mxwire.ResolveResponse{MachineCode: mxwire.CodeUnauthorized})
	}
	t, err := s.getTx(f.TxID)
	if err != nil || t.state != txResolving || t.resolveBusy || len(t.accepted) >= maxRecipientsPerTx {
		return s.writeJSON(mxwire.FrameResolveResult, f.TxID, f.ChannelID, tempFailResolve(domain, recipient))
	}
	t.resolveBusy = true
	jobs, connectionCtx := s.jobs, s.connectionCtx
	txctx := t.ctx
	s.connWG.Add(1)
	go func() {
		defer s.connWG.Done()
		ctx, cancel := context.WithTimeout(txctx, resolveBackendTimeout)
		defer cancel()
		var res mxwire.ResolveResponse
		func() {
			// A panic in the backend must not crash the core; report a
			// temporary failure so the sender retries.
			defer func() {
				if rec := recover(); rec != nil {
					slog.Error("mxdial backend panic recovered", "operation", "resolve", "type", fmt.Sprintf("%T", rec))
					res = mxwire.ResolveResponse{MachineCode: mxwire.CodeTempFail}
				}
			}()
			r, err := s.m.backend.Resolve(ctx, domain, []string{recipient})
			if err != nil {
				r = mxwire.ResolveResponse{MachineCode: mxwire.CodeTempFail}
			}
			res = r
		}()
		res = normalizeResolve(res, domain, recipient)
		select {
		case jobs <- jobResult{kind: jobResolve, txid: f.TxID, channel: f.ChannelID, domain: domain, recipient: recipient, resolve: res}:
		case <-connectionCtx.Done():
		}
	}()
	return nil
}

// authorized reports whether a channel carries live authority for the domain.
// Replaced bindings are not authorized for new work.
func (s *session) authorized(ad *auth, domain string, channel uint64) bool {
	if s.single() {
		return channel == 1
	}
	if ad == nil || ad.state != authActive {
		return false
	}
	return s.auths[domain] == ad && ad.channel == channel && time.Now().Before(ad.expires)
}

// pinnedAuthorized reports whether a transaction may still ingest against a
// domain. A replaced binding that is still authorized and unchanged may finish
// DATA for an already-pinned transaction.
func (s *session) pinnedAuthorized(domain string, pinned uint64) bool {
	if s.single() {
		return pinned == 1
	}
	cur, ok := s.auths[domain]
	if !ok || cur.channel != pinned {
		return false
	}
	return (cur.state == authActive || cur.state == authReplaced) && time.Now().Before(cur.expires)
}

func (s *session) single() bool {
	return s.m.cfg.ReceiverURL != "" && s.url == s.m.cfg.ReceiverURL
}

var errTxLimit = errors.New("transaction limit")

// getTx returns the transaction for id, creating it when necessary. Creation
// consumes one global slot; at the cap it returns errTxLimit and callers answer
// per-recipient instead of closing the connection.
func (s *session) getTx(id uint64) (*tx, error) {
	if t := s.txs[id]; t != nil {
		return t, nil
	}
	select {
	case s.m.slots <- struct{}{}:
	default:
		return nil, errTxLimit
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.m.cfg.TransactionTimeout)
	t := &tx{
		ctx:        ctx,
		cancel:     cancel,
		start:      time.Now(),
		domains:    map[string]uint64{},
		recipients: map[string]map[string]bool{},
		accepted:   map[string]bool{},
		state:      txResolving,
	}
	s.txs[id] = t
	return t, nil
}

// cleanup closes the staging file, removes it, and frees the global slot. It is
// called only after any in-flight backend ingest has finished, so the file is
// never deleted while a worker holds it.
func (t *tx) cleanup(s *session) {
	t.cancel()
	if t.file != nil {
		_ = t.file.Close()
		t.file = nil
	}
	if t.path != "" {
		_ = os.Remove(t.path)
		t.path = ""
	}
	select {
	case <-s.m.slots:
	default:
	}
}

// cancelTx cancels a transaction. If a backend ingest is in flight the
// transaction stays in the map until its completion runs cleanup, so the file
// is not deleted under the worker.
func (s *session) cancelTx(id uint64) {
	t := s.txs[id]
	if t == nil {
		return
	}
	t.state = txCancelled
	if t.busy || t.resolveBusy {
		t.cancel()
		return
	}
	delete(s.txs, id)
	t.cleanup(s)
}

func (s *session) ingestFrame(f mxwire.Frame) error {
	switch f.Type {
	case mxwire.FrameCancel:
		if f.ChannelID != 0 {
			return errors.New("invalid cancel channel")
		}
		s.cancelTx(f.TxID)
		return nil
	case mxwire.FrameIngestStart:
		return s.ingestStart(f)
	case mxwire.FrameIngestChunk:
		return s.ingestChunk(f)
	case mxwire.FrameIngestEnd:
		return s.ingestEnd(f)
	}
	return errors.New("unexpected transaction frame")
}

func (s *session) ingestStart(f mxwire.Frame) error {
	if f.ChannelID != 0 {
		return errors.New("invalid ingest start channel")
	}
	var start mxwire.V2IngestStart
	if mxwire.DecodeFrame(f, &start) != nil {
		return errors.New("invalid ingest start")
	}
	t := s.txs[f.TxID]
	if t == nil {
		return errors.New("unknown transaction")
	}
	if t.ctx.Err() != nil {
		return errors.New("transaction expired")
	}
	// Ingest begins only in the resolving phase, after every resolve answered.
	if t.state != txResolving || t.resolveBusy {
		return errors.New("invalid ingest start state")
	}
	if len(start.Domains) == 0 || len(start.Domains) > maxDomainsPerIngest || len(start.Metadata.Recipients) == 0 || len(start.Metadata.Recipients) > maxRecipientsPerTx {
		return errors.New("invalid ingest bounds")
	}
	if start.Metadata.Size <= 0 || start.Metadata.Size > s.m.cfg.MaxMessageBytes {
		return errors.New("invalid ingest size")
	}
	if _, err := hex.DecodeString(start.Metadata.ContentDigest); err != nil || len(start.Metadata.ContentDigest) != 64 {
		return errors.New("invalid ingest digest")
	}

	seen := map[string]bool{}
	for _, raw := range start.Domains {
		d, e := mxwire.CanonicalDomain(raw)
		pinned := t.domains[d]
		if e != nil || d != raw || seen[d] || pinned == 0 || len(t.recipients[d]) == 0 || !s.pinnedAuthorized(d, pinned) {
			return errors.New("unauthorized ingest domain")
		}
		seen[d] = true
	}
	if len(seen) != len(t.domains) {
		return errors.New("ingest domain mismatch")
	}

	recips := map[string]bool{}
	for _, r := range start.Metadata.Recipients {
		r = strings.ToLower(strings.TrimSpace(r))
		parts := strings.Split(r, "@")
		if len(parts) != 2 || !seen[parts[1]] || !t.recipients[parts[1]][r] || recips[r] {
			return errors.New("unresolved ingest recipient")
		}
		recips[r] = true
	}
	if len(recips) != len(t.accepted) {
		return fmt.Errorf("ingest recipient set mismatch: got %d, expected %d", len(recips), len(t.accepted))
	}
	for r := range t.accepted {
		if !recips[r] {
			return errors.New("incomplete ingest recipient set")
		}
	}
	if enc, err := encodedMetadataLen(start.Metadata); err != nil || enc > mxwire.MaxMetadataBytes {
		return errors.New("invalid ingest metadata")
	}

	root := filepath.Join(s.m.cfg.DataDir, "messages", ".tmp")
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(root, "mxdial-*")
	if err != nil {
		return err
	}
	_ = file.Chmod(0600)
	t.meta = start.Metadata
	t.file = file
	t.path = file.Name()
	t.hash = sha256.New()
	t.state = txStaging
	return nil
}

func encodedMetadataLen(m mxwire.IngestMetadata) (int, error) {
	// JSONFrame requires a non-zero transaction id; a dummy id is used because
	// only the encoded payload length is measured.
	f, err := mxwire.JSONFrame(mxwire.FrameIngestStart, 1, 0, mxwire.V2IngestStart{Domains: []string{"x.test"}, Metadata: m})
	if err != nil {
		return 0, err
	}
	return len(f.Payload), nil
}

func (s *session) ingestChunk(f mxwire.Frame) error {
	if f.ChannelID != 0 {
		return errors.New("invalid ingest chunk channel")
	}
	t := s.txs[f.TxID]
	if t == nil {
		return errors.New("unknown transaction")
	}
	if t.ctx.Err() != nil || t.state != txStaging || t.file == nil {
		return errors.New("invalid ingest chunk state")
	}
	seq, data, err := mxwire.DecodeChunk(f)
	if err != nil || seq != t.seq || t.size+int64(len(data)) > s.m.cfg.MaxMessageBytes {
		return errors.New("invalid ingest chunk")
	}
	if _, err = t.file.Write(data); err != nil {
		return err
	}
	_, _ = t.hash.Write(data)
	t.size += int64(len(data))
	t.seq++
	return nil
}

func (s *session) ingestEnd(f mxwire.Frame) error {
	if f.ChannelID != 0 {
		return errors.New("invalid ingest end channel")
	}
	t := s.txs[f.TxID]
	if t == nil {
		return errors.New("unknown transaction")
	}
	var end mxwire.V2IngestEnd
	if mxwire.DecodeFrame(f, &end) != nil {
		return errors.New("invalid ingest end")
	}
	if t.ctx.Err() != nil || t.state != txStaging || t.file == nil {
		return errors.New("invalid ingest end state")
	}
	// Re-check authority and local configuration at the end of the stream: a
	// domain may have been revoked or reconfigured while chunks were arriving.
	for d, pinned := range t.domains {
		if !s.pinnedAuthorized(d, pinned) {
			return errors.New("ingest authority lapsed")
		}
		if _, ok := s.domains[d]; !ok && !s.single() {
			return errors.New("ingest configuration changed")
		}
	}
	if end.Size != t.size || end.Size != t.meta.Size || end.ContentDigest != hex.EncodeToString(t.hash.Sum(nil)) || end.ContentDigest != t.meta.ContentDigest {
		return errors.New("ingest integrity mismatch")
	}
	if err := t.file.Sync(); err != nil {
		return err
	}
	_ = t.file.Close()
	t.file = nil
	t.state = txCommitting
	t.busy = true

	path, meta := t.path, t.meta
	receiverURL := s.url
	jobs, connectionCtx := s.jobs, s.connectionCtx
	txctx := t.ctx
	domains := make([]string, 0, len(t.domains))
	for d := range t.domains {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	s.connWG.Add(1)
	go func() {
		defer s.connWG.Done()
		ctx, cancel := context.WithTimeout(txctx, ingestBackendTimeout)
		defer cancel()
		var response mxwire.IngestResponse
		func() {
			// A panic in the backend must not crash the core; report a
			// temporary failure so the sender retries.
			defer func() {
				if rec := recover(); rec != nil {
					slog.Error("mxdial backend panic recovered", "operation", "ingest", "type", fmt.Sprintf("%T", rec))
					response = mxwire.IngestResponse{MachineCode: mxwire.CodeTempFail}
				}
			}()
			r, err := s.m.backend.Ingest(ctx, domains, meta, path, receiverURL)
			if err != nil {
				r = mxwire.IngestResponse{MachineCode: mxwire.CodeTempFail}
			}
			response = r
		}()
		response = normalizeIngest(response, meta.Recipients)
		select {
		case jobs <- jobResult{kind: jobIngest, txid: f.TxID, ingest: response}:
		case <-connectionCtx.Done():
		}
	}()
	return nil
}

// jobKind distinguishes backend completions.
type jobKind uint8

const (
	jobResolve jobKind = iota
	jobIngest
)

// jobResult is an immutable backend completion delivered to the loop.
type jobResult struct {
	kind      jobKind
	txid      uint64
	channel   uint64
	domain    string
	recipient string
	resolve   mxwire.ResolveResponse
	ingest    mxwire.IngestResponse
}

// completeJob applies a worker result on the loop goroutine. Pins are updated
// before any result is emitted, so a subsequent ingest can never observe a
// half-applied resolve, and resolveBusy is cleared before the result is sent.
func (s *session) completeJob(r jobResult) {
	t := s.txs[r.txid]
	if t == nil {
		return
	}
	if t.state == txCancelled || t.ctx.Err() != nil {
		delete(s.txs, r.txid)
		t.cleanup(s)
		return
	}
	switch r.kind {
	case jobResolve:
		t.resolveBusy = false
		if t.ctx.Err() != nil {
			return
		}
		if !s.authorized(s.byChan[r.channel], r.domain, r.channel) {
			r.resolve = tempFailResolve(r.domain, r.recipient)
		}
		accepted := false
		for _, rr := range r.resolve.Results {
			if strings.EqualFold(rr.Recipient, r.recipient) && rr.Accept {
				accepted = true
				break
			}
		}
		switch {
		case accepted && len(t.accepted) >= maxRecipientsPerTx:
			// At the recipient cap an otherwise-acceptable recipient is a
			// temporary failure for this transaction.
			r.resolve = tempFailResolve(r.domain, r.recipient)
		case accepted:
			if t.recipients[r.domain] == nil {
				t.recipients[r.domain] = map[string]bool{}
			}
			t.recipients[r.domain][r.recipient] = true
			t.accepted[r.recipient] = true
			if t.domains[r.domain] == 0 {
				t.domains[r.domain] = r.channel
			}
		}
		// A core decision of "unknown/denied recipient" (accept=false) is a
		// permanent result and is preserved verbatim; only a genuinely missing
		// or errored backend result was already normalized to a temporary
		// failure.
		_ = s.writeJSON(mxwire.FrameResolveResult, r.txid, r.channel, r.resolve)
	case jobIngest:
		t.busy = false
		canceled := t.ctx.Err() != nil
		delete(s.txs, r.txid)
		// Emit before cleanup cancels the transaction context. Results are only
		// sent after the backend has returned and its durable commit completed.
		if !canceled {
			_ = s.writeJSON(mxwire.FrameIngestResult, r.txid, 0, r.ingest)
		}
		t.cleanup(s)
	}
}

func tempFailResolve(domain, recipient string) mxwire.ResolveResponse {
	return mxwire.ResolveResponse{
		MachineCode: mxwire.CodeTempFail,
		Results:     []mxwire.ResolveRecipient{{Recipient: recipient, Domain: domain, Temporary: true, Code: string(mxwire.CodeTempFail)}},
	}
}

// normalizeResolve guarantees exactly one result for the requested recipient,
// scoped to this domain, so a misbehaving backend cannot smuggle extra
// recipients into a transaction. A missing result is a temporary failure; a
// core decision of "unknown recipient" (accept=false) is preserved as
// permanent and never upgraded to a temporary failure.
func normalizeResolve(res mxwire.ResolveResponse, domain, recipient string) mxwire.ResolveResponse {
	var out mxwire.ResolveResponse
	out.Version = mxwire.V2Protocol
	out.MachineCode = res.MachineCode
	for _, r := range res.Results {
		if !strings.EqualFold(strings.TrimSpace(r.Recipient), recipient) {
			continue
		}
		r.Recipient, r.Domain = recipient, domain
		out.Results = []mxwire.ResolveRecipient{r}
		return out
	}
	return tempFailResolve(domain, recipient)
}

// normalizeIngest ensures every accepted recipient has exactly one result and
// no spurious recipients leak through. The backend's per-recipient machine
// codes are preserved; only a genuinely missing result is filled with a
// temporary failure. No authorization-OK result is ever invented.
func normalizeIngest(res mxwire.IngestResponse, recipients []string) mxwire.IngestResponse {
	want := make([]string, 0, len(recipients))
	seen := map[string]bool{}
	for _, r := range recipients {
		r = strings.ToLower(strings.TrimSpace(r))
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		want = append(want, r)
	}
	sort.Strings(want)
	byRecipient := map[string]mxwire.RecipientIngestResult{}
	for _, r := range res.PerRecipient {
		k := strings.ToLower(strings.TrimSpace(r.Recipient))
		if _, ok := byRecipient[k]; !ok {
			byRecipient[k] = r
		}
	}
	out := mxwire.IngestResponse{Version: mxwire.V2Protocol, MachineCode: res.MachineCode, MessageID: res.MessageID}
	for _, r := range want {
		rr, ok := byRecipient[r]
		if !ok {
			rr = mxwire.RecipientIngestResult{Recipient: r, MachineCode: mxwire.CodeTempFail, Reason: "no_result"}
		}
		rr.Recipient = r
		out.PerRecipient = append(out.PerRecipient, rr)
	}
	if out.MachineCode == "" {
		out.MachineCode = mxwire.CodeOK
	}
	return out
}

// cancelDomain cancels every transaction pinned to a domain.
func (s *session) cancelDomain(domain string) {
	for id, t := range s.txs {
		_, ok := t.domains[domain]
		_, accepted := t.recipients[domain]
		if ok || accepted {
			s.cancelTx(id)
		}
	}
}

// write serializes all frame writes for the current physical connection and
// captures the writer generation, so a write queued across a reconnect cannot
// touch the next connection.
func (s *session) write(f mxwire.Frame) error {
	s.outMu.Lock()
	defer s.outMu.Unlock()
	s.lifeMu.Lock()
	w, ctxErr := s.writer, s.ctx.Err()
	s.lifeMu.Unlock()
	if ctxErr != nil {
		return ctxErr
	}
	if w == nil {
		return io.ErrClosedPipe
	}
	stop := time.AfterFunc(writeTimeout, func() { _ = w.CloseWithError(context.DeadlineExceeded) })
	defer stop.Stop()
	return mxwire.WriteFrame(w, f)
}

func (s *session) writeJSON(t mxwire.FrameType, tx, ch uint64, v any) error {
	f, err := mxwire.JSONFrame(t, tx, ch, v)
	if err != nil {
		return err
	}
	return s.write(f)
}
