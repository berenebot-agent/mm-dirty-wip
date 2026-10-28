package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/dellarb/mammothmail/internal/idgen"
	"github.com/dellarb/mammothmail/internal/model"
	"github.com/dellarb/mammothmail/internal/store"
	"github.com/dellarb/mammothmail/internal/transport/gmail"
	"github.com/dellarb/mammothmail/internal/transport/one"
)

type oneCachedClient struct {
	revision int64
	client   *one.Client
}
type OneMailboxChoice struct {
	ID            string `json:"id"`
	Email         string `json:"email,omitempty"`
	Provider      string `json:"provider"`
	OneStatus     string `json:"one_status"`
	Environment   string `json:"environment"`
	InboxID       string `json:"inbox_id,omitempty"`
	Status        string `json:"status"`
	Available     bool   `json:"available"`
	MessagesTotal int    `json:"messages_total,omitempty"`
}

// One history windows offered on the import screen. Everything is the default;
// the shorter windows exist because a full metadata build spends roughly one
// provider request per message against One's account-wide quota.
type OneHistoryWindow struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Days  int    `json:"days"`
}

var oneHistoryWindows = []OneHistoryWindow{
	{Key: "all", Label: "Everything", Days: 0},
	{Key: "365d", Label: "Last year", Days: 365},
	{Key: "90d", Label: "Last 90 days", Days: 90},
	{Key: "30d", Label: "Last 30 days", Days: 30},
}

// OneHistoryWindowOptions returns the import-screen history windows in display
// order, with Everything first.
func OneHistoryWindowOptions() []OneHistoryWindow {
	out := make([]OneHistoryWindow, len(oneHistoryWindows))
	copy(out, oneHistoryWindows)
	return out
}

// oneBackfillAfter maps a window key to a Gmail "after:" boundary. The empty
// string means no boundary (index everything). Unknown keys default to
// unbounded so a bad form value can never silently narrow a mailbox.
func oneBackfillAfter(key string, now time.Time) string {
	for _, w := range oneHistoryWindows {
		if w.Key == key && w.Days > 0 {
			return now.UTC().AddDate(0, 0, -w.Days).Format("2006/01/02")
		}
	}
	return ""
}

type OneImportResult struct {
	ConnectionID string `json:"connection_id"`
	InboxID      string `json:"inbox_id,omitempty"`
	Error        string `json:"error,omitempty"`
}

func oneBindingAAD(account, inbox string) string { return "one_binding:" + account + ":" + inbox }
func oneKeyAAD(account, inbox string) string     { return "one_key:" + account + ":" + inbox }
func oneSetupAAD(account, id string) string      { return "one_import:" + account + ":" + id }
func oneError(kind, message string) error        { return model.NewMailboxError(kind, message, false, nil) }
func oneFingerprint(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}
func onePermission(p model.Principal) error {
	if !p.Admin || p.UserID == "" {
		return oneError(model.ErrKindForbidden, "A signed-in account administrator must manage One credentials")
	}
	return nil
}

func (m *RemoteMailboxService) oneLimiter(id string) *one.Limiter {
	m.oneMu.Lock()
	defer m.oneMu.Unlock()
	if m.oneLimiters == nil {
		m.oneLimiters = make(map[string]*one.Limiter)
		m.oneClients = make(map[string]oneCachedClient)
	}
	if m.oneLimiters[id] == nil {
		m.oneLimiters[id] = &one.Limiter{Interval: m.OneInterval}
	}
	return m.oneLimiters[id]
}

// Bind equal keys to the same limiter; retain a mailbox's quota group during
// rotation so changing its key cannot bypass a cooldown shared with siblings.
func (m *RemoteMailboxService) oneKeyLimiter(ctx context.Context, account, fingerprint, group string) (*one.Limiter, error) {
	alias := "key:" + account + ":" + fingerprint
	m.oneMu.Lock()
	if existing := m.oneLimiters[alias]; existing != nil {
		if group != "" {
			m.oneLimiters[group] = existing
		}
		m.oneMu.Unlock()
		return existing, nil
	}
	m.oneMu.Unlock()
	bindings, err := m.Service.Store.ListOneBindings(ctx, account)
	if err != nil {
		return nil, err
	}
	for _, b := range bindings {
		if group == "" && b.Fingerprint == fingerprint {
			group = b.RateGroup
			break
		}
	}
	if group == "" {
		group = alias
	}
	l := m.oneLimiter(group)
	m.oneMu.Lock()
	if existing := m.oneLimiters[alias]; existing != nil {
		l = existing
	}
	m.oneLimiters[alias] = l
	m.oneLimiters[group] = l
	m.oneMu.Unlock()
	return l, nil
}

func (m *RemoteMailboxService) validatedOne(ctx context.Context, p model.Principal, key, group string) (*one.Client, []one.Connection, error) {
	if err := onePermission(p); err != nil {
		return nil, nil, err
	}
	if len(key) < 16 || len(key) > 4096 || strings.ContainsAny(key, " \t\r\n") {
		return nil, nil, oneError(model.ErrKindInvalid, "Enter a standard One REST API key")
	}
	l, err := m.oneKeyLimiter(ctx, p.AccountID, oneFingerprint(key), group)
	if err != nil {
		return nil, nil, err
	}
	c := one.NewClient(key, m.OneHTTP, l)
	rows, err := c.Connections(ctx)
	if err == nil {
		err = c.ValidateActions(ctx)
	}
	return c, rows, err
}

// Save stages an encrypted, user-scoped key for 30 minutes, not a global
// configuration. Import fans it out into independently encrypted inbox rows.
func (m *RemoteMailboxService) PrepareOneImport(ctx context.Context, p model.Principal, key string) (string, error) {
	if _, _, err := m.validatedOne(ctx, p, key, ""); err != nil {
		return "", err
	}
	id := idgen.New("oneimport")
	enc, err := m.Service.EncryptSecretAAD(oneSetupAAD(p.AccountID, id), []byte(key))
	if err != nil {
		return "", err
	}
	err = m.Service.Store.SaveOneImportSetup(ctx, store.OneImportSetup{ID: id, AccountID: p.AccountID, UserID: p.UserID, EncryptedKey: enc, Fingerprint: oneFingerprint(key)})
	return id, err
}

func (m *RemoteMailboxService) oneSetup(ctx context.Context, p model.Principal, id string) (store.OneImportSetup, *one.Client, string, error) {
	if err := onePermission(p); err != nil {
		return store.OneImportSetup{}, nil, "", err
	}
	s, err := m.Service.Store.OneImportSetup(ctx, p.AccountID, p.UserID, id)
	if err != nil {
		return s, nil, "", err
	}
	b, err := m.Service.DecryptSecretAAD(oneSetupAAD(p.AccountID, id), s.EncryptedKey)
	if err != nil {
		return s, nil, "", oneError(model.ErrKindAuth, "One setup credentials unavailable; enter the key again")
	}
	key := string(b)
	clear(b)
	l, err := m.oneKeyLimiter(ctx, p.AccountID, s.Fingerprint, "")
	if err != nil {
		return s, nil, "", err
	}
	return s, one.NewClient(key, m.OneHTTP, l), key, nil
}

func findOne(rows []one.Connection, id string) *one.Connection {
	for i := range rows {
		if rows[i].ID == id && rows[i].Usable() {
			return &rows[i]
		}
	}
	return nil
}

func (m *RemoteMailboxService) DiscoverOne(ctx context.Context, p model.Principal, setupID string) ([]OneMailboxChoice, error) {
	_, c, _, err := m.oneSetup(ctx, p, setupID)
	if err != nil {
		return nil, err
	}
	connections, err := c.Connections(ctx)
	if err != nil {
		return nil, err
	}
	if err = c.ValidateActions(ctx); err != nil {
		return nil, err
	}
	bindings, err := m.Service.Store.ListOneBindings(ctx, p.AccountID)
	if err != nil {
		return nil, err
	}
	boxes, err := m.Service.Store.ListInboxes(ctx, p)
	if err != nil {
		return nil, err
	}
	// An address already configured on any inbox of the account — including one
	// configured as an alias — cannot be imported again. There is no migration
	// path: the operator deletes the existing inbox and imports the address
	// afresh, so the row states the exact remedy instead of a dead end. The check
	// mirrors ImportOneInbox's own collision rule so a row can never promise an
	// import the store would refuse.
	taken := make(map[string]bool, len(boxes))
	for _, box := range boxes {
		taken[strings.ToLower(box.Address)] = true
	}
	aliases, err := m.Service.Store.ListInboxAliases(ctx, p.AccountID)
	if err != nil {
		return nil, err
	}
	for _, list := range aliases {
		for _, a := range list {
			taken[strings.ToLower(a.Address)] = true
		}
	}
	out := []OneMailboxChoice{}
	for _, remote := range connections {
		row := OneMailboxChoice{ID: remote.ID, Provider: remote.Platform, OneStatus: remote.State, Environment: remote.Environment, Status: "Not supported yet"}
		if remote.Platform == "gmail" {
			row.Status = "Inactive or unusable; reconnect in One"
			if remote.Usable() {
				profile, e := c.Gmail(remote.Key, nil, nil).Profile(ctx, "")
				if e == nil {
					row.Email, e = store.ValidateStandaloneAddress(profile.EmailAddress)
				}
				if e == nil {
					row.Status, row.Available = "Not imported", true
					// MessagesTotal is an estimate of the full-history import
					// cost; it is deliberately best-effort and never blocks import.
					row.MessagesTotal = profile.MessagesTotal
				} else {
					row.Status = "Cannot verify Gmail identity/permissions; reconnect in One"
				}
			}
		}
		for _, b := range bindings {
			if b.ConnectionID == row.ID {
				row.InboxID, row.Email, row.Available = b.InboxID, b.GmailEmail, false
				// Already imported: the row is informational only. The key for this
				// inbox is changed on the inbox itself, never from the import screen.
				row.Status = "Already imported"
			}
		}
		if row.Available && taken[strings.ToLower(row.Email)] {
			row.Status, row.Available = "Address already configured — delete the existing inbox before importing", false
		}
		out = append(out, row)
	}
	for i := range out {
		for j := range out {
			if i != j && out[i].Email != "" && strings.EqualFold(out[i].Email, out[j].Email) && out[i].Available {
				out[i].Status = "Duplicate Gmail identity; select only one connection"
			}
		}
	}
	return out, nil
}

// ImportOne imports the selected Gmail connections. windows maps a connection
// ID to a history-window key from oneHistoryWindows; a missing or unknown key
// indexes the full history. Each imported inbox stores its own horizon so the
// progressive backfill stops at the chosen boundary.
func (m *RemoteMailboxService) ImportOne(ctx context.Context, p model.Principal, setupID string, ids []string, windows map[string]string) ([]OneImportResult, error) {
	ctx = store.WithInboxLogOperation(ctx, "one_import")
	ctx = store.WithInboxLogActor(ctx, "", p)
	s, c, key, err := m.oneSetup(ctx, p, setupID)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 || len(ids) > 100 {
		return nil, oneError(model.ErrKindInvalid, "Select between 1 and 100 Gmail connections")
	}
	rows, err := c.Connections(ctx)
	if err != nil {
		return nil, err
	}
	if err = c.ValidateActions(ctx); err != nil {
		return nil, err
	}
	group := "key:" + p.AccountID + ":" + s.Fingerprint
	bindings, err := m.Service.Store.ListOneBindings(ctx, p.AccountID)
	if err != nil {
		return nil, err
	}
	for _, b := range bindings {
		if b.Fingerprint == s.Fingerprint {
			group = b.RateGroup
			break
		}
	}
	out := []OneImportResult{}
	seen := make(map[string]bool)
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		result := OneImportResult{ConnectionID: id}
		remote := findOne(rows, id)
		if remote == nil {
			result.Error = "Connection not present, active Gmail, or usable with this key"
		} else {
			profile, e := c.Gmail(remote.Key, nil, nil).Profile(ctx, "")
			if e != nil || profile.HistoryID == "" {
				result.Error = "Gmail profile/history inaccessible; reconnect in One"
			} else {
				inbox := idgen.New("in")
				b := store.OneBinding{InboxID: inbox, AccountID: p.AccountID, ConnectionID: remote.ID, GmailEmail: profile.EmailAddress, KeyAAD: oneKeyAAD(p.AccountID, inbox), Fingerprint: s.Fingerprint, RateGroup: group}
				b.DisplayName, _ = c.SenderName(ctx, remote.Key, profile.EmailAddress)
				b.EncryptedKey, e = m.Service.EncryptSecretAAD(b.KeyAAD, []byte(key))
				if e == nil {
					b.EncryptedConnectionKey, e = m.Service.EncryptSecretAAD(oneBindingAAD(p.AccountID, inbox), []byte(remote.Key))
				}
				if e == nil {
					result.InboxID, e = m.Service.Store.ImportOneInbox(ctx, b, profile.HistoryID)
				}
				if e != nil {
					result.Error = "Import failed; address already exists or configuration changed"
				} else {
					if after := oneBackfillAfter(windows[id], time.Now()); after != "" {
						if e = m.Service.Store.SetGmailBackfillAfter(ctx, p.AccountID, result.InboxID, after); e != nil {
							result.Error = "Import failed; history window could not be recorded"
						}
					}
				}
				if e == nil {
					m.ScheduleRefresh(p.AccountID, result.InboxID)
				}
			}
		}
		out = append(out, result)
	}
	complete := true
	for _, result := range out {
		if result.Error != "" {
			complete = false
		}
	}
	if complete {
		if err := m.Service.Store.DeleteOneImportSetup(ctx, p.AccountID, p.UserID, setupID); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (m *RemoteMailboxService) oneClient(ctx context.Context, b store.OneBinding) (*one.Client, error) {
	if b.Status == "disconnected" || b.EncryptedKey == "" {
		return nil, oneError(model.ErrKindAuth, "One credentials removed; save a key for this inbox")
	}
	l, err := m.oneKeyLimiter(ctx, b.AccountID, b.Fingerprint, b.RateGroup)
	if err != nil {
		return nil, err
	}
	cacheID := b.AccountID + ":" + b.InboxID
	m.oneMu.Lock()
	cached := m.oneClients[cacheID]
	m.oneMu.Unlock()
	if cached.revision == b.Revision && cached.client != nil {
		return cached.client, nil
	}
	key, err := m.Service.DecryptSecretAAD(b.KeyAAD, b.EncryptedKey)
	if err != nil {
		return nil, oneError(model.ErrKindAuth, "One credentials unavailable")
	}
	c := one.NewClient(string(key), m.OneHTTP, l)
	clear(key)
	m.oneMu.Lock()
	m.oneClients[cacheID] = oneCachedClient{revision: b.Revision, client: c}
	m.oneMu.Unlock()
	return c, nil
}

// Resolve only this inbox's credentials; queued requests verify its revision
// again after pacing. Another inbox's replacement/deletion cannot invalidate it.
func (m *RemoteMailboxService) GoogleMailboxClient(ctx context.Context, account, inbox string) (*gmail.Client, string, error) {
	b, err := m.Service.Store.OneBinding(ctx, account, inbox)
	if errors.Is(err, store.ErrNotFound) {
		t, err := m.GoogleAccess(ctx, account, inbox)
		return m.Google, t, err
	}
	if err != nil {
		return nil, "", err
	}
	c, err := m.oneClient(ctx, b)
	if err != nil {
		return nil, "", err
	}
	key, err := m.Service.DecryptSecretAAD(oneBindingAAD(account, inbox), b.EncryptedConnectionKey)
	if err != nil {
		return nil, "", oneError(model.ErrKindAuth, "One connection credentials unavailable")
	}
	selector := string(key)
	clear(key)
	// Revalidate whenever a failure is pending or the binding is flagged, so the
	// inbox keeps trying in the background: One either confirms the connection,
	// which clears the count and resumes sync without an operator, or the
	// failure is counted again. The periodic check still covers a binding that
	// has never failed.
	if b.FailureCount > 0 || b.Status == "attention" || time.Since(b.LastCheckedAt) > 15*time.Minute {
		if err := m.revalidateOneBinding(ctx, account, inbox, b, c, selector); err != nil {
			return nil, "", err
		}
	}
	// Only a removed or replaced binding stops an attempt; an unhealthy one does
	// not, because every attempt is how the count is earned and cleared.
	check := func(ctx context.Context) error {
		current, e := m.Service.Store.OneBinding(ctx, account, inbox)
		if e != nil || current.Status == "disconnected" || current.Revision != b.Revision {
			return oneError(model.ErrKindAuth, "One connection changed or deleted; refresh settings")
		}
		return nil
	}
	// A real operation One rejects on authorization is evidence the connection
	// is unusable, so it counts. At most one failure is recorded per resolved
	// client, so a single bad sync cannot mark the inbox unavailable on its own.
	// Nothing else counts: a timeout, 5xx, rate limit or connector-action
	// problem is not proof that the stored credential is invalid.
	var counted sync.Once
	status := func(ctx context.Context, e error) {
		var mb *model.MailboxError
		if !errors.As(e, &mb) {
			return
		}
		if mb.Kind != model.ErrKindAuth && mb.Kind != model.ErrKindForbidden {
			return
		}
		counted.Do(func() {
			_ = m.Service.Store.RecordOneFailure(ctx, account, inbox, mb.Message, b.Revision)
		})
	}
	return c.Gmail(selector, check, status), "", nil
}

// revalidateOneBinding asks One for the authoritative verdict on a binding's
// connection. One naming the saved connection as present, usable and bound to
// the expected mailbox clears the failure count; anything else is counted
// towards the threshold that marks the inbox unavailable. It returns an error
// whenever the connection may not be used, so callers never proceed on an
// unconfirmed connection.
func (m *RemoteMailboxService) revalidateOneBinding(ctx context.Context, account, inbox string, b store.OneBinding, c *one.Client, selector string) error {
	rows, err := c.Connections(ctx)
	if err != nil {
		return m.countOneAuthFailure(ctx, account, inbox, b, err)
	}
	selected := findOne(rows, b.ConnectionID)
	if selected == nil || selected.Key != selector {
		return m.oneFailure(ctx, account, inbox, b, "One connection revoked or expired; reconnect the original account in One, then save this inbox's key")
	}
	profile, err := c.Gmail(selector, nil, nil).Profile(ctx, "")
	if err != nil {
		return m.countOneAuthFailure(ctx, account, inbox, b, err)
	}
	if !strings.EqualFold(profile.EmailAddress, b.GmailEmail) {
		return m.oneFailure(ctx, account, inbox, b, "One connection's account or permissions changed; reconnect the original account in One")
	}
	if err := m.Service.Store.ClearOneFailures(ctx, account, inbox, b.Revision); err != nil {
		return err
	}
	m.refreshOneDisplayName(ctx, c, selector, account, inbox, b.GmailEmail)
	return nil
}

// countOneAuthFailure counts an error only when it is an authorization verdict;
// any other failure is passed through uncounted so a transient outage cannot
// mark the connection unavailable.
func (m *RemoteMailboxService) countOneAuthFailure(ctx context.Context, account, inbox string, b store.OneBinding, err error) error {
	var mb *model.MailboxError
	if errors.As(err, &mb) && (mb.Kind == model.ErrKindAuth || mb.Kind == model.ErrKindForbidden) {
		return m.oneFailure(ctx, account, inbox, b, mb.Message)
	}
	return err
}

// oneFailure records one authorization failure and reports the verdict the
// caller should surface. The binding itself decides whether this failure was
// the one that marks it unavailable.
func (m *RemoteMailboxService) oneFailure(ctx context.Context, account, inbox string, b store.OneBinding, detail string) error {
	if err := m.Service.Store.RecordOneFailure(ctx, account, inbox, detail, b.Revision); err != nil {
		return err
	}
	return oneError(model.ErrKindAuth, detail)
}

func (m *RemoteMailboxService) SaveOneInboxKey(ctx context.Context, p model.Principal, inbox, key string) error {
	if err := onePermission(p); err != nil {
		return err
	}
	b, err := m.Service.Store.OneBinding(ctx, p.AccountID, inbox)
	if err != nil {
		return err
	}
	c, rows, err := m.validatedOne(ctx, p, key, b.RateGroup)
	if err != nil {
		return err
	}
	row := findOne(rows, b.ConnectionID)
	if row == nil {
		return oneError(model.ErrKindAuth, "This key does not expose this inbox's original One connection; delete the inbox and import the address again")
	}
	profile, err := c.Gmail(row.Key, nil, nil).Profile(ctx, "")
	if err != nil || !strings.EqualFold(profile.EmailAddress, b.GmailEmail) {
		return oneError(model.ErrKindConflict, "Key would change this inbox's One connection account or permissions")
	}
	b.KeyAAD, b.Fingerprint = oneKeyAAD(p.AccountID, inbox), oneFingerprint(key)
	b.EncryptedKey, err = m.Service.EncryptSecretAAD(b.KeyAAD, []byte(key))
	if err != nil {
		return err
	}
	b.EncryptedConnectionKey, err = m.Service.EncryptSecretAAD(oneBindingAAD(p.AccountID, inbox), []byte(row.Key))
	if err != nil {
		return err
	}
	if err = m.Service.Store.SaveOneBinding(ctx, b, b.Revision); err != nil {
		return err
	}
	m.refreshOneDisplayName(ctx, c, row.Key, p.AccountID, inbox, b.GmailEmail)
	m.forgetOneClient(p.AccountID, inbox)
	m.ScheduleRefresh(p.AccountID, inbox)
	return nil
}

func (m *RemoteMailboxService) DeleteOneInbox(ctx context.Context, p model.Principal, inbox string) error {
	if err := onePermission(p); err != nil {
		return err
	}
	if err := m.Service.Store.DeleteOneInbox(ctx, p.AccountID, inbox); err != nil {
		return err
	}
	m.forgetOneClient(p.AccountID, inbox)
	return nil
}

// forgetOneClient drops the cached One client for an inbox whose credentials
// changed or whose inbox was removed, so the next use re-reads the binding.
func (m *RemoteMailboxService) forgetOneClient(account, inbox string) {
	m.oneMu.Lock()
	delete(m.oneClients, account+":"+inbox)
	m.oneMu.Unlock()
}

func (m *RemoteMailboxService) refreshOneDisplayName(ctx context.Context, c *one.Client, selector, account, inbox, email string) {
	box, err := m.Service.Store.GetInboxInternal(ctx, account, inbox)
	if err != nil || (box.DisplayName != box.Address && box.DisplayName != box.Address+" · Gmail via One") {
		return
	}
	name, err := c.SenderName(ctx, selector, email)
	if err == nil && name != "" {
		_ = m.Service.Store.UpdateOneDisplayName(ctx, account, inbox, name)
	}
}

func (s *Service) bindGmailSend(ctx context.Context, account, inbox string, cfg map[string]any) error {
	if cfg["one_binding"] != true {
		return nil
	}
	if s.GmailMailboxClient == nil || !s.Store.IsOne(ctx, account, inbox) {
		return oneError(model.ErrKindAuth, "One sending binding unavailable")
	}
	client, token, err := s.GmailMailboxClient(ctx, account, inbox)
	if err != nil {
		return err
	}
	cfg["gmail_client"], cfg["access_token"] = client, token
	return nil
}
