package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/idgen"
	"github.com/dellarb/mailmoose/internal/model"
)

func (s *Store) GetAccount(ctx context.Context, accountID string) (model.Account, error) {
	var a model.Account
	var created string
	err := s.read.QueryRowContext(ctx, `SELECT id,name,storage_quota_bytes,storage_used_bytes,created_at,timezone FROM accounts WHERE id=?`, accountID).Scan(&a.ID, &a.Name, &a.StorageQuotaBytes, &a.StorageUsedBytes, &created, &a.Timezone)
	if err == sql.ErrNoRows {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.CreatedAt = parseTime(created)
	return a, nil
}

// DomainCreateOptions controls subdomain inheritance when a domain is created.
// Inheritance only ever applies to a detected subdomain (a name that is a
// proper label-suffix of an existing domain in the same account); a root domain
// ignores it.
type DomainCreateOptions struct {
	// DisableReceiving/DisableSending turn off the default inheritance of the
	// parent's receiving/sending configuration. By default a detected subdomain
	// inherits both, so one receiver serves the whole zone; the operator can opt
	// out per slot.
	DisableReceiving bool
	DisableSending   bool
	// ParentDomainID overrides parent auto-detection when non-empty. It must
	// name a domain in the same account that is a proper suffix of the new name.
	ParentDomainID string
}

// CreateDomain creates a root domain or a subdomain. When the name is a
// subdomain of an existing domain in the account, the nearest such ancestor is
// recorded and the new domain inherits its receiving and sending configuration
// by default; see DomainCreateOptions to opt out per slot.
func (s *Store) CreateDomain(ctx context.Context, accountID, name string) (model.Domain, error) {
	return s.CreateDomainWithOptions(ctx, accountID, name, DomainCreateOptions{})
}

// CreateDomainWithOptions is CreateDomain with explicit inheritance control.
func (s *Store) CreateDomainWithOptions(ctx context.Context, accountID, name string, opts DomainCreateOptions) (model.Domain, error) {
	name = normalizeDomain(name)
	if name == "" || !strings.Contains(name, ".") {
		return model.Domain{}, fmt.Errorf("valid domain required")
	}
	parentID := strings.TrimSpace(opts.ParentDomainID)
	if parentID == "" {
		parentID, _ = s.nearestAncestorDomain(ctx, accountID, name)
	}
	if parentID != "" {
		if _, err := s.validateParentDomain(ctx, accountID, "", name, parentID); err != nil {
			return model.Domain{}, err
		}
	}
	inheritReceiving, inheritSending := false, false
	if parentID != "" {
		inheritReceiving = !opts.DisableReceiving
		inheritSending = !opts.DisableSending
	}
	id := idgen.New("dom")
	now := nowText()
	_, err := s.write.ExecContext(ctx, `INSERT INTO domains(id,account_id,name,parent_domain_id,inherit_receiving,inherit_sending,created_at) VALUES(?,?,?,?,?,?,?)`,
		id, accountID, name, nullString(parentID), boolInt(inheritReceiving), boolInt(inheritSending), now)
	if err != nil {
		return model.Domain{}, err
	}
	// Re-read through GetDomain so the returned view carries the effective
	// (possibly inherited) providers and the parent name.
	return s.GetDomain(ctx, accountID, id)
}

// isSubdomainOf reports whether child is a proper label-suffix of parent (for
// example agent.example.com under example.com), never equal to it.
func isSubdomainOf(child, parent string) bool {
	child = normalizeDomain(child)
	parent = normalizeDomain(parent)
	if child == "" || parent == "" || child == parent {
		return false
	}
	return strings.HasSuffix(child, "."+parent)
}

// nearestAncestorDomain returns the id and name of the longest existing ancestor
// domain of name within the account (its immediate parent if configured, else
// the next suffix up), or ("","") when none exists. Only same-account domains
// are considered, so a subdomain never inherits from another account's domain.
func (s *Store) nearestAncestorDomain(ctx context.Context, accountID, name string) (string, string) {
	labels := strings.Split(normalizeDomain(name), ".")
	for i := 1; i < len(labels); i++ {
		candidate := strings.Join(labels[i:], ".")
		if candidate == "" {
			continue
		}
		var id string
		if err := s.read.QueryRowContext(ctx, `SELECT id FROM domains WHERE account_id=? AND name=?`, accountID, candidate).Scan(&id); err == nil {
			return id, candidate
		}
	}
	return "", ""
}

// domainName returns the stored (normalized) name of a domain in the account.
func (s *Store) domainName(ctx context.Context, accountID, domainID string) (string, error) {
	var name string
	err := s.read.QueryRowContext(ctx, `SELECT name FROM domains WHERE id=? AND account_id=?`, domainID, accountID).Scan(&name)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	return name, err
}

const domainSummarySelect = `SELECT d.id,d.account_id,d.name,COALESCE(d.catch_all_inbox_id,''),COALESCE(d.parent_domain_id,''),d.inherit_receiving,d.inherit_sending,COALESCE(sc.provider,''),COALESCE(rc.provider,''),d.created_at
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
		var inheritRecv, inheritSend int
		if err = rows.Scan(&d.ID, &d.AccountID, &d.Name, &d.CatchAllInboxID, &d.ParentDomainID, &inheritRecv, &inheritSend, &d.SendingProvider, &d.ReceivingProvider, &c); err != nil {
			return nil, err
		}
		d.InheritReceiving = inheritRecv != 0
		d.InheritSending = inheritSend != 0
		d.CreatedAt = parseTime(c)
		out = append(out, d)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].ParentDomainID != "" {
			if name, perr := s.domainName(ctx, accountID, out[i].ParentDomainID); perr == nil {
				out[i].ParentDomain = name
			}
		}
		s.applyInheritedProviders(ctx, accountID, &out[i])
	}
	return out, nil
}

func (s *Store) GetDomain(ctx context.Context, accountID, domainID string) (model.Domain, error) {
	var d model.Domain
	var c string
	var inheritRecv, inheritSend int
	err := s.read.QueryRowContext(ctx, domainSummarySelect+` WHERE d.id=? AND d.account_id=?`, domainID, accountID).Scan(&d.ID, &d.AccountID, &d.Name, &d.CatchAllInboxID, &d.ParentDomainID, &inheritRecv, &inheritSend, &d.SendingProvider, &d.ReceivingProvider, &c)
	if err == sql.ErrNoRows {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	d.InheritReceiving = inheritRecv != 0
	d.InheritSending = inheritSend != 0
	d.CreatedAt = parseTime(c)
	if d.ParentDomainID != "" {
		if name, perr := s.domainName(ctx, accountID, d.ParentDomainID); perr == nil {
			d.ParentDomain = name
		}
	}
	s.applyInheritedProviders(ctx, accountID, &d)
	return d, nil
}

// applyInheritedProviders fills a domain's effective sending/receiving provider
// from the nearest ancestor when it has none of its own and inheritance is on,
// recording the ancestor's name in SendingInheritedFrom/ReceivingInheritedFrom.
func (s *Store) applyInheritedProviders(ctx context.Context, accountID string, d *model.Domain) {
	if d.ParentDomainID == "" {
		return
	}
	if d.SendingProvider == "" && d.InheritSending {
		if p, from := s.effectiveProvider(ctx, accountID, d.ID, true); p != "" {
			d.SendingProvider = p
			d.SendingInheritedFrom = from
		}
	}
	if d.ReceivingProvider == "" && d.InheritReceiving {
		if p, from := s.effectiveProvider(ctx, accountID, d.ID, false); p != "" {
			d.ReceivingProvider = p
			d.ReceivingInheritedFrom = from
		}
	}
}

// SetDomainInheritance updates whether a subdomain inherits its parent's
// receiving and/or sending configuration. It is a no-op for the flags on a root
// domain (which has no parent to inherit from).
func (s *Store) SetDomainInheritance(ctx context.Context, accountID, domainID string, inheritReceiving, inheritSending bool) error {
	res, err := s.write.ExecContext(ctx, `UPDATE domains SET inherit_receiving=?, inherit_sending=? WHERE id=? AND account_id=?`, boolInt(inheritReceiving), boolInt(inheritSending), domainID, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDomainInheritFlag updates a single inheritance switch for a domain,
// leaving the other untouched. sending selects inherit_sending; otherwise
// inherit_receiving is updated.
func (s *Store) SetDomainInheritFlag(ctx context.Context, accountID, domainID string, sending, inherit bool) error {
	column := "inherit_receiving"
	if sending {
		column = "inherit_sending"
	}
	res, err := s.write.ExecContext(ctx, `UPDATE domains SET `+column+`=? WHERE id=? AND account_id=?`, boolInt(inherit), domainID, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// validateParentDomain checks that parentID names a domain in the same account
// that is a proper label-suffix ancestor of childName, returning the parent's
// name. When childID is non-empty it rejects a parent that is the child itself
// or one of the child's own descendants, and a link that would create a cycle.
func (s *Store) validateParentDomain(ctx context.Context, accountID, childID, childName, parentID string) (string, error) {
	var parentName string
	err := s.read.QueryRowContext(ctx, `SELECT name FROM domains WHERE id=? AND account_id=?`, parentID, accountID).Scan(&parentName)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("parent domain not found")
	}
	if err != nil {
		return "", err
	}
	if childID != "" && parentID == childID {
		return "", fmt.Errorf("a domain cannot be its own parent")
	}
	if !isSubdomainOf(childName, parentName) {
		return "", fmt.Errorf("%s is not a subdomain of %s", normalizeDomain(childName), parentName)
	}
	// Walking up from the proposed parent must never reach the child, which
	// would form a cycle.
	id := parentID
	for depth := 0; depth < maxDomainAncestorDepth; depth++ {
		if id == childID && childID != "" {
			return "", fmt.Errorf("linking %s to %s would create a cycle", normalizeDomain(childName), parentName)
		}
		st, err := s.domainInheritanceState(ctx, accountID, id)
		if err != nil {
			// A parent without its own row (or a broken chain) simply ends the walk.
			return parentName, nil
		}
		if st.ParentID == "" {
			return parentName, nil
		}
		id = st.ParentID
	}
	return "", fmt.Errorf("linking %s to %s would create a cycle", normalizeDomain(childName), parentName)
}

// SetDomainParent links a root domain to an ancestor added later, or unlinks it.
// parentID must name a proper suffix ancestor of the domain in the same account;
// an empty parentID unlinks the domain and clears both inherit flags. Linking
// turns both inheritance switches on (matching the create-time default); the
// operator can opt out per slot with SetDomainInheritFlag afterwards. No parent
// configuration is copied - it is resolved at read time.
func (s *Store) SetDomainParent(ctx context.Context, accountID, domainID, parentID string) error {
	name, err := s.domainName(ctx, accountID, domainID)
	if err != nil {
		return err
	}
	parentID = strings.TrimSpace(parentID)
	if parentID == "" {
		res, err := s.write.ExecContext(ctx, `UPDATE domains SET parent_domain_id=NULL, inherit_receiving=0, inherit_sending=0 WHERE id=? AND account_id=?`, domainID, accountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	}
	if _, err := s.validateParentDomain(ctx, accountID, domainID, name, parentID); err != nil {
		return err
	}
	res, err := s.write.ExecContext(ctx, `UPDATE domains SET parent_domain_id=?, inherit_receiving=1, inherit_sending=1 WHERE id=? AND account_id=?`, parentID, domainID, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// InheritableAncestors returns the candidate parent domains for a domain: every
// other domain in the same account whose name is a proper label-suffix ancestor
// of this domain's name. It is used to offer a manual parent link when a parent
// is added after the subdomain already exists.
func (s *Store) InheritableAncestors(ctx context.Context, accountID, domainID string) ([]model.Domain, error) {
	name, err := s.domainName(ctx, accountID, domainID)
	if err != nil {
		return nil, err
	}
	all, err := s.ListDomains(ctx, accountID)
	if err != nil {
		return nil, err
	}
	labels := strings.Split(normalizeDomain(name), ".")
	out := []model.Domain{}
	for _, d := range all {
		if d.ID == domainID {
			continue
		}
		for i := 1; i < len(labels); i++ {
			if strings.Join(labels[i:], ".") == normalizeDomain(d.Name) {
				out = append(out, d)
				break
			}
		}
	}
	return out, nil
}

// SetDomainCatchAll sets or clears the catch-all inbox for a domain.
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
	if _, err = tx.ExecContext(ctx, `DELETE FROM inbound_delivery_log WHERE account_id=? AND domain_id=?`, accountID, domainID); err != nil {
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
	if _, err = tx.ExecContext(ctx, `DELETE FROM clients WHERE id IN (SELECT p.client_id FROM client_push p JOIN clients c ON c.id=p.client_id WHERE p.inbox_id IN (SELECT id FROM inboxes WHERE account_id=? AND domain_id=?) AND c.type IN ('hermes','openclaw','webhook'))`, accountID, domainID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM hermes_enroll_tokens WHERE inbox_id IN (SELECT id FROM inboxes WHERE account_id=? AND domain_id=?)`, accountID, domainID); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM client_inbox_bindings WHERE inbox_id IN (SELECT id FROM inboxes WHERE account_id=? AND domain_id=?)`, accountID, domainID); err != nil {
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
	_, err := s.write.ExecContext(ctx, `INSERT INTO inboxes(id,account_id,domain_id,local_part,display_name,delivery_trigger,storage_used_bytes,created_at) VALUES(?,?,?,?,?,?,0,?)`, id, accountID, domainID, localPart, strings.TrimSpace(display), DeliveryTriggerDefault, now)
	if err != nil {
		return model.Inbox{}, err
	}
	return model.Inbox{ID: id, AccountID: accountID, Kind: model.InboxKindDomain, DomainID: domainID, LocalPart: localPart, Address: addr, DisplayName: display, Enabled: true, DeliveryTrigger: DeliveryTriggerDefault, CreatedAt: parseTime(now)}, nil
}

// fullInboxSelectCols is the projection every full-inbox read shares. It uses
// the domain name only for a domain inbox; a standalone inbox carries its own
// address, so the domain join is a LEFT JOIN and d.name may be NULL.
const fullInboxSelectCols = `i.id,i.account_id,i.kind,i.domain_id,i.local_part,COALESCE(d.name,''),i.address,i.display_name,i.enabled,i.allowed_senders_json,i.sender_restricted,i.require_authenticated,i.approver_email,i.default_sender,i.trash_retention_days,i.auto_mark_read_on_delivery,i.auto_trash_after_delivery_hours,i.delivery_trigger,i.created_at,i.storage_quota_bytes,i.storage_used_bytes,i.namespace,i.remote_host,i.remote_port,i.remote_username,i.remote_security,i.smtp_host,i.smtp_port,i.smtp_username,i.smtp_security,i.remote_configured,i.remote_sent_copy_enabled,i.remote_sent_copy_folder,i.remote_poll_seconds,i.remote_full_sync_minutes`

// scanFullInbox reads one row projected by fullInboxSelectCols. It resolves the
// address and remote description and never queries further tables. storageKnown
// reports whether the storage_used_bytes counter is populated (a NULL counter
// means the inbox predates migration 047 and must be recomputed on read).
func scanFullInbox(row interface{ Scan(...any) error }) (model.Inbox, bool, error) {
	var i model.Inbox
	var domain, allowed, created, ns, rhost, ruser, rsec, shost, suser, ssec, sentCopyFolder string
	var enabled, restricted, requireAuth, autoMarkRead, remoteConfigured, sentCopyEnabled int
	var rport, sport int
	var domainID sql.NullString
	var trashRetention, autoTrashHours, storageQuota, storageUsed sql.NullInt64
	var remotePollSeconds, remoteFullSyncMinutes sql.NullInt64
	if err := row.Scan(&i.ID, &i.AccountID, &i.Kind, &domainID, &i.LocalPart, &domain, &i.Address, &i.DisplayName, &enabled, &allowed, &restricted, &requireAuth, &i.ApproverEmail, &i.DefaultSender, &trashRetention, &autoMarkRead, &autoTrashHours, &i.DeliveryTrigger, &created, &storageQuota, &storageUsed, &ns, &rhost, &rport, &ruser, &rsec, &shost, &sport, &suser, &ssec, &remoteConfigured, &sentCopyEnabled, &sentCopyFolder, &remotePollSeconds, &remoteFullSyncMinutes); err != nil {
		return model.Inbox{}, false, err
	}
	i.DomainID = domainID.String
	if i.Kind == model.InboxKindStandalone {
		if i.Address == "" && domain != "" && i.LocalPart != "" {
			i.Address = i.LocalPart + "@" + domain
		}
	} else {
		i.Address = i.LocalPart + "@" + domain
	}
	i.Enabled = enabled != 0
	i.AllowedSenders = decodeStrings(allowed)
	i.SenderRestricted = restricted != 0
	i.RequireAuthenticated = requireAuth != 0
	i.AutoMarkReadOnDelivery = autoMarkRead != 0
	i.Namespace = ns
	i.RemoteConfigured = remoteConfigured != 0
	i.RemoteSentCopyEnabled = sentCopyEnabled != 0
	i.RemoteSentCopyFolder = sentCopyFolder
	if rhost != "" || ruser != "" || rport != 0 {
		rc := &model.RemoteConnection{Host: rhost, Port: rport, Username: ruser, Security: rsec}
		if shost != "" || suser != "" || sport != 0 {
			rc.SMTP = &model.RemoteSMTP{Host: shost, Port: sport, Username: suser, Security: ssec}
		}
		i.Remote = rc
	}
	if trashRetention.Valid {
		days := int(trashRetention.Int64)
		i.TrashRetentionDays = &days
	}
	if storageQuota.Valid {
		quota := storageQuota.Int64
		i.StorageQuotaBytes = &quota
	}
	if storageUsed.Valid {
		i.StorageUsedBytes = storageUsed.Int64
	}
	if autoTrashHours.Valid {
		hours := int(autoTrashHours.Int64)
		i.AutoTrashAfterDeliveryHours = &hours
	}
	if remotePollSeconds.Valid {
		v := int(remotePollSeconds.Int64)
		i.RemotePollSeconds = &v
	}
	if remoteFullSyncMinutes.Valid {
		v := int(remoteFullSyncMinutes.Int64)
		i.RemoteFullSyncMinutes = &v
	}
	i.CreatedAt = parseTime(created)
	return i, storageUsed.Valid, nil
}

func (s *Store) ListInboxes(ctx context.Context, p model.Principal) ([]model.Inbox, error) {
	q := `SELECT ` + fullInboxSelectCols + ` FROM inboxes i LEFT JOIN domains d ON d.id=i.domain_id WHERE i.account_id=?`
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
	// Standalone inboxes have no managed domain; sort them after the domain
	// inboxes, then by address.
	q += ` ORDER BY (i.kind='standalone'), d.name, i.address, i.local_part`
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Inbox{}
	inboxInitialized := map[string]bool{}
	for rows.Next() {
		i, storageKnown, scanErr := scanFullInbox(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if storageKnown {
			inboxInitialized[i.ID] = true
		}
		out = append(out, i)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		// A NULL counter means the inbox predates migration 047 and has not been
		// written since. Compute its live usage for display without persisting,
		// so the advertised number already equals what enforcement would use.
		if !inboxInitialized[out[i].ID] {
			used, uerr := s.recomputeInboxStorage(ctx, p.AccountID, out[i].ID)
			if uerr != nil {
				return nil, uerr
			}
			out[i].StorageUsedBytes = used
		}
	}
	if len(out) > 0 {
		aliases, aerr := s.ListInboxAliases(ctx, p.AccountID)
		if aerr != nil {
			return nil, aerr
		}
		for i := range out {
			out[i].Aliases = aliasAddresses(aliases[out[i].ID])
			out[i].AliasNames = aliasNames(aliases[out[i].ID])
		}
	}
	for n := range out {
		if out[n].Kind == model.InboxKindStandalone && s.IsGoogle(ctx, p.AccountID, out[n].ID) {
			caps := model.StandaloneCapabilities()
			caps.HierarchicalFolders = false
			caps.Outbound = true
			out[n].Capabilities = &caps
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
	i, storageKnown, err := scanFullInbox(s.read.QueryRowContext(ctx, `SELECT `+fullInboxSelectCols+` FROM inboxes i LEFT JOIN domains d ON d.id=i.domain_id WHERE i.id=? AND i.account_id=?`, id, accountID))
	if err == sql.ErrNoRows {
		return i, ErrNotFound
	}
	if err != nil {
		return i, err
	}
	if !storageKnown {
		// A pre-migration inbox: show live usage without persisting.
		used, uerr := s.recomputeInboxStorage(ctx, accountID, id)
		if uerr != nil {
			return i, uerr
		}
		i.StorageUsedBytes = used
	}
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

// SetInboxTrashRetention sets an inbox's Trash auto-purge override. A nil days
// clears the override so the inbox inherits its account's trash_retention_days;
// 0 keeps this inbox's trashed mail until purged by hand; a positive value
// purges after that many days.
func (s *Store) SetInboxTrashRetention(ctx context.Context, accountID, inboxID string, days *int) error {
	if days != nil && *days < 0 {
		return fmt.Errorf("trash retention must be zero or positive")
	}
	var value any
	if days != nil {
		value = *days
	}
	res, err := s.write.ExecContext(ctx, `UPDATE inboxes SET trash_retention_days=? WHERE id=? AND account_id=?`, value, inboxID, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetInboxRemoteSync sets a standalone inbox's remote sync cadence overrides. A
// nil pointer clears the matching override so the inbox inherits the process
// default; a positive value is seconds (quick poll) or minutes (full sync). A
// non-positive value clears the override rather than storing it, so a bogus
// zero cannot reach the clamp path.
func (s *Store) SetInboxRemoteSync(ctx context.Context, accountID, inboxID string, pollSeconds, fullSyncMinutes *int) error {
	var poll, full any
	if pollSeconds != nil && *pollSeconds > 0 {
		poll = *pollSeconds
	}
	if fullSyncMinutes != nil && *fullSyncMinutes > 0 {
		full = *fullSyncMinutes
	}
	res, err := s.write.ExecContext(ctx, `UPDATE inboxes SET remote_poll_seconds=?,remote_full_sync_minutes=? WHERE id=? AND account_id=?`, poll, full, inboxID, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetInboxStorageQuota sets an inbox's storage cap. A nil quota clears the cap
// so only the account quota applies; 0 means explicitly unlimited for this
// inbox; a positive value is the cap in bytes. A negative value is rejected.
// The cap may be set below current usage: existing mail is untouched and new
// writes are refused with ErrQuota until usage falls.
func (s *Store) SetInboxStorageQuota(ctx context.Context, accountID, inboxID string, quota *int64) error {
	if quota != nil && *quota < 0 {
		return fmt.Errorf("storage quota cannot be negative")
	}
	var value any
	if quota != nil {
		value = *quota
	}
	res, err := s.write.ExecContext(ctx, `UPDATE inboxes SET storage_quota_bytes=? WHERE id=? AND account_id=?`, value, inboxID, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// InboxStorageUsed returns an inbox's current storage usage in bytes. It reads
// the maintained counter, falling back to a live SUM (messages, drafts and
// draft attachments) for an inbox whose counter has not yet been initialized by
// a write, so the display is correct for pre-migration data.
func (s *Store) InboxStorageUsed(ctx context.Context, accountID, inboxID string) (int64, error) {
	var used sql.NullInt64
	if err := s.read.QueryRowContext(ctx, `SELECT storage_used_bytes FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&used); err != nil {
		if err == sql.ErrNoRows {
			return 0, ErrNotFound
		}
		return 0, err
	}
	if used.Valid {
		return used.Int64, nil
	}
	return s.recomputeInboxStorage(ctx, accountID, inboxID)
}

// recomputeInboxStorage sums every byte attributable to an inbox: its message
// rows (all directions and states), its draft bodies and its draft attachment
// files. It is the read-side equivalent of recomputeInboxStorageTx.
func (s *Store) recomputeInboxStorage(ctx context.Context, accountID, inboxID string) (int64, error) {
	var total int64
	if err := s.read.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes),0) FROM messages WHERE account_id=? AND inbox_id=?`, accountID, inboxID).Scan(&total); err != nil {
		return 0, err
	}
	var draftBodies int64
	if err := s.read.QueryRowContext(ctx, `SELECT COALESCE(SUM(LENGTH(CAST(text_body AS BLOB))+LENGTH(CAST(html_body AS BLOB))),0) FROM drafts WHERE account_id=? AND inbox_id=?`, accountID, inboxID).Scan(&draftBodies); err != nil {
		return 0, err
	}
	var draftAtts int64
	if err := s.read.QueryRowContext(ctx, `SELECT COALESCE(SUM(da.size_bytes),0) FROM draft_attachments da JOIN drafts d ON d.id=da.draft_id WHERE d.account_id=? AND d.inbox_id=?`, accountID, inboxID).Scan(&draftAtts); err != nil {
		return 0, err
	}
	return total + draftBodies + draftAtts, nil
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
		if len(seen) > maxInboxAliases {
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
// identity and the id of the domain whose sending configuration must be used.
func resolveSenderQuery(ctx context.Context, q senderQueryer, accountID, inboxID, requested string) (model.Address, string, error) {
	from, target, err := resolveSendingTargetQuery(ctx, q, accountID, inboxID, requested)
	return from, target.DomainID, err
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
	if _, err = tx.ExecContext(ctx, `DELETE FROM inbound_delivery_log WHERE account_id=? AND inbox_id=?`, accountID, id); err != nil {
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
	// Delete the inbox's relay/webhook clients (and their enroll tokens and
	// bindings) before the inbox row goes, mirroring PurgeDomain. client_push
	// cascades on inbox delete, but the parent clients rows would otherwise be
	// orphaned.
	if _, err = tx.ExecContext(ctx, `DELETE FROM clients WHERE id IN (SELECT p.client_id FROM client_push p JOIN clients c ON c.id=p.client_id WHERE p.inbox_id=? AND c.type IN ('hermes','openclaw','webhook'))`, id); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM hermes_enroll_tokens WHERE inbox_id=?`, id); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM client_inbox_bindings WHERE inbox_id=?`, id); err != nil {
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
	i.Kind = model.InboxKindDomain
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
