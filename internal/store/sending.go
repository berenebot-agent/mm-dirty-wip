package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/dellarb/mailmoose/internal/model"
)

// SendingTarget names the managed domain whose sending connector must carry a
// message. An external (non-managed) sender identity no longer exists; every
// sender resolves to a managed domain of the same account.
type SendingTarget struct {
	DomainID string
}

// SendingConfigForTarget returns the sending configuration that applies to a
// resolved target's domain.
func (s *Store) SendingConfigForTarget(ctx context.Context, accountID, inboxID string, target SendingTarget) (DomainSendingConfig, error) {
	return s.ResolveDomainSendingConfig(ctx, accountID, target.DomainID)
}

// ResolveSendingTarget maps a requested sender address to its From identity and
// the managed domain whose connector sends it. It is the send path's entry
// point.
func (s *Store) ResolveSendingTarget(ctx context.Context, accountID, inboxID, requested string) (model.Address, SendingTarget, error) {
	return resolveSendingTargetQuery(ctx, s.read, accountID, inboxID, requested)
}

// SendingConfigForMessage resolves the sending configuration for an already
// queued outbound message by its frozen sending domain.
func (s *Store) SendingConfigForMessage(ctx context.Context, accountID, messageID string) (DomainSendingConfig, error) {
	var domainID string
	err := s.read.QueryRowContext(ctx, `SELECT COALESCE(m.sending_domain_id,i.domain_id) FROM messages m JOIN inboxes i ON i.id=m.inbox_id WHERE m.id=? AND m.account_id=?`, messageID, accountID).Scan(&domainID)
	if err == sql.ErrNoRows {
		return DomainSendingConfig{}, ErrNotFound
	}
	if err != nil {
		return DomainSendingConfig{}, err
	}
	return s.ResolveDomainSendingConfig(ctx, accountID, domainID)
}

// resolveSendingTargetQuery maps a requested sender to its From identity and the
// managed domain that owns it. An empty request, or one equal to the inbox
// primary, resolves to the primary on the inbox's own domain. Any other address
// must match one of the inbox's managed aliases; a request that matches neither
// is ErrForbidden.
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
