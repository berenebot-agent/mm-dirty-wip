package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"gatehouse-mail/internal/idgen"
	"gatehouse-mail/internal/model"
)

var ErrExternalAliasDeleted = errors.New("external sending alias was removed; select a sender and send again")
var ErrInvalidAlias = errors.New("invalid external alias")

// ExternalAlias includes the encrypted configuration only on the store surface.
// HTTP handlers must serialize ExternalAlias.ExternalAlias, never credentials.
type ExternalAlias struct {
	model.ExternalAlias
	AccountID       string `json:"-"`
	EncryptedConfig string `json:"-"`
}

const externalAliasColumns = `id,account_id,inbox_id,address,display_name,provider,encrypted_config,revision,created_at,updated_at`

func scanExternalAlias(row interface{ Scan(...any) error }) (ExternalAlias, error) {
	var a ExternalAlias
	var created, updated string
	err := row.Scan(&a.ID, &a.AccountID, &a.InboxID, &a.Address, &a.DisplayName, &a.Provider, &a.EncryptedConfig, &a.Revision, &created, &updated)
	if err == sql.ErrNoRows {
		return a, ErrNotFound
	}
	a.Configured = a.Provider != ""
	a.CreatedAt, a.UpdatedAt = parseTime(created), parseTime(updated)
	return a, err
}

func (s *Store) GetExternalAlias(ctx context.Context, accountID, inboxID, aliasID string) (ExternalAlias, error) {
	return scanExternalAlias(s.read.QueryRowContext(ctx, `SELECT `+externalAliasColumns+` FROM external_aliases WHERE account_id=? AND inbox_id=? AND id=?`, accountID, inboxID, aliasID))
}

// ListExternalAliases returns redacted metadata, grouped by inbox.
func (s *Store) ListExternalAliases(ctx context.Context, accountID string) (map[string][]model.ExternalAlias, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT id,inbox_id,address,display_name,provider,revision,created_at,updated_at FROM external_aliases WHERE account_id=? ORDER BY address`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]model.ExternalAlias{}
	for rows.Next() {
		a, scanErr := scanExternalAliasMetadata(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out[a.InboxID] = append(out[a.InboxID], a)
	}
	return out, rows.Err()
}

// ListExternalAliasesForInbox returns one inbox's redacted external aliases.
func (s *Store) ListExternalAliasesForInbox(ctx context.Context, accountID, inboxID string) ([]model.ExternalAlias, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT id,inbox_id,address,display_name,provider,revision,created_at,updated_at FROM external_aliases WHERE account_id=? AND inbox_id=? ORDER BY address`, accountID, inboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ExternalAlias{}
	for rows.Next() {
		a, scanErr := scanExternalAliasMetadata(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func scanExternalAliasMetadata(row interface{ Scan(...any) error }) (model.ExternalAlias, error) {
	var a model.ExternalAlias
	var created, updated string
	if err := row.Scan(&a.ID, &a.InboxID, &a.Address, &a.DisplayName, &a.Provider, &a.Revision, &created, &updated); err != nil {
		return a, err
	}
	a.Configured = a.Provider != ""
	a.CreatedAt, a.UpdatedAt = parseTime(created), parseTime(updated)
	return a, nil
}

func (s *Store) CreateExternalAlias(ctx context.Context, accountID, inboxID, address, display string) (ExternalAlias, error) {
	address = normalizeAddress(address)
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Address != address || strings.ContainsAny(address, " <>\t\r\n") || len(address) > 254 {
		return ExternalAlias{}, fmt.Errorf("%w: enter a full email address", ErrInvalidAlias)
	}
	display, err = NormalizeAliasDisplayName(display)
	if err != nil {
		return ExternalAlias{}, fmt.Errorf("%w: %v", ErrInvalidAlias, err)
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return ExternalAlias{}, err
	}
	defer tx.Rollback()
	primary, _, err := resolveSenderQuery(ctx, tx, accountID, inboxID, "")
	if err != nil {
		return ExternalAlias{}, err
	}
	if primary.Address == address {
		return ExternalAlias{}, fmt.Errorf("%w: address is the inbox primary", ErrInvalidAlias)
	}
	if _, _, err = resolveSenderQuery(ctx, tx, accountID, inboxID, address); err == nil {
		return ExternalAlias{}, fmt.Errorf("%w: address already belongs to this inbox", ErrConflict)
	} else if !errors.Is(err, ErrForbidden) {
		return ExternalAlias{}, err
	}
	// An external alias is send-only, but its address must not duplicate an
	// address that already resolves inbound anywhere in the account (a managed
	// inbox or managed alias on a managed domain), or the two identities would
	// be ambiguous.
	var managedCollision int
	if err = tx.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM inboxes i JOIN domains d ON d.id=i.domain_id WHERE i.account_id=? AND (i.local_part||'@'||d.name)=?)
			+(SELECT count(*) FROM inbox_aliases a JOIN domains d ON d.id=a.domain_id WHERE a.account_id=? AND (a.local_part||'@'||d.name)=?)`,
		accountID, address, accountID, address).Scan(&managedCollision); err != nil {
		return ExternalAlias{}, err
	}
	if managedCollision != 0 {
		return ExternalAlias{}, fmt.Errorf("%w: address is already an inbound mailbox or alias", ErrConflict)
	}
	// One external address, one connector: the same address in two inboxes
	// would be two sending identities for a single address.
	var externalCollision int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM external_aliases WHERE account_id=? AND address=? COLLATE NOCASE`, accountID, address).Scan(&externalCollision); err != nil {
		return ExternalAlias{}, err
	}
	if externalCollision != 0 {
		return ExternalAlias{}, fmt.Errorf("%w: address is already an external alias in this account", ErrConflict)
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM inbox_aliases WHERE inbox_id=?)+(SELECT count(*) FROM external_aliases WHERE inbox_id=?)`, inboxID, inboxID).Scan(&count); err != nil {
		return ExternalAlias{}, err
	}
	if count >= maxInboxAliases {
		return ExternalAlias{}, fmt.Errorf("%w: too many aliases", ErrInvalidAlias)
	}
	id, now := idgen.New("ea"), nowText()
	if _, err = tx.ExecContext(ctx, `INSERT INTO external_aliases(id,account_id,inbox_id,address,display_name,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, id, accountID, inboxID, address, display, now, now); err != nil {
		return ExternalAlias{}, err
	}
	a, err := scanExternalAlias(tx.QueryRowContext(ctx, `SELECT `+externalAliasColumns+` FROM external_aliases WHERE id=?`, id))
	if err != nil {
		return a, err
	}
	return a, tx.Commit()
}

func (s *Store) UpdateExternalAlias(ctx context.Context, accountID, inboxID, aliasID, display string) (ExternalAlias, error) {
	display, err := NormalizeAliasDisplayName(display)
	if err != nil {
		return ExternalAlias{}, fmt.Errorf("%w: %v", ErrInvalidAlias, err)
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return ExternalAlias{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE external_aliases SET display_name=?,revision=revision+1,updated_at=? WHERE id=? AND inbox_id=? AND account_id=?`, display, nowText(), aliasID, inboxID, accountID)
	if err != nil {
		return ExternalAlias{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ExternalAlias{}, ErrNotFound
	}
	a, err := scanExternalAlias(tx.QueryRowContext(ctx, `SELECT `+externalAliasColumns+` FROM external_aliases WHERE id=?`, aliasID))
	if err != nil {
		return a, err
	}
	return a, tx.Commit()
}

func (s *Store) DeleteExternalAlias(ctx context.Context, accountID, inboxID, aliasID string) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, err := scanExternalAlias(tx.QueryRowContext(ctx, `SELECT `+externalAliasColumns+` FROM external_aliases WHERE id=? AND inbox_id=? AND account_id=?`, aliasID, inboxID, accountID))
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE inboxes SET default_sender='' WHERE id=? AND account_id=? AND default_sender=? COLLATE NOCASE`, inboxID, accountID, a.Address); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM external_aliases WHERE id=?`, aliasID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SaveExternalAliasSendingConfig(ctx context.Context, accountID, inboxID, aliasID, provider, encrypted string, expected ConfigVersion) (ExternalAlias, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return ExternalAlias{}, err
	}
	defer tx.Rollback()
	if _, err := scanExternalAlias(tx.QueryRowContext(ctx, `SELECT `+externalAliasColumns+` FROM external_aliases WHERE id=? AND inbox_id=? AND account_id=?`, aliasID, inboxID, accountID)); err != nil {
		return ExternalAlias{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE external_aliases SET provider=?,encrypted_config=?,revision=revision+1,updated_at=? WHERE id=? AND inbox_id=? AND account_id=? AND id=? AND revision=?`, provider, encrypted, nowText(), aliasID, inboxID, accountID, expected.ID, expected.Revision)
	if err != nil {
		return ExternalAlias{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ExternalAlias{}, ErrConflict
	}
	a, err := scanExternalAlias(tx.QueryRowContext(ctx, `SELECT `+externalAliasColumns+` FROM external_aliases WHERE id=?`, aliasID))
	if err != nil {
		return a, err
	}
	return a, tx.Commit()
}

func (s *Store) RequeuePendingForExternalAlias(ctx context.Context, accountID, aliasID string) (int64, error) {
	res, err := s.write.ExecContext(ctx, `UPDATE messages SET attempts=0,last_error='',next_attempt_at='',claim_owner='',claim_expires_at='' WHERE account_id=? AND sending_external_alias_id=? AND direction='outbound' AND status='pending'`, accountID, aliasID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SendingTarget captures ownership independently of provider configuration.
type SendingTarget struct {
	DomainID        string
	ExternalAliasID string
}

// ResolveSendingTarget maps a requested sender address to its From identity and
// the sending target (a managed domain or an external alias). It is the send
// path's entry point; drafts additionally freeze the alias id (see
// ResolveSendingTargetByID) so a deleted-then-recreated alias cannot rebind.
func (s *Store) ResolveSendingTarget(ctx context.Context, accountID, inboxID, requested string) (model.Address, SendingTarget, error) {
	return resolveSendingTargetQuery(ctx, s.read, accountID, inboxID, requested)
}

// ResolveSendingTargetByID resolves a frozen external alias id to its current
// From identity. It returns ErrExternalAliasDeleted if the alias no longer
// exists, so a queued or draft send can never silently fall back to a domain
// connector or a recreated alias with the same address.
func (s *Store) ResolveSendingTargetByID(ctx context.Context, accountID, inboxID, aliasID string) (model.Address, SendingTarget, error) {
	a, err := s.GetExternalAlias(ctx, accountID, inboxID, aliasID)
	if errors.Is(err, ErrNotFound) {
		return model.Address{}, SendingTarget{}, ErrExternalAliasDeleted
	}
	if err != nil {
		return model.Address{}, SendingTarget{}, err
	}
	name := a.DisplayName
	if name == "" {
		if box, berr := s.GetInboxInternal(ctx, accountID, inboxID); berr == nil {
			name = box.DisplayName
		}
	}
	return model.Address{Name: name, Address: a.Address}, SendingTarget{ExternalAliasID: a.ID}, nil
}

func (s *Store) SendingConfigForTarget(ctx context.Context, accountID, inboxID string, target SendingTarget) (DomainSendingConfig, error) {
	if target.ExternalAliasID == "" {
		return s.GetDomainSendingConfig(ctx, accountID, target.DomainID)
	}
	a, err := s.GetExternalAlias(ctx, accountID, inboxID, target.ExternalAliasID)
	if errors.Is(err, ErrNotFound) {
		return DomainSendingConfig{}, ErrExternalAliasDeleted
	}
	if err != nil {
		return DomainSendingConfig{}, err
	}
	if !a.Configured {
		return DomainSendingConfig{}, ErrNoProvider
	}
	return DomainSendingConfig{ID: a.ID, AccountID: a.AccountID, ExternalAliasID: a.ID, Provider: a.Provider, EncryptedConfig: a.EncryptedConfig, Revision: a.Revision, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt}, nil
}

func (s *Store) SendingConfigForMessage(ctx context.Context, accountID, messageID string) (DomainSendingConfig, error) {
	var inboxID string
	var target SendingTarget
	err := s.read.QueryRowContext(ctx, `SELECT m.inbox_id,COALESCE(m.sending_domain_id,i.domain_id),m.sending_external_alias_id FROM messages m JOIN inboxes i ON i.id=m.inbox_id WHERE m.id=? AND m.account_id=?`, messageID, accountID).Scan(&inboxID, &target.DomainID, &target.ExternalAliasID)
	if err == sql.ErrNoRows {
		return DomainSendingConfig{}, ErrNotFound
	}
	if err != nil {
		return DomainSendingConfig{}, err
	}
	return s.SendingConfigForTarget(ctx, accountID, inboxID, target)
}
