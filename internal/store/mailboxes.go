package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/mail"
	"strings"

	"github.com/open-agent-inbox/open-agent-inbox/internal/idgen"
	"github.com/open-agent-inbox/open-agent-inbox/internal/model"
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
	rows, err := s.read.QueryContext(ctx, `SELECT id,account_id,name,COALESCE(catch_all_inbox_id,''),created_at FROM domains WHERE account_id=? ORDER BY name`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Domain
	for rows.Next() {
		var d model.Domain
		var c string
		if err = rows.Scan(&d.ID, &d.AccountID, &d.Name, &d.CatchAllInboxID, &c); err != nil {
			return nil, err
		}
		d.CreatedAt = parseTime(c)
		out = append(out, d)
	}
	return out, rows.Err()
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
	q := `SELECT i.id,i.account_id,i.domain_id,i.local_part,d.name,i.display_name,i.enabled,COALESCE(i.outbound_credential_id,''),i.created_at FROM inboxes i JOIN domains d ON d.id=i.domain_id WHERE i.account_id=?`
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
		var domain, created string
		var enabled int
		if err = rows.Scan(&i.ID, &i.AccountID, &i.DomainID, &i.LocalPart, &domain, &i.DisplayName, &enabled, &i.OutboundCredentialID, &created); err != nil {
			return nil, err
		}
		i.Address = i.LocalPart + "@" + domain
		i.Enabled = enabled != 0
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
	var domain, created string
	var enabled int
	err := s.read.QueryRowContext(ctx, `SELECT i.id,i.account_id,i.domain_id,i.local_part,d.name,i.display_name,i.enabled,COALESCE(i.outbound_credential_id,''),i.created_at FROM inboxes i JOIN domains d ON d.id=i.domain_id WHERE i.id=? AND i.account_id=?`, id, accountID).Scan(&i.ID, &i.AccountID, &i.DomainID, &i.LocalPart, &domain, &i.DisplayName, &enabled, &i.OutboundCredentialID, &created)
	if err == sql.ErrNoRows {
		return i, ErrNotFound
	}
	if err != nil {
		return i, err
	}
	i.Address = i.LocalPart + "@" + domain
	i.Enabled = enabled != 0
	i.CreatedAt = parseTime(created)
	return i, nil
}
func (s *Store) UpdateInbox(ctx context.Context, p model.Principal, id, display string, enabled *bool, outboundCredID *string) error {
	if !p.CanOwn(id) && !p.Admin {
		return ErrForbidden
	}
	if display != "" {
		_, _ = s.write.ExecContext(ctx, `UPDATE inboxes SET display_name=? WHERE id=? AND account_id=?`, strings.TrimSpace(display), id, p.AccountID)
	}
	if enabled != nil {
		_, _ = s.write.ExecContext(ctx, `UPDATE inboxes SET enabled=? WHERE id=? AND account_id=?`, boolInt(*enabled), id, p.AccountID)
	}
	if outboundCredID != nil {
		if *outboundCredID != "" {
			var n int
			if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM outbound_credentials WHERE id=? AND account_id=?`, *outboundCredID, p.AccountID).Scan(&n); err != nil || n != 1 {
				return ErrForbidden
			}
		}
		_, _ = s.write.ExecContext(ctx, `UPDATE inboxes SET outbound_credential_id=? WHERE id=? AND account_id=?`, nullString(*outboundCredID), id, p.AccountID)
	}
	return nil
}
func (s *Store) DeleteInbox(ctx context.Context, accountID, id string) error {
	res, err := s.write.ExecContext(ctx, `DELETE FROM inboxes WHERE id=? AND account_id=?`, id, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
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
	var id, actualLocal, display, cred, created string
	var enabled int
	err = s.read.QueryRowContext(ctx, `SELECT id,local_part,display_name,enabled,COALESCE(outbound_credential_id,''),created_at FROM inboxes WHERE domain_id=? AND local_part=?`, domainID, local).Scan(&id, &actualLocal, &display, &enabled, &cred, &created)
	usedCatch := false
	if err == sql.ErrNoRows && catch != "" {
		err = s.read.QueryRowContext(ctx, `SELECT id,local_part,display_name,enabled,COALESCE(outbound_credential_id,''),created_at FROM inboxes WHERE id=? AND domain_id=?`, catch, domainID).Scan(&id, &actualLocal, &display, &enabled, &cred, &created)
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
	return model.Inbox{ID: id, AccountID: accountID, DomainID: domainID, LocalPart: actualLocal, Address: actualLocal + "@" + domainName, DisplayName: display, Enabled: true, OutboundCredentialID: cred, CreatedAt: parseTime(created)}, usedCatch, nil
}
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}
