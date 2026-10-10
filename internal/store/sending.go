package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
)

// SendingTarget names the sending connector that must carry a message. For a
// domain inbox it is the managed domain whose sending configuration applies. For
// a standalone inbox it is the inbox itself (DomainID empty), whose sending is
// carried by the inbox's own remote SMTP binding.
type SendingTarget struct {
	DomainID string
	// InboxID is set for a standalone inbox's own binding: DomainID is empty and
	// the sending connector is resolved from the inbox's remote SMTP
	// configuration.
	InboxID string
}

// StandaloneSenderResolver resolves the sending configuration for a standalone
// inbox's own remote SMTP binding. The real SMTP send integration is a later
// wave; when no resolver is installed, SendingConfigForTarget reports
// ErrNoProvider so a standalone message is queued and held rather than failing or
// being attributed to a non-existent domain.
type StandaloneSenderResolver interface {
	ResolveInboxSendingConfig(ctx context.Context, accountID, inboxID string) (DomainSendingConfig, error)
}

// SetStandaloneSenderResolver installs the resolver for a standalone inbox's own
// sending configuration. It sets the single bridge on this Store (never a global).
func (s *Store) SetStandaloneSenderResolver(r StandaloneSenderResolver) { s.standaloneSender = r }

// SendingConfigForTarget returns the sending configuration that applies to a
// resolved target: a domain's (with inheritance) or, for a standalone inbox, the
// inbox's own remote SMTP binding.
func (s *Store) SendingConfigForTarget(ctx context.Context, accountID, inboxID string, target SendingTarget) (DomainSendingConfig, error) {
	if target.DomainID == "" {
		// A standalone inbox has no managed domain. Its sending is carried by its
		// own remote SMTP binding, resolved through the injected bridge; until the
		// SMTP send integration is wired this is ErrNoProvider, so the message is
		// queued and held rather than attributed to a domain that does not exist.
		if s.standaloneSender == nil {
			return DomainSendingConfig{}, ErrNoProvider
		}
		return s.standaloneSender.ResolveInboxSendingConfig(ctx, accountID, target.InboxID)
	}
	return s.ResolveDomainSendingConfig(ctx, accountID, target.DomainID)
}

// ResolveSendingTarget maps a requested sender address to its From identity and
// the managed domain whose connector sends it. It is the send path's entry
// point.
func (s *Store) ResolveSendingTarget(ctx context.Context, accountID, inboxID, requested string) (model.Address, SendingTarget, error) {
	return resolveSendingTargetQuery(ctx, s.read, accountID, inboxID, requested)
}

// SendingConfigForMessage resolves the sending configuration for an already
// queued outbound message by its frozen sending domain, or — for a standalone
// inbox — by the inbox's own remote SMTP binding. The frozen sending_domain_id is
// empty for a standalone message, so it routes through the standalone resolver
// rather than being attributed to a non-existent domain.
func (s *Store) SendingConfigForMessage(ctx context.Context, accountID, messageID string) (DomainSendingConfig, error) {
	var domainID sql.NullString
	var inboxID, kind string
	err := s.read.QueryRowContext(ctx, `SELECT m.sending_domain_id,i.id,COALESCE(i.kind,'domain') FROM messages m JOIN inboxes i ON i.id=m.inbox_id WHERE m.id=? AND m.account_id=?`, messageID, accountID).Scan(&domainID, &inboxID, &kind)
	if err == sql.ErrNoRows {
		return DomainSendingConfig{}, ErrNotFound
	}
	if err != nil {
		return DomainSendingConfig{}, err
	}
	if kind == model.InboxKindStandalone || !domainID.Valid || domainID.String == "" {
		if kind == model.InboxKindStandalone {
			return s.SendingConfigForTarget(ctx, accountID, inboxID, SendingTarget{InboxID: inboxID})
		}
		// A domain message that has not frozen a sending domain resolves against
		// the inbox's own domain (legacy rows).
		var inboxDomain string
		if derr := s.read.QueryRowContext(ctx, `SELECT COALESCE(domain_id,'') FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&inboxDomain); derr != nil {
			return DomainSendingConfig{}, derr
		}
		if inboxDomain == "" {
			return DomainSendingConfig{}, ErrNoProvider
		}
		return s.ResolveDomainSendingConfig(ctx, accountID, inboxDomain)
	}
	return s.ResolveDomainSendingConfig(ctx, accountID, domainID.String)
}

// resolveSendingTargetQuery maps a requested sender to its From identity and the
// managed domain that owns it. An empty request, or one equal to the inbox
// primary, resolves to the primary on the inbox's own domain. Any other address
// must match one of the inbox's managed aliases; a request that matches neither
// is ErrForbidden.
func resolveSendingTargetQuery(ctx context.Context, q senderQueryer, accountID, inboxID, requested string) (model.Address, SendingTarget, error) {
	// Domain inbox: the primary is local_part@domain; managed aliases may override.
	// Standalone inbox: the primary is the inbox's own address and there are no
	// managed aliases or domain, so the sender must be exactly that address.
	var kind, primaryName, primary, domainID string
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(i.kind,'domain'),i.display_name,COALESCE(i.local_part||'@'||d.name,i.address),COALESCE(i.domain_id,'') FROM inboxes i LEFT JOIN domains d ON d.id=i.domain_id WHERE i.id=? AND i.account_id=?`, inboxID, accountID).Scan(&kind, &primaryName, &primary, &domainID); err != nil {
		if err == sql.ErrNoRows {
			return model.Address{}, SendingTarget{}, ErrNotFound
		}
		return model.Address{}, SendingTarget{}, err
	}
	requested = strings.ToLower(strings.TrimSpace(requested))
	if kind == model.InboxKindStandalone {
		// A standalone inbox has no managed domain: it sends as its own connected
		// address only. A different requested sender is not allowed.
		if requested == "" || requested == strings.ToLower(primary) {
			return model.Address{Name: primaryName, Address: primary}, SendingTarget{InboxID: inboxID}, nil
		}
		return model.Address{}, SendingTarget{}, fmt.Errorf("%w: %w", ErrForbidden, ErrSenderNotAllowed)
	}
	if requested == "" || requested == strings.ToLower(primary) {
		return model.Address{Name: primaryName, Address: primary}, SendingTarget{DomainID: domainID}, nil
	}
	var alias, aliasDomainID, aliasName string
	err := q.QueryRowContext(ctx, `SELECT a.local_part||'@'||d.name,a.domain_id,COALESCE(NULLIF(a.display_name,''),i.display_name) FROM inbox_aliases a JOIN domains d ON d.id=a.domain_id JOIN inboxes i ON i.id=a.inbox_id WHERE a.account_id=? AND a.inbox_id=? AND (a.local_part||'@'||d.name)=?`, accountID, inboxID, requested).Scan(&alias, &aliasDomainID, &aliasName)
	if err == sql.ErrNoRows {
		return model.Address{}, SendingTarget{}, fmt.Errorf("%w: %w", ErrForbidden, ErrSenderNotAllowed)
	}
	if err != nil {
		return model.Address{}, SendingTarget{}, err
	}
	return model.Address{Name: aliasName, Address: alias}, SendingTarget{DomainID: aliasDomainID}, nil
}
