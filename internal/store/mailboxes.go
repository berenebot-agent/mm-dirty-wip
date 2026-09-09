package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/mail"
	"strings"

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
func (s *Store) ListDomains(ctx context.Context, accountID string) ([]model.Domain, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT id,account_id,name,COALESCE(catch_all_inbox_id,''),COALESCE(outbound_credential_id,''),COALESCE(inbound_credential_id,''),created_at FROM domains WHERE account_id=? ORDER BY name`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Domain
	for rows.Next() {
		var d model.Domain
		var c string
		if err = rows.Scan(&d.ID, &d.AccountID, &d.Name, &d.CatchAllInboxID, &d.OutboundCredentialID, &d.InboundCredentialID, &c); err != nil {
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
	err := s.read.QueryRowContext(ctx, `SELECT id,account_id,name,COALESCE(catch_all_inbox_id,''),COALESCE(outbound_credential_id,''),COALESCE(inbound_credential_id,''),created_at FROM domains WHERE id=? AND account_id=?`, domainID, accountID).Scan(&d.ID, &d.AccountID, &d.Name, &d.CatchAllInboxID, &d.OutboundCredentialID, &d.InboundCredentialID, &c)
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

// SetDomainOutboundCredential assigns a domain's outbound provider. An empty id
// clears it, so the domain queues mail until a provider is assigned.
func (s *Store) SetDomainOutboundCredential(ctx context.Context, accountID, domainID, credentialID string) error {
	if credentialID != "" {
		var n int
		if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM outbound_credentials WHERE id=? AND account_id=?`, credentialID, accountID).Scan(&n); err != nil || n != 1 {
			return ErrForbidden
		}
	}
	res, err := s.write.ExecContext(ctx, `UPDATE domains SET outbound_credential_id=? WHERE id=? AND account_id=?`, nullString(credentialID), domainID, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
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
	id := idgen.New("in")
	now := nowText()
	_, err := s.write.ExecContext(ctx, `INSERT INTO inboxes(id,account_id,domain_id,local_part,display_name,created_at) VALUES(?,?,?,?,?,?)`, id, accountID, domainID, localPart, strings.TrimSpace(display), now)
	if err != nil {
		return model.Inbox{}, err
	}
	return model.Inbox{ID: id, AccountID: accountID, DomainID: domainID, LocalPart: localPart, Address: addr, DisplayName: display, Enabled: true, CreatedAt: parseTime(now)}, nil
}
func (s *Store) ListInboxes(ctx context.Context, p model.Principal) ([]model.Inbox, error) {
	q := `SELECT i.id,i.account_id,i.domain_id,i.local_part,d.name,i.display_name,i.enabled,i.allowed_senders_json,i.created_at FROM inboxes i JOIN domains d ON d.id=i.domain_id WHERE i.account_id=?`
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
	var out []model.Inbox
	for rows.Next() {
		var i model.Inbox
		var domain, allowed, created string
		var enabled int
		if err = rows.Scan(&i.ID, &i.AccountID, &i.DomainID, &i.LocalPart, &domain, &i.DisplayName, &enabled, &allowed, &created); err != nil {
			return nil, err
		}
		i.Address = i.LocalPart + "@" + domain
		i.Enabled = enabled != 0
		i.AllowedSenders = decodeStrings(allowed)
		i.CreatedAt = parseTime(created)
		out = append(out, i)
	}
	return out, rows.Err()
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
	var enabled int
	err := s.read.QueryRowContext(ctx, `SELECT i.id,i.account_id,i.domain_id,i.local_part,d.name,i.display_name,i.enabled,i.allowed_senders_json,i.created_at FROM inboxes i JOIN domains d ON d.id=i.domain_id WHERE i.id=? AND i.account_id=?`, id, accountID).Scan(&i.ID, &i.AccountID, &i.DomainID, &i.LocalPart, &domain, &i.DisplayName, &enabled, &allowed, &created)
	if err == sql.ErrNoRows {
		return i, ErrNotFound
	}
	if err != nil {
		return i, err
	}
	i.Address = i.LocalPart + "@" + domain
	i.Enabled = enabled != 0
	i.AllowedSenders = decodeStrings(allowed)
	i.CreatedAt = parseTime(created)
	return i, nil
}
func (s *Store) UpdateInbox(ctx context.Context, p model.Principal, id, display string, enabled *bool) error {
	if !p.CanOwn(id) && !p.Admin {
		return ErrForbidden
	}
	if display != "" {
		_, _ = s.write.ExecContext(ctx, `UPDATE inboxes SET display_name=? WHERE id=? AND account_id=?`, strings.TrimSpace(display), id, p.AccountID)
	}
	if enabled != nil {
		_, _ = s.write.ExecContext(ctx, `UPDATE inboxes SET enabled=? WHERE id=? AND account_id=?`, boolInt(*enabled), id, p.AccountID)
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

func (s *Store) ResolveRecipient(ctx context.Context, address string) (model.Inbox, bool, error) {
	address = normalizeAddress(address)
	parts := strings.Split(address, "@")
	if len(parts) != 2 {
		return model.Inbox{}, false, ErrNotFound
	}
	local, domain := parts[0], parts[1]
	var accountID, domainID, domainName, catch string
	err := s.read.QueryRowContext(ctx, `SELECT account_id,id,name,COALESCE(catch_all_inbox_id,'') FROM domains WHERE name=?`, domain).Scan(&accountID, &domainID, &domainName, &catch)
	if err == sql.ErrNoRows {
		return model.Inbox{}, false, ErrNotFound
	}
	if err != nil {
		return model.Inbox{}, false, err
	}
	var id, actualLocal, display, allowed, created string
	var enabled int
	err = s.read.QueryRowContext(ctx, `SELECT id,local_part,display_name,enabled,allowed_senders_json,created_at FROM inboxes WHERE domain_id=? AND local_part=?`, domainID, local).Scan(&id, &actualLocal, &display, &enabled, &allowed, &created)
	usedCatch := false
	if err == sql.ErrNoRows && catch != "" {
		err = s.read.QueryRowContext(ctx, `SELECT id,local_part,display_name,enabled,allowed_senders_json,created_at FROM inboxes WHERE id=? AND domain_id=?`, catch, domainID).Scan(&id, &actualLocal, &display, &enabled, &allowed, &created)
		usedCatch = true
	}
	if err == sql.ErrNoRows {
		return model.Inbox{}, false, ErrNotFound
	}
	if err != nil {
		return model.Inbox{}, false, err
	}
	if enabled == 0 {
		return model.Inbox{}, usedCatch, ErrNotFound
	}
	return model.Inbox{ID: id, AccountID: accountID, DomainID: domainID, LocalPart: actualLocal, Address: actualLocal + "@" + domainName, DisplayName: display, Enabled: true, AllowedSenders: decodeStrings(allowed), CreatedAt: parseTime(created)}, usedCatch, nil
}
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}
