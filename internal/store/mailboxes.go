package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"gatehouse-mail/internal/idgen"
	"gatehouse-mail/internal/model"
)

func (s *Store) GetAccount(ctx context.Context, accountID string) (model.Account, error) {
	var a model.Account
	var created string
	err := s.read.QueryRowContext(ctx, `SELECT id,name,storage_quota_bytes,storage_used_bytes,created_at FROM accounts WHERE id=?`, accountID).Scan(&a.ID, &a.Name, &a.StorageQuotaBytes, &a.StorageUsedBytes, &created)
	if err == sql.ErrNoRows {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.CreatedAt = parseTime(created)
	return a, nil
}

func (s *Store) CreateDomain(ctx context.Context, accountID, name string) (model.Domain, error) {
	name = normalizeDomain(name)
	if name == "" || !strings.Contains(name, ".") {
		return model.Domain{}, fmt.Errorf("valid domain required")
	}
	id := idgen.New("dom")
	now := nowText()
	_, err := s.write.ExecContext(ctx, `INSERT INTO domains(id,account_id,name,created_at) VALUES(?,?,?,?)`, id, accountID, name, now)
	if err != nil {
		return model.Domain{}, err
	}
	return model.Domain{ID: id, AccountID: accountID, Name: name, CreatedAt: parseTime(now)}, nil
}

const domainSummarySelect = `SELECT d.id,d.account_id,d.name,COALESCE(d.catch_all_inbox_id,''),COALESCE(sc.provider,''),COALESCE(rc.provider,''),d.created_at
	FROM domains d
	LEFT JOIN domain_sending_configs sc ON sc.domain_id=d.id AND sc.account_id=d.account_id
	LEFT JOIN domain_receiving_configs rc ON rc.domain_id=d.id AND rc.account_id=d.account_id`

func (s *Store) ListDomains(ctx context.Context, accountID string) ([]model.Domain, error) {
	rows, err := s.read.QueryContext(ctx, domainSummarySelect+` WHERE d.account_id=? ORDER BY d.name`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Domain{}
	for rows.Next() {
		var d model.Domain
		var c string
		if err = rows.Scan(&d.ID, &d.AccountID, &d.Name, &d.CatchAllInboxID, &d.SendingProvider, &d.ReceivingProvider, &c); err != nil {
			return nil, err
		}
		d.CreatedAt = parseTime(c)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) GetDomain(ctx context.Context, accountID, domainID string) (model.Domain, error) {
	var d model.Domain
	var c string
	err := s.read.QueryRowContext(ctx, domainSummarySelect+` WHERE d.id=? AND d.account_id=?`, domainID, accountID).Scan(&d.ID, &d.AccountID, &d.Name, &d.CatchAllInboxID, &d.SendingProvider, &d.ReceivingProvider, &c)
	if err == sql.ErrNoRows {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	d.CreatedAt = parseTime(c)
	return d, nil
}

func (s *Store) SetDomainCatchAll(ctx context.Context, accountID, domainID, inboxID string) error {
	if inboxID != "" {
		var n int
		if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM inboxes WHERE id=? AND domain_id=? AND account_id=?`, inboxID, domainID, accountID).Scan(&n); err != nil || n != 1 {
			return ErrForbidden
		}
	}
	res, err := s.write.ExecContext(ctx, `UPDATE domains SET catch_all_inbox_id=? WHERE id=? AND account_id=?`, nullString(inboxID), domainID, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteDomain(ctx context.Context, accountID, domainID string) error {
	res, err := s.write.ExecContext(ctx, `DELETE FROM domains WHERE id=? AND account_id=?`, domainID, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// PurgeDomain permanently deletes a domain and every inbox and message it owns,
// returning the raw .eml paths the caller must unlink from disk. It mirrors
// PurgeInbox but across all inboxes of the domain, and clears message_fts,
// storage accounting, events, idempotency, blocked messages, drafts, key roles
// and relay connections transactionally.
func (s *Store) PurgeDomain(ctx context.Context, accountID, domainID string) ([]string, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM domains WHERE id=? AND account_id=?`, domainID, accountID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, ErrNotFound
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.raw_path,m.size_bytes FROM messages m JOIN inboxes i ON i.id=m.inbox_id WHERE i.account_id=? AND i.domain_id=?`, accountID, domainID)
	if err != nil {
		return nil, err
	}
	var paths []string
	var total int64
	for rows.Next() {
		var path string
		var size int64
		if err = rows.Scan(&path, &size); err != nil {
			rows.Close()
			return nil, err
		}
		if strings.TrimSpace(path) != "" {
			paths = append(paths, path)
		}
		total += size
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	draftRows, err := tx.QueryContext(ctx, `SELECT d.text_body,d.html_body FROM drafts d JOIN inboxes i ON i.id=d.inbox_id WHERE i.account_id=? AND i.domain_id=?`, accountID, domainID)
	if err != nil {
		return nil, err
	}
	for draftRows.Next() {
		var text, html string
		if err = draftRows.Scan(&text, &html); err != nil {
			draftRows.Close()
			return nil, err
		}
		total += int64(len(text) + len(html))
	}
	if err = draftRows.Err(); err != nil {
		draftRows.Close()
		return nil, err
	}
	draftRows.Close()
	attRows, err := tx.QueryContext(ctx, `SELECT da.raw_path,da.size_bytes FROM draft_attachments da JOIN drafts d ON d.id=da.draft_id JOIN inboxes i ON i.id=d.inbox_id WHERE i.account_id=? AND i.domain_id=?`, accountID, domainID)
	if err != nil {
		return nil, err
	}
	for attRows.Next() {
		var path string
		var size int64
		if err = attRows.Scan(&path, &size); err != nil {
			attRows.Close()
			return nil, err
		}
		if strings.TrimSpace(path) != "" {
			paths = append(paths, path)
		}
		total += size
	}
	if err = attRows.Err(); err != nil {
		attRows.Close()
		return nil, err
	}
	attRows.Close()
	wfRows, err := tx.QueryContext(ctx, `SELECT w.raw_path FROM outbound_workflow w JOIN inboxes i ON i.id=w.inbox_id WHERE i.account_id=? AND i.domain_id=?`, accountID, domainID)
	if err != nil {
		return nil, err
	}
	for wfRows.Next() {
		var path string
		if err = wfRows.Scan(&path); err != nil {
			wfRows.Close()
			return nil, err
		}
		if strings.TrimSpace(path) != "" {
			paths = append(paths, path)
		}
	}
	if err = wfRows.Err(); err != nil {
		wfRows.Close()
		return nil, err
	}
	wfRows.Close()
	if _, err = tx.ExecContext(ctx, `DELETE FROM message_fts WHERE message_id IN (SELECT m.id FROM messages m JOIN inboxes i ON i.id=m.inbox_id WHERE i.account_id=? AND i.domain_id=?)`, accountID, domainID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM outbound_idempotency WHERE account_id=? AND message_id IN (SELECT m.id FROM messages m JOIN inboxes i ON i.id=m.inbox_id WHERE i.account_id=? AND i.domain_id=?)`, accountID, accountID, domainID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM messages WHERE inbox_id IN (SELECT id FROM inboxes WHERE account_id=? AND domain_id=?)`, accountID, domainID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM threads WHERE inbox_id IN (SELECT id FROM inboxes WHERE account_id=? AND domain_id=?)`, accountID, domainID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM events WHERE inbox_id IN (SELECT id FROM inboxes WHERE account_id=? AND domain_id=?)`, accountID, domainID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM blocked_messages WHERE inbox_id IN (SELECT id FROM inboxes WHERE account_id=? AND domain_id=?)`, accountID, domainID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM drafts WHERE inbox_id IN (SELECT id FROM inboxes WHERE account_id=? AND domain_id=?)`, accountID, domainID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM hermes_connections WHERE inbox_id IN (SELECT id FROM inboxes WHERE account_id=? AND domain_id=?)`, accountID, domainID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM hermes_enroll_tokens WHERE inbox_id IN (SELECT id FROM inboxes WHERE account_id=? AND domain_id=?)`, accountID, domainID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM api_key_mailbox_roles WHERE inbox_id IN (SELECT id FROM inboxes WHERE account_id=? AND domain_id=?)`, accountID, domainID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE accounts SET storage_used_bytes=MAX(0,storage_used_bytes-?) WHERE id=?`, total, accountID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM inboxes WHERE account_id=? AND domain_id=?`, accountID, domainID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM domains WHERE id=? AND account_id=?`, domainID, accountID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return paths, nil
}

func (s *Store) CreateInbox(ctx context.Context, accountID, domainID, localPart, display string) (model.Inbox, error) {
	localPart = normalizeLocal(localPart)
	if localPart == "" || strings.ContainsAny(localPart, "@ <>\t\r\n") {
		return model.Inbox{}, fmt.Errorf("invalid local part")
	}
	var domain string
	if err := s.read.QueryRowContext(ctx, `SELECT name FROM domains WHERE id=? AND account_id=?`, domainID, accountID).Scan(&domain); err == sql.ErrNoRows {
		return model.Inbox{}, ErrForbidden
	} else if err != nil {
		return model.Inbox{}, err
	}
	addr := localPart + "@" + domain
	if _, err := mail.ParseAddress(addr); err != nil {
		return model.Inbox{}, fmt.Errorf("invalid address: %w", err)
	}
	var aliasCollision int
	if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM inbox_aliases WHERE domain_id=? AND local_part=?`, domainID, localPart).Scan(&aliasCollision); err != nil {
		return model.Inbox{}, err
	}
	if aliasCollision != 0 {
		return model.Inbox{}, fmt.Errorf("address is already an alias")
	}
	id := idgen.New("in")
	now := nowText()
	_, err := s.write.ExecContext(ctx, `INSERT INTO inboxes(id,account_id,domain_id,local_part,display_name,created_at) VALUES(?,?,?,?,?,?)`, id, accountID, domainID, localPart, strings.TrimSpace(display), now)
	if err != nil {
		return model.Inbox{}, err
	}
	return model.Inbox{ID: id, AccountID: accountID, DomainID: domainID, LocalPart: localPart, Address: addr, DisplayName: display, Enabled: true, CreatedAt: parseTime(now)}, nil
}
func (s *Store) ListInboxes(ctx context.Context, p model.Principal) ([]model.Inbox, error) {
	q := `SELECT i.id,i.account_id,i.domain_id,i.local_part,d.name,i.display_name,i.enabled,i.allowed_senders_json,i.sender_restricted,i.require_authenticated,i.approver_email,i.default_sender,i.created_at FROM inboxes i JOIN domains d ON d.id=i.domain_id WHERE i.account_id=?`
	args := []any{p.AccountID}
	if !p.Admin {
		ids := principalInboxIDs(p)
		if len(ids) == 0 {
			return []model.Inbox{}, nil
		}
		q += ` AND i.id IN (` + placeholders(len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	q += ` ORDER BY d.name,i.local_part`
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Inbox{}
	for rows.Next() {
		var i model.Inbox
		var domain, allowed, created string
		var enabled, restricted, requireAuth int
		if err = rows.Scan(&i.ID, &i.AccountID, &i.DomainID, &i.LocalPart, &domain, &i.DisplayName, &enabled, &allowed, &restricted, &requireAuth, &i.ApproverEmail, &i.DefaultSender, &created); err != nil {
			return nil, err
		}
		i.Address = i.LocalPart + "@" + domain
		i.Enabled = enabled != 0
		i.AllowedSenders = decodeStrings(allowed)
		i.SenderRestricted = restricted != 0
		i.RequireAuthenticated = requireAuth != 0
		i.CreatedAt = parseTime(created)
		out = append(out, i)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if len(out) > 0 {
		aliases, aerr := s.ListInboxAliases(ctx, p.AccountID)
		if aerr != nil {
			return nil, aerr
		}
		external, eerr := s.ListExternalAliases(ctx, p.AccountID)
		if eerr != nil {
			return nil, eerr
		}
		for i := range out {
			out[i].ExternalAliases = external[out[i].ID]
			out[i].Aliases = aliasAddresses(aliases[out[i].ID])
			out[i].AliasNames = aliasNames(aliases[out[i].ID])
		}
	}
	return out, nil
}

// aliasAddresses flattens alias rows into the full addresses an inbox displays.
func aliasAddresses(aliases []InboxAlias) []string {
	out := make([]string, 0, len(aliases))
	for _, a := range aliases {
		out = append(out, a.Address)
	}
	return out
}

// aliasNames maps an inbox's alias addresses to their sender display names,
// omitting aliases with no name.
func aliasNames(aliases []InboxAlias) map[string]string {
	out := map[string]string{}
	for _, a := range aliases {
		if a.DisplayName != "" {
			out[a.Address] = a.DisplayName
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Store) GetInbox(ctx context.Context, p model.Principal, id string) (model.Inbox, error) {
	if !p.CanRead(id) {
		return model.Inbox{}, ErrForbidden
	}
	return s.GetInboxInternal(ctx, p.AccountID, id)
}
func (s *Store) GetInboxInternal(ctx context.Context, accountID, id string) (model.Inbox, error) {
	var i model.Inbox
	var domain, allowed, created string
	var enabled, restricted, requireAuth int
	err := s.read.QueryRowContext(ctx, `SELECT i.id,i.account_id,i.domain_id,i.local_part,d.name,i.display_name,i.enabled,i.allowed_senders_json,i.sender_restricted,i.require_authenticated,i.approver_email,i.default_sender,i.created_at FROM inboxes i JOIN domains d ON d.id=i.domain_id WHERE i.id=? AND i.account_id=?`, id, accountID).Scan(&i.ID, &i.AccountID, &i.DomainID, &i.LocalPart, &domain, &i.DisplayName, &enabled, &allowed, &restricted, &requireAuth, &i.ApproverEmail, &i.DefaultSender, &created)
	if err == sql.ErrNoRows {
		return i, ErrNotFound
	}
	if err != nil {
		return i, err
	}
	i.Address = i.LocalPart + "@" + domain
	i.Enabled = enabled != 0
	i.AllowedSenders = decodeStrings(allowed)
	i.SenderRestricted = restricted != 0
	i.RequireAuthenticated = requireAuth != 0
	i.CreatedAt = parseTime(created)
	rows, err := s.read.QueryContext(ctx, `SELECT a.local_part,d.name,a.display_name FROM inbox_aliases a JOIN domains d ON d.id=a.domain_id WHERE a.inbox_id=? AND a.account_id=? ORDER BY d.name,a.local_part`, id, accountID)
	if err != nil {
		return i, err
	}
	defer rows.Close()
	for rows.Next() {
		var local, aliasDomain, aliasName string
		if err = rows.Scan(&local, &aliasDomain, &aliasName); err != nil {
			return i, err
		}
		addr := local + "@" + aliasDomain
		i.Aliases = append(i.Aliases, addr)
		if aliasName != "" {
			if i.AliasNames == nil {
				i.AliasNames = map[string]string{}
			}
			i.AliasNames[addr] = aliasName
		}
	}
	if err = rows.Err(); err != nil {
		return i, err
	}
	rows.Close()
	external, err := s.ListExternalAliasesForInbox(ctx, accountID, id)
	if err != nil {
		return i, err
	}
	i.ExternalAliases = external
	return i, nil
}
func (s *Store) UpdateInbox(ctx context.Context, p model.Principal, id, display string, enabled *bool) error {
	if !p.CanOwn(id) && !p.Admin {
		return ErrForbidden
	}
	if display != "" {
		res, err := s.write.ExecContext(ctx, `UPDATE inboxes SET display_name=? WHERE id=? AND account_id=?`, strings.TrimSpace(display), id, p.AccountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
	}
	if enabled != nil {
		res, err := s.write.ExecContext(ctx, `UPDATE inboxes SET enabled=? WHERE id=? AND account_id=?`, boolInt(*enabled), id, p.AccountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
	}
	return nil
}

// SetInboxDisplayName sets an inbox's display name, including clearing it.
func (s *Store) SetInboxDisplayName(ctx context.Context, accountID, id, display string) error {
	res, err := s.write.ExecContext(ctx, `UPDATE inboxes SET display_name=? WHERE id=? AND account_id=?`, strings.TrimSpace(display), id, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetInboxAllowedSenders replaces an inbox's allowed-senders allowlist. An
// empty list restores unrestricted delivery.
func (s *Store) SetInboxAllowedSenders(ctx context.Context, accountID, inboxID string, senders []string) error {
	if senders == nil {
		senders = []string{}
	}
	res, err := s.write.ExecContext(ctx, `UPDATE inboxes SET allowed_senders_json=? WHERE id=? AND account_id=?`, jsonString(senders), inboxID, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetInboxSenderRestricted toggles whether an inbox enforces its
// allowed-senders list. When false, any sender is accepted and the list is
// ignored.
func (s *Store) SetInboxSenderRestricted(ctx context.Context, accountID, inboxID string, restricted bool) error {
	res, err := s.write.ExecContext(ctx, `UPDATE inboxes SET sender_restricted=? WHERE id=? AND account_id=?`, boolInt(restricted), inboxID, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetInboxRequireAuthenticated toggles the MX-only authenticated-sender
// requirement. It is only meaningful when the allow-list is enforced; it has no
// effect on webhook providers, which carry no authentication evidence.
func (s *Store) SetInboxRequireAuthenticated(ctx context.Context, accountID, inboxID string, require bool) error {
	res, err := s.write.ExecContext(ctx, `UPDATE inboxes SET require_authenticated=? WHERE id=? AND account_id=?`, boolInt(require), inboxID, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetInboxApprover replaces an inbox's external approver. An empty email clears
// the approver. Outstanding send requests keep the approver they were created
// with, so changing the inbox setting never invalidates a pending decision.
func (s *Store) SetInboxApprover(ctx context.Context, accountID, inboxID, email string) error {
	res, err := s.write.ExecContext(ctx, `UPDATE inboxes SET approver_email=? WHERE id=? AND account_id=?`, strings.ToLower(strings.TrimSpace(email)), inboxID, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// maxInboxAliases bounds the aliases a single inbox may carry so a bad client
// cannot grow the table without limit.
const maxInboxAliases = 100

// InboxAlias is an alternate inbound address that delivers to an inbox. It is
// an address-to-inbox mapping, not a mailbox: the target may live on a
// different domain of the same account.
type InboxAlias struct {
	ID          string    `json:"id"`
	AccountID   string    `json:"account_id"`
	DomainID    string    `json:"domain_id"`
	InboxID     string    `json:"inbox_id"`
	LocalPart   string    `json:"local_part"`
	Address     string    `json:"address"`
	DisplayName string    `json:"display_name,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// AliasInput is one desired alias on SetInboxAliases: a local part and the id
// of the domain it lives on (which may differ from the target inbox's domain),
// plus an optional sender display name.
type AliasInput struct {
	DomainID    string
	LocalPart   string
	DisplayName string
}

// maxAliasDisplayName bounds an alias's sender display name.
const maxAliasDisplayName = 128

// NormalizeAliasDisplayName trims and validates an alias's sender display name.
// An empty value clears it (falling back to the inbox name). Control characters
// and commas are rejected: control characters would corrupt the From header,
// and commas are significant to the parallel-field UI encoding and to RFC 5322
// address lists.
func NormalizeAliasDisplayName(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	if len([]rune(v)) > maxAliasDisplayName {
		return "", fmt.Errorf("alias display name is too long")
	}
	if strings.ContainsAny(v, ",\r\n") {
		return "", fmt.Errorf("alias display name may not contain commas or newlines")
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("alias display name contains control characters")
		}
	}
	return v, nil
}

// ListInboxAliases returns every alias in an account grouped by target inbox id.
func (s *Store) ListInboxAliases(ctx context.Context, accountID string) (map[string][]InboxAlias, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT a.id,a.account_id,a.domain_id,a.inbox_id,a.local_part,d.name,a.display_name,a.created_at FROM inbox_aliases a JOIN domains d ON d.id=a.domain_id WHERE a.account_id=? ORDER BY d.name,a.local_part`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]InboxAlias{}
	for rows.Next() {
		var a InboxAlias
		var domain, created string
		if err = rows.Scan(&a.ID, &a.AccountID, &a.DomainID, &a.InboxID, &a.LocalPart, &domain, &a.DisplayName, &created); err != nil {
			return nil, err
		}
		a.Address = a.LocalPart + "@" + domain
		a.CreatedAt = parseTime(created)
		out[a.InboxID] = append(out[a.InboxID], a)
	}
	return out, rows.Err()
}

// SetInboxAliases replaces an inbox's alias set transactionally. Every alias
// domain must belong to the account, every local part must be valid and must
// not shadow an existing mailbox on the same domain, and no two aliases may
// share an address. A missing or foreign inbox is ErrNotFound.
func (s *Store) SetInboxAliases(ctx context.Context, accountID, inboxID string, aliases []AliasInput) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM inbox_aliases WHERE account_id=? AND inbox_id=?`, accountID, inboxID); err != nil {
		return err
	}
	var externalCount int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM external_aliases WHERE inbox_id=?`, inboxID).Scan(&externalCount); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, in := range aliases {
		local := normalizeLocal(in.LocalPart)
		if local == "" || strings.ContainsAny(local, "@ <>\t\r\n") {
			return fmt.Errorf("invalid alias local part")
		}
		var domainName string
		if err = tx.QueryRowContext(ctx, `SELECT name FROM domains WHERE id=? AND account_id=?`, in.DomainID, accountID).Scan(&domainName); err == sql.ErrNoRows {
			return ErrForbidden
		} else if err != nil {
			return err
		}
		if _, err = mail.ParseAddress(local + "@" + domainName); err != nil {
			return fmt.Errorf("invalid alias address: %w", err)
		}
		key := in.DomainID + "\x00" + local
		if seen[key] {
			return fmt.Errorf("duplicate alias %s@%s", local, domainName)
		}
		seen[key] = true
		var collision int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inboxes WHERE domain_id=? AND local_part=?`, in.DomainID, local).Scan(&collision); err != nil {
			return err
		}
		if collision != 0 {
			return fmt.Errorf("alias %s@%s is already a mailbox", local, domainName)
		}
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inbox_aliases WHERE domain_id=? AND local_part=?`, in.DomainID, local).Scan(&collision); err != nil {
			return err
		}
		if collision != 0 {
			return fmt.Errorf("alias %s@%s is already in use", local, domainName)
		}
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM external_aliases WHERE inbox_id=? AND address=?`, inboxID, local+"@"+domainName).Scan(&collision); err != nil {
			return err
		}
		if collision != 0 {
			return fmt.Errorf("address already exists as an external alias")
		}
		if len(seen)+externalCount > maxInboxAliases {
			return fmt.Errorf("too many aliases")
		}
		displayName, err := NormalizeAliasDisplayName(in.DisplayName)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_aliases(id,account_id,domain_id,inbox_id,local_part,display_name,created_at) VALUES(?,?,?,?,?,?,?)`, idgen.New("al"), accountID, in.DomainID, inboxID, local, displayName, nowText()); err != nil {
			return err
		}
	}
	// A default sender that is no longer the primary or a surviving alias is
	// cleared, so replacing the alias set can never leave a stale send-from.
	var currentDefault string
	if err = tx.QueryRowContext(ctx, `SELECT default_sender FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&currentDefault); err != nil {
		return err
	}
	if currentDefault != "" {
		if _, _, rerr := resolveSenderQuery(ctx, tx, accountID, inboxID, currentDefault); rerr != nil {
			if !errors.Is(rerr, ErrForbidden) && !errors.Is(rerr, ErrNotFound) {
				return rerr
			}
			if _, err = tx.ExecContext(ctx, `UPDATE inboxes SET default_sender='' WHERE id=? AND account_id=?`, inboxID, accountID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// senderQueryer is the row-read surface shared by the store's read pool and an
// open transaction, so sender resolution works both standalone and inside a
// transaction.
type senderQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// resolveSenderQuery maps a requested sender address to its canonical From
// identity (display name plus address) and the id of the domain whose sending
// configuration must be used. An empty request, or one matching the inbox
// primary address, resolves to the primary and the inbox's own domain, using
// the inbox display name. Any other address must match one of the inbox's
// managed aliases (its own domain and display name) or external aliases (who
// carry their own sending connector). A request that is neither is ErrForbidden.
func resolveSenderQuery(ctx context.Context, q senderQueryer, accountID, inboxID, requested string) (model.Address, string, error) {
	from, target, err := resolveSendingTargetQuery(ctx, q, accountID, inboxID, requested)
	return from, target.DomainID, err
}

// resolveSendingTargetQuery is resolveSenderQuery generalized to return the
// full sending target: a managed domain id, or the immutable id of an external
// sending alias. Exactly one of the two is set.
func resolveSendingTargetQuery(ctx context.Context, q senderQueryer, accountID, inboxID, requested string) (model.Address, SendingTarget, error) {
	var primaryName, primary, domainID string
	if err := q.QueryRowContext(ctx, `SELECT i.display_name,i.local_part||'@'||d.name,i.domain_id FROM inboxes i JOIN domains d ON d.id=i.domain_id WHERE i.id=? AND i.account_id=?`, inboxID, accountID).Scan(&primaryName, &primary, &domainID); err != nil {
		if err == sql.ErrNoRows {
			return model.Address{}, SendingTarget{}, ErrNotFound
		}
		return model.Address{}, SendingTarget{}, err
	}
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "" || requested == strings.ToLower(primary) {
		return model.Address{Name: primaryName, Address: primary}, SendingTarget{DomainID: domainID}, nil
	}
	var alias, aliasDomainID, aliasName, externalID string
	err := q.QueryRowContext(ctx, `SELECT a.local_part||'@'||d.name,a.domain_id,COALESCE(NULLIF(a.display_name,''),i.display_name) FROM inbox_aliases a JOIN domains d ON d.id=a.domain_id JOIN inboxes i ON i.id=a.inbox_id WHERE a.account_id=? AND a.inbox_id=? AND (a.local_part||'@'||d.name)=?`, accountID, inboxID, requested).Scan(&alias, &aliasDomainID, &aliasName)
	if err == sql.ErrNoRows {
		err = q.QueryRowContext(ctx, `SELECT e.address,COALESCE(NULLIF(e.display_name,''),i.display_name),e.id FROM external_aliases e JOIN inboxes i ON i.id=e.inbox_id AND i.account_id=e.account_id WHERE e.account_id=? AND e.inbox_id=? AND e.address=?`, accountID, inboxID, requested).Scan(&alias, &aliasName, &externalID)
		if err == sql.ErrNoRows {
			return model.Address{}, SendingTarget{}, fmt.Errorf("%w: %w", ErrForbidden, ErrSenderNotAllowed)
		}
	}
	if err != nil {
		return model.Address{}, SendingTarget{}, err
	}
	if externalID != "" {
		return model.Address{Name: aliasName, Address: alias}, SendingTarget{ExternalAliasID: externalID}, nil
	}
	return model.Address{Name: aliasName, Address: alias}, SendingTarget{DomainID: aliasDomainID}, nil
}

// SetInboxDefaultSender sets the address compose/reply preselects as From. It
// must be the inbox primary or one of its aliases; an empty value clears it
// back to the primary.
func (s *Store) SetInboxDefaultSender(ctx context.Context, accountID, inboxID, address string) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, _, err = resolveSenderQuery(ctx, tx, accountID, inboxID, address); err != nil {
		return err
	}
	value := ""
	if strings.TrimSpace(address) != "" {
		value = strings.ToLower(strings.TrimSpace(address))
	}
	res, err := tx.ExecContext(ctx, `UPDATE inboxes SET default_sender=? WHERE id=? AND account_id=?`, value, inboxID, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// PurgeInbox permanently deletes an inbox and every row it owns, returning the
// raw .eml paths the caller must unlink from disk. Unlike a bare DELETE it also
// clears message_fts, decrements storage accounting, removes events and detaches
// any domain catch-all pointing at the inbox. Attachments, drafts, blocked
// messages, relay connections, enroll tokens and key roles cascade via their
// inbox foreign keys.
func (s *Store) PurgeInbox(ctx context.Context, accountID, id string) ([]string, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var exists int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inboxes WHERE id=? AND account_id=?`, id, accountID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, ErrNotFound
	}
	rows, err := tx.QueryContext(ctx, `SELECT raw_path,size_bytes FROM messages WHERE account_id=? AND inbox_id=?`, accountID, id)
	if err != nil {
		return nil, err
	}
	var paths []string
	var total int64
	for rows.Next() {
		var path string
		var size int64
		if err = rows.Scan(&path, &size); err != nil {
			rows.Close()
			return nil, err
		}
		if strings.TrimSpace(path) != "" {
			paths = append(paths, path)
		}
		total += size
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	draftRows, err := tx.QueryContext(ctx, `SELECT text_body,html_body FROM drafts WHERE account_id=? AND inbox_id=?`, accountID, id)
	if err != nil {
		return nil, err
	}
	for draftRows.Next() {
		var text, html string
		if err = draftRows.Scan(&text, &html); err != nil {
			draftRows.Close()
			return nil, err
		}
		total += int64(len(text) + len(html))
	}
	if err = draftRows.Err(); err != nil {
		draftRows.Close()
		return nil, err
	}
	draftRows.Close()
	attRows, err := tx.QueryContext(ctx, `SELECT da.raw_path,da.size_bytes FROM draft_attachments da JOIN drafts d ON d.id=da.draft_id WHERE d.account_id=? AND d.inbox_id=?`, accountID, id)
	if err != nil {
		return nil, err
	}
	for attRows.Next() {
		var path string
		var size int64
		if err = attRows.Scan(&path, &size); err != nil {
			attRows.Close()
			return nil, err
		}
		if strings.TrimSpace(path) != "" {
			paths = append(paths, path)
		}
		total += size
	}
	if err = attRows.Err(); err != nil {
		attRows.Close()
		return nil, err
	}
	attRows.Close()
	wfRows, err := tx.QueryContext(ctx, `SELECT raw_path FROM outbound_workflow WHERE account_id=? AND inbox_id=?`, accountID, id)
	if err != nil {
		return nil, err
	}
	for wfRows.Next() {
		var path string
		if err = wfRows.Scan(&path); err != nil {
			wfRows.Close()
			return nil, err
		}
		if strings.TrimSpace(path) != "" {
			paths = append(paths, path)
		}
	}
	if err = wfRows.Err(); err != nil {
		wfRows.Close()
		return nil, err
	}
	wfRows.Close()
	if _, err = tx.ExecContext(ctx, `DELETE FROM message_fts WHERE message_id IN (SELECT id FROM messages WHERE account_id=? AND inbox_id=?)`, accountID, id); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM outbound_idempotency WHERE account_id=? AND message_id IN (SELECT id FROM messages WHERE account_id=? AND inbox_id=?)`, accountID, accountID, id); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM messages WHERE account_id=? AND inbox_id=?`, accountID, id); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM threads WHERE account_id=? AND inbox_id=?`, accountID, id); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM events WHERE account_id=? AND inbox_id=?`, accountID, id); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE domains SET catch_all_inbox_id=NULL WHERE account_id=? AND catch_all_inbox_id=?`, accountID, id); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE accounts SET storage_used_bytes=MAX(0,storage_used_bytes-?) WHERE id=?`, total, accountID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM inboxes WHERE id=? AND account_id=?`, id, accountID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return paths, nil
}

// RecipientRoute reports how ResolveRecipient matched an address, so the
// ingest core can apply the right binding check: exact and catch-all matches
// must stay inside the authenticated domain, while an alias may cross domains
// within the account.
type RecipientRoute int

const (
	// RouteNone means the address did not resolve.
	RouteNone RecipientRoute = iota
	// RouteInbox is a direct match on a real inbox address.
	RouteInbox
	// RouteAlias is a match on an alias that delivers to another inbox.
	RouteAlias
	// RouteCatchAll is a match on the domain catch-all inbox.
	RouteCatchAll
)

const inboxSelectCols = `i.id,i.account_id,i.domain_id,i.local_part,d.name,i.display_name,i.enabled,i.allowed_senders_json,i.sender_restricted,i.require_authenticated,i.approver_email,i.created_at`

func scanResolvedInbox(sc interface {
	Scan(dest ...any) error
}) (model.Inbox, error) {
	var i model.Inbox
	var domain, allowed, created string
	var enabled, restricted, requireAuth int
	if err := sc.Scan(&i.ID, &i.AccountID, &i.DomainID, &i.LocalPart, &domain, &i.DisplayName, &enabled, &allowed, &restricted, &requireAuth, &i.ApproverEmail, &created); err != nil {
		return model.Inbox{}, err
	}
	i.Address = i.LocalPart + "@" + domain
	i.Enabled = enabled != 0
	i.AllowedSenders = decodeStrings(allowed)
	i.SenderRestricted = restricted != 0
	i.RequireAuthenticated = requireAuth != 0
	i.CreatedAt = parseTime(created)
	return i, nil
}

// ResolveRecipient maps an address to its delivery inbox, in precedence order:
// exact inbox, then an alias on the address's domain, then the domain
// catch-all. The returned route tells the caller which match was used. A
// disabled inbox is treated as unresolved.
func (s *Store) ResolveRecipient(ctx context.Context, address string) (model.Inbox, RecipientRoute, error) {
	address = normalizeAddress(address)
	parts := strings.Split(address, "@")
	if len(parts) != 2 {
		return model.Inbox{}, RouteNone, ErrNotFound
	}
	local, domain := parts[0], parts[1]
	var accountID, domainID, domainName, catch string
	err := s.read.QueryRowContext(ctx, `SELECT account_id,id,name,COALESCE(catch_all_inbox_id,'') FROM domains WHERE name=?`, domain).Scan(&accountID, &domainID, &domainName, &catch)
	if err == sql.ErrNoRows {
		return model.Inbox{}, RouteNone, ErrNotFound
	}
	if err != nil {
		return model.Inbox{}, RouteNone, err
	}
	inbox, err := scanResolvedInbox(s.read.QueryRowContext(ctx, `SELECT `+inboxSelectCols+` FROM inboxes i JOIN domains d ON d.id=i.domain_id WHERE i.domain_id=? AND i.local_part=?`, domainID, local))
	if err == nil {
		if !inbox.Enabled {
			return model.Inbox{}, RouteNone, ErrNotFound
		}
		return inbox, RouteInbox, nil
	}
	if err != sql.ErrNoRows {
		return model.Inbox{}, RouteNone, err
	}
	// An alias lives on the address's own domain but delivers to its target
	// inbox, which may be on a different domain of the same account.
	inbox, err = scanResolvedInbox(s.read.QueryRowContext(ctx, `SELECT `+inboxSelectCols+` FROM inbox_aliases a JOIN inboxes i ON i.id=a.inbox_id JOIN domains d ON d.id=i.domain_id WHERE a.domain_id=? AND a.local_part=?`, domainID, local))
	if err == nil {
		if !inbox.Enabled {
			return model.Inbox{}, RouteNone, ErrNotFound
		}
		return inbox, RouteAlias, nil
	}
	if err != sql.ErrNoRows {
		return model.Inbox{}, RouteNone, err
	}
	if catch != "" {
		inbox, err = scanResolvedInbox(s.read.QueryRowContext(ctx, `SELECT `+inboxSelectCols+` FROM inboxes i JOIN domains d ON d.id=i.domain_id WHERE i.id=? AND i.domain_id=?`, catch, domainID))
		if err == nil {
			if !inbox.Enabled {
				return model.Inbox{}, RouteNone, ErrNotFound
			}
			return inbox, RouteCatchAll, nil
		}
		if err != sql.ErrNoRows {
			return model.Inbox{}, RouteNone, err
		}
	}
	return model.Inbox{}, RouteNone, ErrNotFound
}
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}
