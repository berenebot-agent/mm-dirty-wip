package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/idgen"
	"github.com/dellarb/mailmoose/internal/model"
	"github.com/dellarb/mailmoose/internal/store"
	"github.com/dellarb/mailmoose/internal/transport/gmail"
	"github.com/dellarb/mailmoose/internal/transport/one"
)

type oneCachedClient struct {
	revision int64
	client   *one.Client
}
type OneMailboxChoice struct {
	ID          string `json:"id"`
	Email       string `json:"email,omitempty"`
	Provider    string `json:"provider"`
	OneStatus   string `json:"one_status"`
	Environment string `json:"environment"`
	InboxID     string `json:"inbox_id,omitempty"`
	Status      string `json:"status"`
	Available   bool   `json:"available"`
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
				} else {
					row.Status = "Cannot verify Gmail identity/permissions; reconnect in One"
				}
			}
		}
		for _, b := range bindings {
			if b.ConnectionID == row.ID {
				row.InboxID, row.Email, row.Available = b.InboxID, b.GmailEmail, false
				row.Status = "Imported (" + b.Status + ")"
			}
		}
		if row.Available {
			for _, box := range boxes {
				if strings.EqualFold(box.Address, row.Email) {
					row.Status, row.Available = "Address already configured; explicit migration required", false
				}
			}
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

func (m *RemoteMailboxService) ImportOne(ctx context.Context, p model.Principal, setupID string, ids []string) ([]OneImportResult, error) {
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
	if b.Status == "attention" {
		return nil, "", oneError(model.ErrKindAuth, "One connection requires attention; save the key again for this inbox")
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
	if time.Since(b.LastCheckedAt) > 15*time.Minute {
		rows, e := c.Connections(ctx)
		if e != nil {
			return nil, "", e
		}
		selected := findOne(rows, b.ConnectionID)
		if selected == nil || selected.Key != selector {
			_ = m.Service.Store.SetOneHealth(ctx, account, inbox, "attention", "Reconnect the original Gmail connection in One, then save this inbox's key", b.Revision)
			return nil, "", oneError(model.ErrKindAuth, "One connection requires attention")
		}
		profile, e := c.Gmail(selector, nil, nil).Profile(ctx, "")
		if e != nil || !strings.EqualFold(profile.EmailAddress, b.GmailEmail) {
			_ = m.Service.Store.SetOneHealth(ctx, account, inbox, "attention", "Gmail identity or permission changed; explicit reconnect required", b.Revision)
			return nil, "", oneError(model.ErrKindAuth, "Gmail identity or permission changed")
		}
		_ = m.Service.Store.SetOneHealth(ctx, account, inbox, "active", "", b.Revision)
		m.refreshOneDisplayName(ctx, c, selector, account, inbox, b.GmailEmail)
	}
	check := func(ctx context.Context) error {
		current, e := m.Service.Store.OneBinding(ctx, account, inbox)
		if e != nil || current.Status == "disconnected" || current.Status == "attention" || current.Revision != b.Revision {
			return oneError(model.ErrKindAuth, "One connection changed or deleted; refresh settings")
		}
		return nil
	}
	status := func(ctx context.Context, e error) {
		var mb *model.MailboxError
		if errors.As(e, &mb) {
			state := "active"
			if mb.Kind == model.ErrKindAuth || mb.Kind == model.ErrKindForbidden || mb.Kind == model.ErrKindUnsupported {
				state = "attention"
			}
			_ = m.Service.Store.SetOneHealth(ctx, account, inbox, state, mb.Message, b.Revision)
		}
	}
	return c.Gmail(selector, check, status), "", nil
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
		return oneError(model.ErrKindAuth, "Key must expose this inbox's original Gmail connection")
	}
	profile, err := c.Gmail(row.Key, nil, nil).Profile(ctx, "")
	if err != nil || !strings.EqualFold(profile.EmailAddress, b.GmailEmail) {
		return oneError(model.ErrKindConflict, "Key would change this inbox's Gmail identity or permissions")
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
	m.oneMu.Lock()
	delete(m.oneClients, p.AccountID+":"+inbox)
	m.oneMu.Unlock()
	m.ScheduleRefresh(p.AccountID, inbox)
	return nil
}

func (m *RemoteMailboxService) DeleteOneConnection(ctx context.Context, p model.Principal, inbox string) error {
	if err := onePermission(p); err != nil {
		return err
	}
	if err := m.Service.Store.DeleteOneConnection(ctx, p.AccountID, inbox); err != nil {
		return err
	}
	m.oneMu.Lock()
	delete(m.oneClients, p.AccountID+":"+inbox)
	m.oneMu.Unlock()
	return nil
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
