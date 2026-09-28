package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/auth"
	"github.com/dellarb/mailmoose/internal/idgen"
	"github.com/dellarb/mailmoose/internal/model"
)

// AccountMailerInboxID returns the mailbox an account uses to send its
// invitations, or "" when none has been selected or the mailbox was deleted.
func (s *Store) AccountMailerInboxID(ctx context.Context, accountID string) (string, error) {
	var v sql.NullString
	err := s.read.QueryRowContext(ctx, `SELECT mailer_inbox_id FROM accounts WHERE id=?`, accountID).Scan(&v)
	if err == sql.ErrNoRows {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if !v.Valid {
		return "", nil
	}
	return v.String, nil
}

// SetAccountMailerInbox stores an account's invitation mailer. A non-empty id
// must name an inbox owned by that account, so an account can only send its
// invitations from one of its own mailboxes. An empty id clears the selection.
func (s *Store) SetAccountMailerInbox(ctx context.Context, accountID, inboxID string) error {
	if inboxID != "" {
		var n int
		if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return ErrForbidden
		}
	}
	res, err := s.write.ExecContext(ctx, `UPDATE accounts SET mailer_inbox_id=? WHERE id=?`, nullString(inboxID), accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AccountSummary is an account as shown on the system administrator plane: the
// email of its Admin (empty until an invitation is accepted) and, while an
// account invitation is outstanding, that invitation's id and expiry.
type AccountSummary struct {
	ID              string
	Name            string
	AdminEmail      string
	InviteID        string
	InviteExpiresAt time.Time
	CreatedAt       time.Time
}

// ListAccounts returns every account with its Admin and any outstanding
// new-account invitation, newest first.
func (s *Store) ListAccounts(ctx context.Context) ([]AccountSummary, error) {
	const q = `SELECT a.id,a.name,a.created_at,
	  COALESCE((SELECT u.email FROM users u WHERE u.account_id=a.id AND u.is_admin=1 ORDER BY u.created_at LIMIT 1),''),
	  COALESCE((SELECT i.id FROM invites i WHERE i.account_id=a.id AND i.kind='account_admin' AND i.accepted_at IS NULL AND i.revoked_at IS NULL ORDER BY i.created_at DESC LIMIT 1),''),
	  (SELECT i.expires_at FROM invites i WHERE i.account_id=a.id AND i.kind='account_admin' AND i.accepted_at IS NULL AND i.revoked_at IS NULL ORDER BY i.created_at DESC LIMIT 1)
	FROM accounts a ORDER BY a.created_at DESC`
	rows, err := s.read.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AccountSummary{}
	for rows.Next() {
		var a AccountSummary
		var created string
		var expiry sql.NullString
		if err = rows.Scan(&a.ID, &a.Name, &created, &a.AdminEmail, &a.InviteID, &expiry); err != nil {
			return nil, err
		}
		a.CreatedAt = parseTime(created)
		if expiry.Valid {
			a.InviteExpiresAt = parseTime(expiry.String)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAccountUsers returns the account's human users. Non-admin members carry
// their per-inbox roles; an Admin has no explicit role rows.
func (s *Store) ListAccountUsers(ctx context.Context, accountID string) ([]model.User, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT id,account_id,email,is_admin,is_system_admin,created_at FROM users WHERE account_id=? ORDER BY created_at`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.User
	for rows.Next() {
		var u model.User
		var admin, sysadmin int
		var created string
		if err = rows.Scan(&u.ID, &u.AccountID, &u.Email, &admin, &sysadmin, &created); err != nil {
			return nil, err
		}
		u.IsAdmin = admin != 0
		u.SystemAdmin = sysadmin != 0
		u.CreatedAt = parseTime(created)
		out = append(out, u)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if out[i].IsAdmin {
			continue
		}
		roles, rerr := s.userMailboxRoles(ctx, out[i].ID)
		if rerr != nil {
			return nil, rerr
		}
		if len(roles) > 0 {
			out[i].Roles = roles
		}
	}
	return out, nil
}

// SetUserRoles replaces a non-admin user's per-inbox roles. It refuses to
// change an account Admin (whose access is implicit) or a system administrator.
func (s *Store) SetUserRoles(ctx context.Context, accountID, userID string, roles map[string]string) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var admin, sysadmin int
	err = tx.QueryRowContext(ctx, `SELECT is_admin,is_system_admin FROM users WHERE id=? AND account_id=?`, userID, accountID).Scan(&admin, &sysadmin)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if admin != 0 || sysadmin != 0 {
		return fmt.Errorf("%w: account Admins have implicit access", ErrForbidden)
	}
	if err = setUserMailboxRolesTx(ctx, tx, accountID, userID, roles); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteAccountMember removes a non-admin, non-system user from an account. Its
// mailbox-role rows cascade away.
func (s *Store) DeleteAccountMember(ctx context.Context, accountID, userID string) error {
	res, err := s.write.ExecContext(ctx, `DELETE FROM users WHERE id=? AND account_id=? AND is_admin=0 AND is_system_admin=0`, userID, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// createUserTx inserts a user inside an existing transaction, enforcing the
// global email uniqueness rule first.
func createUserTx(ctx context.Context, tx *sql.Tx, accountID, email, passwordHash string, admin, systemAdmin bool) (model.User, error) {
	email = normalizeAddress(email)
	if err := ensureEmailUnused(ctx, tx, email, ""); err != nil {
		return model.User{}, err
	}
	id := idgen.New("usr")
	now := nowText()
	if _, err := tx.ExecContext(ctx, `INSERT INTO users(id,account_id,email,password_hash,is_admin,is_system_admin,created_at) VALUES(?,?,?,?,?,?,?)`, id, accountID, email, passwordHash, boolInt(admin), boolInt(systemAdmin), now); err != nil {
		return model.User{}, err
	}
	return model.User{ID: id, AccountID: accountID, Email: email, IsAdmin: admin, SystemAdmin: systemAdmin, CreatedAt: parseTime(now)}, nil
}

// setUserMailboxRolesTx replaces a user's per-inbox roles, validating each role
// name and that every inbox belongs to the account.
func setUserMailboxRolesTx(ctx context.Context, tx *sql.Tx, accountID, userID string, roles map[string]string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_mailbox_roles WHERE user_id=?`, userID); err != nil {
		return err
	}
	for inboxID, role := range roles {
		role = strings.ToLower(strings.TrimSpace(role))
		if role != "read" && role != "assistant" && role != "owner" {
			return fmt.Errorf("invalid role %q", role)
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&n); err != nil || n != 1 {
			return ErrForbidden
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_mailbox_roles(user_id,inbox_id,role) VALUES(?,?,?)`, userID, inboxID, role); err != nil {
			return err
		}
	}
	return nil
}

// InviteInput is the request to create an invitation. For an account_admin
// invite AccountID must be empty (a fresh account is created); for an operator
// invite AccountID is the existing account and InboxIDs are the mailboxes the
// operator will own.
type InviteInput struct {
	AccountID   string
	AccountName string
	Email       string
	Kind        string
	InboxIDs    []string
	Quota       int64
	CreatedBy   string
	TTL         time.Duration
}

// CreateInvite persists a pending invitation and returns it together with the
// one-time plaintext setup token. Only the token's hash is stored.
func (s *Store) CreateInvite(ctx context.Context, in InviteInput) (model.Invite, string, error) {
	email := normalizeAddress(in.Email)
	if email == "" || !strings.Contains(email, "@") {
		return model.Invite{}, "", fmt.Errorf("a valid email address is required")
	}
	if in.Kind != model.InviteKindAccountAdmin && in.Kind != model.InviteKindOperator {
		return model.Invite{}, "", fmt.Errorf("invalid invitation kind %q", in.Kind)
	}
	if in.TTL <= 0 {
		in.TTL = 7 * 24 * time.Hour
	}
	token, err := auth.RandomToken(32)
	if err != nil {
		return model.Invite{}, "", err
	}
	inv := model.Invite{ID: idgen.New("inv"), Email: email, Kind: in.Kind, InboxIDs: in.InboxIDs}
	tx, err := s.write.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return model.Invite{}, "", err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	inv.CreatedAt = now
	inv.ExpiresAt = now.Add(in.TTL)
	if in.Kind == model.InviteKindOperator {
		if in.AccountID == "" {
			return model.Invite{}, "", fmt.Errorf("operator invitations require an account")
		}
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM accounts WHERE id=?`, in.AccountID).Scan(&n); err != nil || n != 1 {
			return model.Invite{}, "", ErrNotFound
		}
		for _, id := range in.InboxIDs {
			var c int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inboxes WHERE id=? AND account_id=?`, id, in.AccountID).Scan(&c); err != nil || c != 1 {
				return model.Invite{}, "", ErrForbidden
			}
		}
		inv.AccountID = in.AccountID
	} else {
		accountName := strings.TrimSpace(in.AccountName)
		if accountName == "" {
			accountName = strings.SplitN(email, "@", 2)[0]
		}
		aid := idgen.New("acct")
		if _, err = tx.ExecContext(ctx, `INSERT INTO accounts(id,name,storage_quota_bytes,created_at) VALUES(?,?,?,?)`, aid, accountName, in.Quota, timeText(now)); err != nil {
			return model.Invite{}, "", err
		}
		inv.AccountID = aid
		inv.AccountName = accountName
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO invites(id,account_id,account_name,email,kind,inbox_ids_json,token_hash,expires_at,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		inv.ID, inv.AccountID, inv.AccountName, email, inv.Kind, jsonString(in.InboxIDs), auth.HashToken(token), timeText(inv.ExpiresAt), in.CreatedBy, timeText(now)); err != nil {
		return model.Invite{}, "", err
	}
	if err = tx.Commit(); err != nil {
		return model.Invite{}, "", err
	}
	return inv, token, nil
}

// ListInvites returns pending and historical invitations. When accountID is
// non-empty it is scoped to that account; an empty accountID returns every
// invitation, for the installation-level system administrator.
func (s *Store) ListInvites(ctx context.Context, accountID string) ([]model.Invite, error) {
	q := `SELECT id,COALESCE(account_id,''),account_name,email,kind,inbox_ids_json,expires_at,accepted_at,revoked_at,created_at FROM invites`
	args := []any{}
	if accountID != "" {
		q += ` WHERE account_id=?`
		args = append(args, accountID)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Invite{}
	for rows.Next() {
		inv, err := scanInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// GetInviteByID loads a single invitation by id.
func (s *Store) GetInviteByID(ctx context.Context, id string) (model.Invite, error) {
	row := s.read.QueryRowContext(ctx, `SELECT id,COALESCE(account_id,''),account_name,email,kind,inbox_ids_json,expires_at,accepted_at,revoked_at,created_at FROM invites WHERE id=?`, id)
	inv, err := scanInvite(row)
	if err == sql.ErrNoRows {
		return model.Invite{}, ErrNotFound
	}
	return inv, err
}

// GetInviteByToken resolves a redeemable invitation from its plaintext token.
// An unknown, revoked or expired token returns ErrInviteExpired.
func (s *Store) GetInviteByToken(ctx context.Context, token string) (model.Invite, error) {
	row := s.read.QueryRowContext(ctx, `SELECT id,COALESCE(account_id,''),account_name,email,kind,inbox_ids_json,expires_at,accepted_at,revoked_at,created_at FROM invites WHERE token_hash=?`, auth.HashToken(token))
	inv, err := scanInvite(row)
	if err == sql.ErrNoRows {
		return model.Invite{}, ErrInviteExpired
	}
	if err != nil {
		return model.Invite{}, err
	}
	if !inv.Pending(time.Now().UTC()) {
		return model.Invite{}, ErrInviteExpired
	}
	return inv, nil
}

// RedeemInvite consumes a single-use setup token, creating the account Admin or
// mailbox operator it describes. The password is hashed before the transaction
// so the writer lock is held only for the inserts.
func (s *Store) RedeemInvite(ctx context.Context, token, password string) (model.User, error) {
	ph, err := auth.HashPassword(password)
	if err != nil {
		return model.User{}, err
	}
	tx, err := s.write.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return model.User{}, err
	}
	defer tx.Rollback()
	inv, err := scanInvite(tx.QueryRowContext(ctx, `SELECT id,COALESCE(account_id,''),account_name,email,kind,inbox_ids_json,expires_at,accepted_at,revoked_at,created_at FROM invites WHERE token_hash=?`, auth.HashToken(token)))
	if err == sql.ErrNoRows {
		return model.User{}, ErrInviteExpired
	}
	if err != nil {
		return model.User{}, err
	}
	if !inv.Pending(time.Now().UTC()) {
		return model.User{}, ErrInviteExpired
	}
	admin := inv.Kind == model.InviteKindAccountAdmin
	u, err := createUserTx(ctx, tx, inv.AccountID, inv.Email, ph, admin, false)
	if err != nil {
		return model.User{}, err
	}
	if !admin {
		roles := make(map[string]string, len(inv.InboxIDs))
		for _, id := range inv.InboxIDs {
			roles[id] = "owner"
		}
		if err = setUserMailboxRolesTx(ctx, tx, inv.AccountID, u.ID, roles); err != nil {
			return model.User{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE invites SET accepted_at=? WHERE id=?`, nowText(), inv.ID); err != nil {
		return model.User{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.User{}, err
	}
	return u, nil
}

// RotateInviteToken issues a fresh one-time token for a pending invite,
// invalidating any previous setup link, and returns the new plaintext token.
// It backs "send" (and re-send) so a later send always carries a live link.
func (s *Store) RotateInviteToken(ctx context.Context, accountID, id string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	token, err := auth.RandomToken(32)
	if err != nil {
		return "", err
	}
	res, err := s.write.ExecContext(ctx, `UPDATE invites SET token_hash=?,expires_at=? WHERE id=? AND accepted_at IS NULL AND revoked_at IS NULL AND (?='' OR account_id=?)`,
		auth.HashToken(token), timeText(time.Now().UTC().Add(ttl)), id, accountID, accountID)
	if err != nil {
		return "", err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return "", ErrNotFound
	}
	return token, nil
}

// RevokeInvite cancels a pending invitation. It returns ErrNotFound when no
// matching pending invite exists.
func (s *Store) RevokeInvite(ctx context.Context, accountID, id string) error {
	res, err := s.write.ExecContext(ctx, `UPDATE invites SET revoked_at=? WHERE id=? AND accepted_at IS NULL AND revoked_at IS NULL AND (?='' OR account_id=?)`, nowText(), id, accountID, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanInvite(row interface{ Scan(...any) error }) (model.Invite, error) {
	var inv model.Invite
	var inboxJSON, expiry, created string
	var accepted, revoked sql.NullString
	if err := row.Scan(&inv.ID, &inv.AccountID, &inv.AccountName, &inv.Email, &inv.Kind, &inboxJSON, &expiry, &accepted, &revoked, &created); err != nil {
		return inv, err
	}
	inv.InboxIDs = decodeStrings(inboxJSON)
	inv.ExpiresAt = parseTime(expiry)
	inv.CreatedAt = parseTime(created)
	if accepted.Valid {
		t := parseTime(accepted.String)
		inv.AcceptedAt = &t
	}
	if revoked.Valid {
		t := parseTime(revoked.String)
		inv.RevokedAt = &t
	}
	return inv, nil
}
