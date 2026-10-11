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

func oneAccountAAD(account, id string) string    { return "one_account:" + account + ":" + id }
func oneBindingAAD(account, inbox string) string { return "one_binding:" + account + ":" + inbox }
func oneError(kind, message string) error        { return model.NewMailboxError(kind, message, false, nil) }

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

func (m *RemoteMailboxService) oneClient(ctx context.Context, a store.OneAccount) (*one.Client, error) {
	limiter := m.oneLimiter(a.ID)
	if a.Status != "active" || a.EncryptedKey == "" {
		return nil, oneError(model.ErrKindAuth, "One credentials removed; configure One again")
	}
	m.oneMu.Lock()
	cached := m.oneClients[a.ID]
	m.oneMu.Unlock()
	if cached.revision == a.Revision && cached.client != nil {
		return cached.client, nil
	}
	b, err := m.Service.DecryptSecretAAD(oneAccountAAD(a.AccountID, a.ID), a.EncryptedKey)
	if err != nil {
		return nil, oneError(model.ErrKindAuth, "One credentials unavailable")
	}
	c := one.NewClient(string(b), m.OneHTTP, limiter)
	clear(b)
	m.oneMu.Lock()
	m.oneClients[a.ID] = oneCachedClient{revision: a.Revision, client: c}
	m.oneMu.Unlock()
	return c, nil
}

func (m *RemoteMailboxService) testOne(ctx context.Context, p model.Principal, key, id string) (*one.Client, []one.Connection, error) {
	if err := onePermission(p); err != nil {
		return nil, nil, err
	}
	if len(key) < 16 || len(key) > 4096 || strings.ContainsAny(key, " \t\r\n") {
		return nil, nil, oneError(model.ErrKindInvalid, "Enter a standard One REST API key")
	}
	c := one.NewClient(key, m.OneHTTP, m.oneLimiter(id))
	rows, err := c.Connections(ctx)
	if err == nil {
		err = c.ValidateActions(ctx)
	}
	return c, rows, err
}

func (m *RemoteMailboxService) TestOneKey(ctx context.Context, p model.Principal, key string) error {
	id := "setup:" + p.AccountID + ":" + p.UserID
	if a, err := m.Service.Store.OneAccount(ctx, p.AccountID, p.UserID); err == nil {
		id = a.ID
	}
	_, _, err := m.testOne(ctx, p, key, id)
	return err
}

func (m *RemoteMailboxService) TestStoredOne(ctx context.Context, p model.Principal) error {
	_, c, err := m.ownedOne(ctx, p)
	if err != nil {
		return err
	}
	if _, err = c.Connections(ctx); err != nil {
		return err
	}
	return c.ValidateActions(ctx)
}

func (m *RemoteMailboxService) RemoveOneAccount(ctx context.Context, p model.Principal, confirmed bool) error {
	if err := onePermission(p); err != nil {
		return err
	}
	a, err := m.Service.Store.OneAccount(ctx, p.AccountID, p.UserID)
	if err != nil {
		return err
	}
	if err = m.Service.Store.RemoveOneAccount(ctx, p.AccountID, p.UserID, confirmed); err != nil {
		return err
	}
	m.oneMu.Lock()
	delete(m.oneClients, a.ID)
	m.oneMu.Unlock()
	// Retain the account limiter across removal/reconfiguration: creating
	// another key must not bypass the upstream account's existing cooldown.
	return nil
}

func (m *RemoteMailboxService) SaveOneKey(ctx context.Context, p model.Principal, key string) error {
	if err := onePermission(p); err != nil {
		return err
	}
	a, err := m.Service.Store.OneAccount(ctx, p.AccountID, p.UserID)
	expected := a.Revision
	if errors.Is(err, store.ErrNotFound) {
		a = store.OneAccount{ID: store.NewOneID(), AccountID: p.AccountID, OwnerUserID: p.UserID}
	} else if err != nil {
		return err
	}
	c, rows, err := m.testOne(ctx, p, key, a.ID)
	if err != nil {
		return err
	}
	bindings, err := m.Service.Store.OneBindings(ctx, p.AccountID, a.ID)
	if err != nil {
		return err
	}
	for i := range bindings {
		b := &bindings[i]
		selected := findOne(rows, b.ConnectionID)
		if selected == nil {
			return oneError(model.ErrKindConflict, "Replacement key lacks an imported Gmail connection; existing credentials retained")
		}
		profile, err := c.Gmail(selected.Key, nil, nil).Profile(ctx, "")
		if err != nil || !strings.EqualFold(profile.EmailAddress, b.GmailEmail) {
			return oneError(model.ErrKindConflict, "Replacement key changes mailbox identity or permissions; explicit migration required")
		}
		b.EncryptedConnectionKey, err = m.Service.EncryptSecretAAD(oneBindingAAD(p.AccountID, b.InboxID), []byte(selected.Key))
		if err != nil {
			return err
		}
	}
	a.EncryptedKey, err = m.Service.EncryptSecretAAD(oneAccountAAD(p.AccountID, a.ID), []byte(key))
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(key))
	a.Fingerprint = hex.EncodeToString(hash[:])
	if err = m.Service.Store.SaveOneAccount(ctx, a, expected, bindings); err != nil {
		return err
	}
	m.oneMu.Lock()
	m.oneClients[a.ID] = oneCachedClient{revision: expected + 1, client: c}
	m.oneMu.Unlock()
	return nil
}

func findOne(rows []one.Connection, id string) *one.Connection {
	for i := range rows {
		if rows[i].ID == id && rows[i].Usable() {
			return &rows[i]
		}
	}
	return nil
}

func (m *RemoteMailboxService) ownedOne(ctx context.Context, p model.Principal) (store.OneAccount, *one.Client, error) {
	if err := onePermission(p); err != nil {
		return store.OneAccount{}, nil, err
	}
	a, err := m.Service.Store.OneAccount(ctx, p.AccountID, p.UserID)
	if err != nil {
		return a, nil, err
	}
	c, err := m.oneClient(ctx, a)
	return a, c, err
}

func (m *RemoteMailboxService) DiscoverOne(ctx context.Context, p model.Principal) ([]OneMailboxChoice, error) {
	a, c, err := m.ownedOne(ctx, p)
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
	bindings, err := m.Service.Store.OneBindings(ctx, p.AccountID, a.ID)
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

type OneImportResult struct {
	ConnectionID string `json:"connection_id"`
	InboxID      string `json:"inbox_id,omitempty"`
	Error        string `json:"error,omitempty"`
}

func (m *RemoteMailboxService) ImportOne(ctx context.Context, p model.Principal, ids []string) ([]OneImportResult, error) {
	a, c, err := m.ownedOne(ctx, p)
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
			result.Error = "Connection not present, active Gmail, or usable in your One account"
		} else {
			profile, e := c.Gmail(remote.Key, nil, nil).Profile(ctx, "")
			if e != nil || profile.HistoryID == "" {
				result.Error = "Gmail profile/history inaccessible; reconnect in One"
			} else {
				inbox := idgen.New("in")
				enc, e := m.Service.EncryptSecretAAD(oneBindingAAD(p.AccountID, inbox), []byte(remote.Key))
				if e == nil {
					result.InboxID, e = m.Service.Store.ImportOne(ctx, a, store.OneBinding{InboxID: inbox, ConnectionID: remote.ID, EncryptedConnectionKey: enc, GmailEmail: profile.EmailAddress}, profile.HistoryID)
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
	return out, nil
}

// GoogleMailboxClient separates authentication from the existing Gmail logic.
// A One API key never becomes a fake Google token or a queued sending secret.
func (m *RemoteMailboxService) GoogleMailboxClient(ctx context.Context, account, inbox string) (*gmail.Client, string, error) {
	b, err := m.Service.Store.OneBinding(ctx, account, inbox)
	if errors.Is(err, store.ErrNotFound) {
		t, err := m.GoogleAccess(ctx, account, inbox)
		return m.Google, t, err
	}
	if err != nil {
		return nil, "", err
	}
	if b.Status == "disconnected" || b.Status == "attention" {
		return nil, "", oneError(model.ErrKindAuth, "One connection requires attention; reconnect in One settings")
	}
	a, err := m.Service.Store.OneAccountByID(ctx, account, b.OneAccountID)
	if err != nil {
		return nil, "", err
	}
	c, err := m.oneClient(ctx, a)
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
			_ = m.Service.Store.SetOneHealth(ctx, account, inbox, "attention", "Reconnect the original Gmail connection in One, then reconnect in MailMoose")
			return nil, "", oneError(model.ErrKindAuth, "One connection requires attention; reconnect the original Gmail connection")
		}
		profile, e := c.Gmail(selector, nil, nil).Profile(ctx, "")
		if e != nil || !strings.EqualFold(profile.EmailAddress, b.GmailEmail) {
			_ = m.Service.Store.SetOneHealth(ctx, account, inbox, "attention", "Gmail identity or permission changed; explicit reconnect required")
			return nil, "", oneError(model.ErrKindAuth, "Gmail identity or permission changed; explicit reconnect required")
		}
		_ = m.Service.Store.SetOneHealth(ctx, account, inbox, "active", "")
	}
	check := func(ctx context.Context) error {
		current, err := m.Service.Store.OneBinding(ctx, account, inbox)
		if err != nil || current.Status == "disconnected" || current.Status == "attention" || current.ConnectionID != b.ConnectionID || current.EncryptedConnectionKey != b.EncryptedConnectionKey {
			return oneError(model.ErrKindAuth, "One binding changed or disconnected; refresh settings")
		}
		active, err := m.Service.Store.OneAccountByID(ctx, account, a.ID)
		if err != nil || active.Status != "active" || active.Revision != a.Revision {
			return oneError(model.ErrKindAuth, "One credentials changed or removed; refresh settings")
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
			_ = m.Service.Store.SetOneHealth(ctx, account, inbox, state, mb.Message)
		}
	}
	return c.Gmail(selector, check, status), "", nil
}

func (m *RemoteMailboxService) ReconnectOne(ctx context.Context, p model.Principal, inbox string) error {
	a, c, err := m.ownedOne(ctx, p)
	if err != nil {
		return err
	}
	b, err := m.Service.Store.OneBinding(ctx, p.AccountID, inbox)
	if err != nil || a.ID != b.OneAccountID {
		return store.ErrForbidden
	}
	rows, err := c.Connections(ctx)
	if err != nil {
		return err
	}
	row := findOne(rows, b.ConnectionID)
	if row == nil {
		return oneError(model.ErrKindAuth, "Reconnect the original Gmail account in One, then refresh")
	}
	profile, err := c.Gmail(row.Key, nil, nil).Profile(ctx, "")
	if err != nil || !strings.EqualFold(profile.EmailAddress, b.GmailEmail) {
		return oneError(model.ErrKindConflict, "Reconnect would change mailbox identity")
	}
	b.EncryptedConnectionKey, err = m.Service.EncryptSecretAAD(oneBindingAAD(p.AccountID, inbox), []byte(row.Key))
	if err != nil {
		return err
	}
	if err = c.ValidateActions(ctx); err != nil {
		return err
	}
	if err = m.Service.Store.SaveOneAccount(ctx, a, a.Revision, []store.OneBinding{b}); err != nil {
		return err
	}
	if err = m.Service.Store.SetOneConnected(ctx, p.AccountID, p.UserID, inbox, true); err != nil {
		return err
	}
	m.ScheduleRefresh(p.AccountID, inbox)
	return nil
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
