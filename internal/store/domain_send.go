package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// migration074 adds the per-key "domain-wide sending" grant. The grant lives on
// a key's binding row for a domain's catch-all inbox: when the flag is set the
// key may put ANY local part on that domain in the From address, not only the
// inbox primary or a managed alias. The inbox is the anchor because the
// catch-all is what makes an arbitrary address reployable — the check is
// evaluated against the domain's current, enabled catch-all at send time, so the
// flag goes dormant (not deleted) if the catch-all is cleared or moved.
//
// The flag is additive and defaults to 0, so every existing binding is unchanged.
const migration074 = `
ALTER TABLE client_inbox_bindings ADD COLUMN send_any_in_domain INTEGER NOT NULL DEFAULT 0;
`

// ErrNoCatchAll reports that a domain has no enabled catch-all inbox, so a
// domain-wide send grant cannot apply.
var ErrNoCatchAll = errors.New("domain has no enabled catch-all")

// senderResolver is the row-read surface both the read pool and an open
// transaction satisfy.
type senderResolver interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// domainSendGrantAt reports whether clientID holds the domain-wide sending grant
// on the given inbox AND that inbox is currently the domain's enabled catch-all.
// It is the single live-condition check for the feature: the flag alone is not
// enough, the anchor must still be the catch-all.
//
// An empty clientID (a web session, system mail, or the principal-free approval
// path) never holds the grant, so those callers keep the exact-primary/alias
// rule.
func domainSendGrantAt(ctx context.Context, q senderResolver, clientID, inboxID string) (bool, error) {
	if clientID == "" || inboxID == "" {
		return false, nil
	}
	var granted int
	err := q.QueryRowContext(ctx, `SELECT b.send_any_in_domain FROM client_inbox_bindings b WHERE b.client_id=? AND b.inbox_id=?`, clientID, inboxID).Scan(&granted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if granted == 0 {
		return false, nil
	}
	// The flag is only live while this inbox is the domain's current, enabled
	// catch-all. A cleared, moved or disabled catch-all makes it dormant without
	// touching the stored flag.
	var enabled int
	err = q.QueryRowContext(ctx, `SELECT i.enabled FROM inboxes i JOIN domains d ON d.id=i.domain_id WHERE i.id=? AND d.catch_all_inbox_id=i.id`, inboxID).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return enabled != 0, nil
}

// DomainSendGrant describes the domain-wide sending grant on one binding row.
type DomainSendGrant struct {
	InboxID string
	Domain  string
	Enabled bool
}

// ListDomainSendGrants returns the bindings of one key that carry the
// domain-wide sending grant, with the domain that each is anchored to and
// whether the grant is currently live (the inbox is still the enabled
// catch-all). It backs the Clients & Access read path.
func (s *Store) ListDomainSendGrants(ctx context.Context, clientID string) ([]DomainSendGrant, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT b.inbox_id,COALESCE(d.name,''),b.send_any_in_domain,CASE WHEN d.catch_all_inbox_id=b.inbox_id AND i.enabled=1 THEN 1 ELSE 0 END FROM client_inbox_bindings b JOIN inboxes i ON i.id=b.inbox_id LEFT JOIN domains d ON d.id=i.domain_id WHERE b.client_id=? AND b.send_any_in_domain=1`, clientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DomainSendGrant
	for rows.Next() {
		var g DomainSendGrant
		var flag, live int
		if err := rows.Scan(&g.InboxID, &g.Domain, &flag, &live); err != nil {
			return nil, err
		}
		g.Enabled = live != 0
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetBindingDomainSend sets or clears the domain-wide sending grant on a key's
// binding for one inbox. Setting the grant forces the binding's role to owner
// (domain-wide sending and owner are one state); clearing it leaves the role as
// owner, because the system never lowers a role on its own. Dropping the role
// below owner through UpdateAPIKey drops the grant with it.
//
// The inbox must be the domain's catch-all: the grant is only meaningful
// anchored there, and hiding the control everywhere else in the UI is mirrored
// here so a direct API call cannot place it elsewhere. A missing binding is
// ErrNotFound.
func (s *Store) SetBindingDomainSend(ctx context.Context, accountID, clientID, inboxID string, enabled bool) error {
	if strings.TrimSpace(clientID) == "" || strings.TrimSpace(inboxID) == "" {
		return ErrNotFound
	}
	var isCatchAll int
	err := s.read.QueryRowContext(ctx, `SELECT CASE WHEN d.catch_all_inbox_id=i.id THEN 1 ELSE 0 END FROM inboxes i JOIN domains d ON d.id=i.domain_id WHERE i.id=? AND i.account_id=?`, inboxID, accountID).Scan(&isCatchAll)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if isCatchAll == 0 {
		return ErrNoCatchAll
	}
	res, err := s.write.ExecContext(ctx, `UPDATE client_inbox_bindings SET send_any_in_domain=?, role=CASE WHEN ?=1 THEN 'owner' ELSE role END WHERE client_id=? AND inbox_id=?`, boolInt(enabled), boolInt(enabled), clientID, inboxID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
