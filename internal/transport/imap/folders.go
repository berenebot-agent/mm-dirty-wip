package imap

import (
	"context"
	"sort"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
	"github.com/emersion/go-imap/v2"
)

// resolveScope determines the root scope for the session. When explicitRoot is
// non-empty it is used verbatim (an arbitrary root). Otherwise the personal
// namespace is discovered via NAMESPACE and its prefix is used.
//
// The server's "Other Users" and "Shared" namespaces are always recorded as
// exclusions, even when the personal prefix is empty, so a single personal
// mailbox never reaches a shared/upstream namespace. When NAMESPACE is
// unsupported there is no discoverable personal boundary, so the scope is
// deliberately conservative: only the explicit selected root (default INBOX)
// and its children, never the whole login.
func (a *Adapter) resolveScope(ctx context.Context, explicitRoot string) (RootScope, error) {
	explicitRoot = strings.TrimSpace(explicitRoot)

	var delim rune
	personalPrefix := ""
	personalKnown := false
	var excludes []string
	if a.caps.Namespace {
		ns, err := a.conn.Namespace().Wait()
		if err == nil && ns != nil {
			for _, d := range ns.Other {
				if p := strings.TrimSpace(d.Prefix); p != "" {
					excludes = append(excludes, p)
				}
			}
			for _, d := range ns.Shared {
				if p := strings.TrimSpace(d.Prefix); p != "" {
					excludes = append(excludes, p)
				}
			}
			if len(ns.Personal) > 0 {
				personalKnown = true
				personalPrefix = strings.TrimSpace(ns.Personal[0].Prefix)
				delim = ns.Personal[0].Delim
			}
		}
	}

	// A personal scope is used only when NAMESPACE actually described a personal
	// namespace and the requested root is the default (empty) or INBOX. Any
	// explicit custom root is a strict root.
	wantPersonal := personalKnown && (explicitRoot == "" || strings.EqualFold(explicitRoot, model.NamespaceINBOX))
	if wantPersonal {
		return RootScope{
			Root:            personalPrefix,
			Delimiter:       delim,
			Personal:        true,
			INBOXInScope:    true,
			ExcludePrefixes: excludes,
		}, nil
	}

	if explicitRoot == "" {
		// NAMESPACE unsupported (or no personal descriptor): be conservative.
		explicitRoot = model.NamespaceINBOX
	}

	// Without a discovered delimiter fall back to the conventional '/' so
	// children still resolve; DiscoverFolders will reconcile the real one.
	if delim == 0 {
		delim = a.discoverDelimiter()
	}
	if delim == 0 {
		delim = '/'
	}
	inboxInScope := strings.EqualFold(explicitRoot, model.NamespaceINBOX) || isDelimiterDescendant(explicitRoot, model.NamespaceINBOX, delim)
	return RootScope{
		Root:            explicitRoot,
		Delimiter:       delim,
		Personal:        false,
		INBOXInScope:    inboxInScope,
		ExcludePrefixes: excludes,
	}, nil
}

// isDelimiterDescendant reports whether path is the root or a delimiter child of
// it (case-insensitive).
func isDelimiterDescendant(path, root string, delim rune) bool {
	if strings.EqualFold(path, root) {
		return true
	}
	if delim == 0 {
		return false
	}
	return hasDelimiterPrefix(path, root, delim)
}

// discoverDelimiter lists the root hierarchy to learn the server's delimiter.
func (a *Adapter) discoverDelimiter() rune {
	cmd := a.conn.List("", "*", nil)
	var delim rune
	for {
		data := cmd.Next()
		if data == nil {
			break
		}
		if data.Delim != 0 {
			delim = data.Delim
			break
		}
	}
	_ = cmd.Close()
	return delim
}

// DiscoverFolders lists the folders in the inbox's root scope with their
// special-use roles and selectability. It returns the folders in hierarchical
// order and the resolved scope. It performs a live LIST; it never reads a cache.
//
// The scope is resolved first (explicit root or personal namespace), then every
// listed mailbox is filtered to that scope: a personal root keeps INBOX and its
// siblings/children; an arbitrary root keeps only the root and its
// delimiter-children.
func (a *Adapter) DiscoverFolders(ctx context.Context, explicitRoot string) ([]RemoteFolder, RootScope, error) {
	scope, err := a.Discover(ctx, explicitRoot)
	if err != nil {
		return nil, RootScope{}, wrapErr(err)
	}

	options := &imap.ListOptions{}
	if a.caps.SpecialUse {
		options.ReturnSpecialUse = true
	}
	if a.caps.ListExtended {
		options.ReturnChildren = true
	}

	cmd := a.conn.List("", "*", options)
	var listed []*imap.ListData
	for {
		data := cmd.Next()
		if data == nil {
			break
		}
		listed = append(listed, data)
	}
	if err := cmd.Close(); err != nil {
		return nil, scope, wrapErr(err)
	}

	folders := make([]RemoteFolder, 0, len(listed))
	for _, data := range listed {
		path := data.Mailbox
		if path == "" {
			continue
		}
		if !scope.InScope(path) {
			continue
		}
		folders = append(folders, remoteFolderFromList(data))
	}
	sortFolders(folders, scope.Delimiter)
	return folders, scope, nil
}

// RemoteFolder is a folder discovered on the remote server, independent of any
// local representation. Path is the full IMAP folder path; Name is the final
// path segment; Role is a model.FolderRole* value inferred from the server's
// special-use attribute (falling back to a name heuristic).
type RemoteFolder struct {
	Path        string
	Name        string
	Delimiter   rune
	Role        string
	Selectable  bool
	HasChildren bool
	Subscribed  bool
	SpecialUse  []string
}

// remoteFolderFromList maps one LIST response into a RemoteFolder.
func remoteFolderFromList(data *imap.ListData) RemoteFolder {
	f := RemoteFolder{
		Path:      data.Mailbox,
		Delimiter: data.Delim,
	}
	f.Name = lastPathSegment(f.Path, f.Delimiter)
	f.Selectable = true
	special := make([]string, 0, len(data.Attrs))
	for _, attr := range data.Attrs {
		switch attr {
		case imap.MailboxAttrNoSelect, imap.MailboxAttrNonExistent:
			f.Selectable = false
		case imap.MailboxAttrHasChildren:
			f.HasChildren = true
		case imap.MailboxAttrSubscribed:
			f.Subscribed = true
		case imap.MailboxAttrAll:
			f.Role = model.FolderRoleArchive
			special = append(special, string(attr))
		case imap.MailboxAttrArchive:
			f.Role = model.FolderRoleArchive
			special = append(special, string(attr))
		case imap.MailboxAttrDrafts:
			f.Role = model.FolderRoleDrafts
			special = append(special, string(attr))
		case imap.MailboxAttrSent:
			f.Role = model.FolderRoleSent
			special = append(special, string(attr))
		case imap.MailboxAttrTrash:
			f.Role = model.FolderRoleTrash
			special = append(special, string(attr))
		case imap.MailboxAttrJunk:
			f.Role = model.FolderRoleSpam
			special = append(special, string(attr))
		case imap.MailboxAttrFlagged, imap.MailboxAttrImportant:
			// Not a role we model distinctly; keep as a folder.
		}
	}
	f.SpecialUse = special
	if f.Role == "" {
		f.Role = roleForName(f.Name)
	}
	return f
}

// roleForName is a local, conservative name heuristic used only when the server
// advertises no special-use attribute. It mirrors store.FolderRoleForName but is
// kept inside the adapter so the adapter never imports the store.
func roleForName(name string) string {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "INBOX":
		return model.FolderRoleInbox
	case "SENT", "SENT ITEMS", "SENT MESSAGES", "SENT E-MAIL":
		return model.FolderRoleSent
	case "DRAFTS":
		return model.FolderRoleDrafts
	case "TRASH", "DELETED", "DELETED ITEMS", "DELETED MESSAGES", "BIN":
		return model.FolderRoleTrash
	case "JUNK", "SPAM", "JUNK E-MAIL", "BULK MAIL":
		return model.FolderRoleSpam
	case "ARCHIVE", "ALL MAIL", "ALL":
		return model.FolderRoleArchive
	case "OUTBOX":
		return model.FolderRoleOutbox
	default:
		return model.FolderRoleFolder
	}
}

// ToModelFolder converts a discovered remote folder into the shared model type,
// scoped to an inbox. Counts are zero: they are populated from a separate
// Status/SELECT pass or a local cache, not by the LIST itself.
func (f RemoteFolder) ToModelFolder(inboxID string) model.Folder {
	return model.Folder{
		InboxID:    inboxID,
		Path:       f.Path,
		Name:       f.Name,
		ParentPath: parentPath(f.Path, f.Delimiter),
		Role:       f.Role,
		Selectable: f.Selectable,
	}
}

func lastPathSegment(path string, delim rune) string {
	if delim == 0 {
		return path
	}
	if idx := strings.LastIndexByte(path, byte(delim)); idx >= 0 && idx+1 < len(path) {
		return path[idx+1:]
	}
	return path
}

func parentPath(path string, delim rune) string {
	if delim == 0 {
		return ""
	}
	if idx := strings.LastIndexByte(path, byte(delim)); idx > 0 {
		return path[:idx]
	}
	return ""
}

func sortFolders(folders []RemoteFolder, delim rune) {
	sort.SliceStable(folders, func(i, j int) bool {
		return folders[i].Path < folders[j].Path
	})
}

// EnsureFolderExists reports whether path exists in the current scope by
// selecting it (EXAMINE, read-only) and immediately unselecting. It returns the
// selected UIDVALIDITY so the caller can re-scope a locator.
func (a *Adapter) EnsureFolderExists(ctx context.Context, path string) (uint32, error) {
	var uidValidity uint32
	err := a.withExamine(ctx, path, func(data *imap.SelectData) error {
		uidValidity = data.UIDValidity
		return nil
	})
	return uidValidity, err
}

// withExamine holds the adapter lock, selects path read-only, runs fn with the
// SELECT data and unselects. Holding the lock for the whole operation keeps the
// selection race-free: no other adapter operation can change the selected folder
// between the UIDVALIDITY check and the command that depends on it.
func (a *Adapter) withExamine(ctx context.Context, path string, fn func(*imap.SelectData) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn == nil {
		return wrapErr(ErrNotConnected)
	}
	data, err := a.conn.Select(path, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		a.selected = ""
		return wrapErr(err)
	}
	a.selected = ""
	err = fn(data)
	a.selected = ""
	return err
}

// withSelectRW is withExamine for a read-write selection used by mutation
// commands.
func (a *Adapter) withSelectRW(ctx context.Context, path string, fn func(*imap.SelectData) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn == nil {
		return wrapErr(ErrNotConnected)
	}
	data, err := a.conn.Select(path, nil).Wait()
	if err != nil {
		a.selected = ""
		return wrapErr(err)
	}
	a.selected = path
	defer func() { a.selected = "" }()
	return fn(data)
}

// examine selects a folder read-only and returns its SELECT data. It is used for
// the few operations that need the data outside a withExamine closure (for
// example a one-off UIDVALIDITY read); callers must not assume the folder stays
// selected.
func (a *Adapter) examine(ctx context.Context, path string) (*imap.SelectData, error) {
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
	a.selected = ""
	return data, nil
}

func (a *Adapter) clearSelected() {
	a.mu.Lock()
	a.selected = ""
	a.mu.Unlock()
}
