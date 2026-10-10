package imap

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// RootScope is the resolved scope of a standalone inbox: the single selected
// root (an explicit IMAP folder path) and the hierarchy delimiter the server
// uses, together with the INBOX boundary.
//
// Two kinds of root are supported:
//
//   - The personal root (default). The scope is the whole personal namespace:
//     INBOX and its siblings (Sent, Drafts, Trash, custom folders) and their
//     children. A normal personal account's Sent and Drafts are therefore in
//     scope; the adapter never requires every folder to live under an "INBOX"
//     prefix.
//   - An arbitrary root (for example "Archive" or "Work/Projects"). The scope is
//     exactly that folder or a delimiter child of it ("Archive/2026"), never a
//     loose string prefix ("Archived" is not in an "Archive" scope) and never a
//     different shared namespace.
//
// Root is the literal IMAP path; Delimiter is the server's hierarchy delimiter
// (0 when the server advertised none); Personal reports whether this is the
// personal namespace; INBOXInScope reports whether INBOX itself is inside the
// scope (always true for a personal root, and true for an arbitrary root only
// when that root is INBOX or a descendant of it).
type RootScope struct {
	Root         string
	Delimiter    rune
	Personal     bool
	INBOXInScope bool
	// ExcludePrefixes are namespace prefixes that are never in scope: the
	// server's "Other Users" and "Shared" NAMESPACE namespaces. They are excluded
	// even when the personal namespace prefix is empty (INBOX at the top level),
	// so a single personal mailbox never mirrors or reaches a shared/upstream
	// namespace.
	ExcludePrefixes []string
}

// InScope reports whether a folder path belongs to this scope. The empty path
// (the server's root hierarchy node) is never in scope.
//
// Shared/other namespaces (ExcludePrefixes) are always rejected first, whatever
// the personal prefix. A personal root then covers the personal namespace: when
// the namespace prefix is empty (INBOX at the top level) that is every listed
// folder except the excluded namespaces; when the prefix is non-empty, only that
// prefix and its children. An arbitrary (non-personal) root is stricter:
// membership is the root itself or a delimiter-child of it, never a loose prefix
// and never a different shared namespace.
func (s RootScope) InScope(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	for _, ex := range s.ExcludePrefixes {
		if namespaceCoveredBy(path, strings.TrimSpace(ex), s.Delimiter) {
			return false
		}
	}
	if s.Personal {
		// A non-empty personal prefix bounds the namespace; an empty prefix is a
		// top-level personal namespace (shared namespaces already excluded above).
		prefix := strings.TrimSpace(s.Root)
		if prefix == "" {
			return true
		}
		if strings.EqualFold(path, prefix) {
			return true
		}
		if s.Delimiter != 0 {
			delim := string(s.Delimiter)
			// A namespace prefix that already ends with the delimiter (for
			// example "INBOX.") matches every path under it.
			if strings.HasSuffix(prefix, delim) {
				if len(path) > len(prefix) && strings.EqualFold(path[:len(prefix)], prefix) {
					return true
				}
			} else if hasDelimiterPrefix(path, prefix, s.Delimiter) {
				return true
			}
			// Accept the prefix with its trailing delimiter trimmed.
			if trimmed := strings.TrimRight(prefix, delim); trimmed != "" && strings.EqualFold(path, trimmed) {
				return true
			}
		}
		return false
	}
	root := strings.TrimSpace(s.Root)
	if root == "" {
		return false
	}
	if strings.EqualFold(path, root) {
		return true
	}
	if s.Delimiter == 0 {
		return false
	}
	return hasDelimiterPrefix(path, root, s.Delimiter)
}

// hasDelimiterPrefix reports whether path is a strict delimiter child of root.
func hasDelimiterPrefix(path, root string, delim rune) bool {
	if len(path) <= len(root) {
		return false
	}
	if !strings.EqualFold(path[:len(root)], root) {
		return false
	}
	return rune(path[len(root)]) == delim
}

// namespaceCoveredBy reports whether path is the namespace prefix or lies under
// it. It handles a prefix that is already terminated by its delimiter (for
// example a server NAMESPACE descriptor reporting "Shared/" or "Other Users.")
// which a plain delimiter-child check would miss, and it tolerates either the
// session delimiter or the conventional '/' and '.' separators so a mismatch
// between the personal and excluded namespace delimiters still isolates them.
func namespaceCoveredBy(path, prefix string, delim rune) bool {
	if prefix == "" {
		return false
	}
	if strings.EqualFold(path, prefix) {
		return true
	}
	// A delimiter-terminated prefix also covers the namespace root without its
	// trailing delimiter ("Shared/" covers "Shared").
	if trimmed := strings.TrimRight(prefix, "/."); trimmed != "" && strings.EqualFold(path, trimmed) {
		return true
	}
	for _, d := range namespaceDelimiters(delim) {
		if strings.HasSuffix(prefix, string(d)) {
			if len(path) > len(prefix) && strings.EqualFold(path[:len(prefix)], prefix) {
				return true
			}
			continue
		}
		if hasDelimiterPrefix(path, prefix, d) {
			return true
		}
	}
	return false
}

// namespaceDelimiters returns the delimiter to test plus the conventional
// '/' and '.', de-duplicated.
func namespaceDelimiters(primary rune) []rune {
	out := make([]rune, 0, 3)
	if primary != 0 {
		out = append(out, primary)
	}
	for _, d := range []rune{'/', '.'} {
		if d != primary {
			out = append(out, d)
		}
	}
	return out
}

// Capabilities is the adapter's view of what the connected server supports. It
// is derived from the IMAP CAPABILITY set at handshake time; callers must not
// assume any of these are true before a session is established.
type Capabilities struct {
	Namespace    bool
	Move         bool
	UIDPlus      bool
	Idle         bool
	ESearch      bool
	Search       bool
	ListExtended bool
	ListStatus   bool
	SpecialUse   bool
	Unselect     bool
	LiteralMinus bool
	Raw          imap.CapSet
}

// Adapter is a live IMAP session for one standalone inbox. It is safe for
// concurrent use only through its methods that serialise internally; the IMAP
// client itself supports concurrent commands but this adapter serialises
// folder selection to keep UIDVALIDITY checks race-free.
//
// An Adapter is created by Dial (which owns the connection) or adopted with
// NewFromClient (which takes ownership of an existing connection). It is never
// shared between goroutines without external synchronisation.
type Adapter struct {
	cfg  Config
	conn *imapclient.Client

	scope RootScope

	sink *notifySink

	mu                sync.Mutex
	selected          string // currently selected folder, "" when none
	readOnlySelection *imap.SelectData
	stopCancellation  func() bool
	caps              Capabilities
}

// NewFromClient adopts an existing, authenticated IMAP client. It performs the
// initial capability discovery but does not select a folder. The caller
// transfers ownership of conn to the Adapter, which closes it with Close.
//
// It is primarily useful for tests that install a fake server; production code
// should use Dial.
func NewFromClient(cfg Config, conn *imapclient.Client) *Adapter {
	a := &Adapter{cfg: cfg.Normalize(), conn: conn}
	a.caps = a.readCapabilities()
	return a
}

// Dial connects to the configured server and authenticates. The returned
// Adapter owns the connection and must be closed with Close.
func Dial(ctx context.Context, cfg Config) (*Adapter, error) {
	sink := newNotifySink(32)
	conn, err := connect(ctx, cfg, sink)
	if err != nil {
		return nil, err
	}
	a := &Adapter{cfg: cfg.Normalize(), conn: conn, sink: sink}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	a.stopCancellation = stop
	a.caps = a.readCapabilities()
	if err := ctx.Err(); err != nil {
		stop()
		_ = conn.Close()
		return nil, wrapErr(err)
	}
	return a, nil
}

// Close logs out and closes the connection. It is safe to call more than once.
func (a *Adapter) Close() error {
	a.mu.Lock()
	conn := a.conn
	a.conn = nil
	a.selected = ""
	if a.stopCancellation != nil {
		a.stopCancellation()
	}
	a.mu.Unlock()
	if conn == nil {
		return nil
	}
	deadline := time.AfterFunc(2*time.Second, func() { _ = conn.Close() })
	defer deadline.Stop()
	_ = conn.Logout().Wait()
	return conn.Close()
}

// Capabilities returns the negotiated server capabilities.
func (a *Adapter) Capabilities() Capabilities { return a.caps }

// Scope returns the resolved root scope. It is only meaningful after Discover.
func (a *Adapter) Scope() RootScope { return a.scope }

// Discover resolves the inbox's root scope. It discovers the server namespace
// (when NAMESPACE is available), chooses the personal namespace or the explicit
// root, and computes the delimiter and INBOX boundary. When explicitRoot is
// empty the default personal root is used.
//
// Discover must be called before folder listing and message operations.
func (a *Adapter) Discover(ctx context.Context, explicitRoot string) (RootScope, error) {
	scope, err := a.resolveScope(ctx, explicitRoot)
	if err != nil {
		return RootScope{}, err
	}
	a.scope = scope
	return scope, nil
}

func (a *Adapter) readCapabilities() Capabilities {
	caps := a.conn.Caps()
	out := Capabilities{Raw: caps, Search: true}
	if caps == nil {
		return out
	}
	out.Namespace = caps.Has(imap.CapNamespace)
	out.Move = caps.Has(imap.CapMove)
	out.UIDPlus = caps.Has(imap.CapUIDPlus)
	out.Idle = caps.Has(imap.CapIdle)
	out.ESearch = caps.Has(imap.CapESearch)
	out.ListExtended = caps.Has(imap.CapListExtended)
	out.ListStatus = caps.Has(imap.CapListStatus)
	out.SpecialUse = caps.Has(imap.CapSpecialUse)
	out.Unselect = caps.Has(imap.CapUnselect)
	out.LiteralMinus = caps.Has(imap.CapLiteralMinus)
	return out
}

// ModelCapabilities projects the negotiated IMAP capabilities onto the shared
// model surface so workflow code can branch on capability, not transport.
func (a *Adapter) ModelCapabilities() model.Capabilities {
	caps := model.StandaloneCapabilities()
	caps.Realtime = a.caps.Idle
	caps.Move = true // COPY-based fallback is always available
	caps.Search = true
	caps.HierarchicalFolders = true
	return caps
}

// selectedFolder returns the currently selected folder, or "" when none.
func (a *Adapter) selectedFolder() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.selected
}
