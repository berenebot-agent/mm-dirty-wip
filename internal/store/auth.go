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

func (s *Store) HasUsers(ctx context.Context) (bool, error) {
	var n int
	err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n > 0, err
}

// CreateInitialAdmin atomically creates the first account and admin user and
// marks that user as the installation's system administrator. It returns
// ErrConflict if any user already exists, so two concurrent setup requests
// cannot both succeed. The writer connection is serialized, so the
// check-and-insert inside one transaction is race-free.
func (s *Store) CreateInitialAdmin(ctx context.Context, name, email, password string, quota int64) (model.User, error) {
	return s.createAccountAndAdmin(ctx, name, email, password, quota, true, true)
}

func (s *Store) CreateAccountAndAdmin(ctx context.Context, name, email, password string, quota int64) (model.User, error) {
	return s.createAccountAndAdmin(ctx, name, email, password, quota, false, false)
}

// createAccountAndAdmin performs the shared account + admin-user insert. When
// requireEmpty is set it first asserts that no user exists, which makes it the
// one-time bootstrap path. When systemAdmin is set the new user also carries
// the installation-level system-administrator role.
func (s *Store) createAccountAndAdmin(ctx context.Context, name, email, password string, quota int64, requireEmpty, systemAdmin bool) (model.User, error) {
	email = normalizeAddress(email)
	if email == "" {
		return model.User{}, fmt.Errorf("email required")
	}
	ph, err := auth.HashPassword(password)
	if err != nil {
		return model.User{}, err
	}
	// BEGIN IMMEDIATE so the empty-users check and the inserts are atomic
	// across processes: a second process blocks until this transaction
	// commits, then observes the user and returns ErrConflict instead of
	// creating a second administrator.
	tx, err := s.write.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return model.User{}, err
	}
	defer tx.Rollback()
	if requireEmpty {
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
			return model.User{}, err
		}
		if n > 0 {
			return model.User{}, ErrConflict
		}
	}
	aid, uid := idgen.New("acct"), idgen.New("usr")
	now := nowText()
	if _, err = tx.ExecContext(ctx, `INSERT INTO accounts(id,name,storage_quota_bytes,created_at) VALUES(?,?,?,?)`, aid, strings.TrimSpace(name), quota, now); err != nil {
		return model.User{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO users(id,account_id,email,password_hash,is_admin,is_system_admin,created_at) VALUES(?,?,?,?,1,?,?)`, uid, aid, email, ph, boolInt(systemAdmin), now); err != nil {
		return model.User{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.User{}, err
	}
	return model.User{ID: uid, AccountID: aid, Email: email, IsAdmin: true, SystemAdmin: systemAdmin, CreatedAt: parseTime(now)}, nil
}

// HasSystemAdmin reports whether the installation has a system administrator.
func (s *Store) HasSystemAdmin(ctx context.Context) (bool, error) {
	var n int
	err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE is_system_admin=1`).Scan(&n)
	return n > 0, err
}

// SyncSystemAdmin reconciles the configured system-administrator credentials
// with the database. The configured values are authoritative when present: a
// changed email or password is applied and every existing session for that user
// is revoked, so a rotated deployment secret takes effect on restart. When no
// system administrator exists it is created together with its own account.
//
// A configured email that already belongs to a different user is refused rather
// than silently taking over that identity. The error never contains the
// password.
func (s *Store) SyncSystemAdmin(ctx context.Context, accountName, email, password string, quota int64) (model.User, bool, error) {
	email = normalizeAddress(email)
	if email == "" {
		return model.User{}, false, fmt.Errorf("email required")
	}
	ph, err := auth.HashPassword(password)
	if err != nil {
		return model.User{}, false, err
	}
	tx, err := s.write.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return model.User{}, false, err
	}
	defer tx.Rollback()

	var uid, aid, currentEmail, currentHash, created string
	var admin int
	err = tx.QueryRowContext(ctx, `SELECT id,account_id,email,password_hash,is_admin,created_at FROM users WHERE is_system_admin=1 ORDER BY created_at LIMIT 1`).Scan(&uid, &aid, &currentEmail, &currentHash, &admin, &created)
	if err == sql.ErrNoRows {
		if err = ensureEmailUnused(ctx, tx, email, ""); err != nil {
			return model.User{}, false, err
		}
		aid, uid = idgen.New("acct"), idgen.New("usr")
		now := nowText()
		name := strings.TrimSpace(accountName)
		if name == "" {
			name = "MailMoose"
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO accounts(id,name,storage_quota_bytes,created_at) VALUES(?,?,?,?)`, aid, name, quota, now); err != nil {
			return model.User{}, false, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO users(id,account_id,email,password_hash,is_admin,is_system_admin,created_at) VALUES(?,?,?,?,1,1,?)`, uid, aid, email, ph, now); err != nil {
			return model.User{}, false, err
		}
		if err = tx.Commit(); err != nil {
			return model.User{}, false, err
		}
		return model.User{ID: uid, AccountID: aid, Email: email, IsAdmin: true, SystemAdmin: true, CreatedAt: parseTime(now)}, true, nil
	}
	if err != nil {
		return model.User{}, false, err
	}
	changed := false
	if currentEmail != email {
		if err = ensureEmailUnused(ctx, tx, email, uid); err != nil {
			return model.User{}, false, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE users SET email=? WHERE id=?`, email, uid); err != nil {
			return model.User{}, false, err
		}
		changed = true
	}
	if !auth.CheckPassword(currentHash, password) {
		if _, err = tx.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, ph, uid); err != nil {
			return model.User{}, false, err
		}
		changed = true
	}
	if changed {
		if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, uid); err != nil {
			return model.User{}, false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return model.User{}, false, err
	}
	return model.User{ID: uid, AccountID: aid, Email: email, IsAdmin: admin != 0, SystemAdmin: true, CreatedAt: parseTime(created)}, changed, nil
}

// ensureEmailUnused returns ErrConflict when email belongs to a user other than
// excludeID. It runs inside the same transaction as the write it guards.
func ensureEmailUnused(ctx context.Context, tx *sql.Tx, email, excludeID string) error {
	var other string
	err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE email=? AND id!=?`, email, excludeID).Scan(&other)
	switch {
	case err == nil:
		return fmt.Errorf("%w: email %s is already in use", ErrConflict, email)
	case err == sql.ErrNoRows:
		return nil
	default:
		return err
	}
}

func (s *Store) AuthenticateUser(ctx context.Context, email, password string) (model.User, error) {
	var u model.User
	var ph, created string
	var admin, sysadmin int
	err := s.read.QueryRowContext(ctx, `SELECT id,account_id,email,password_hash,is_admin,is_system_admin,created_at FROM users WHERE email=?`, normalizeAddress(email)).Scan(&u.ID, &u.AccountID, &u.Email, &ph, &admin, &sysadmin, &created)
	if err == sql.ErrNoRows {
		// Equalize the work done for an unknown account so login timing cannot
		// be used to enumerate accounts.
		auth.DummyPasswordCheck(password)
		return model.User{}, ErrNotFound
	}
	if err != nil {
		return model.User{}, err
	}
	if !auth.CheckPassword(ph, password) {
		return model.User{}, ErrNotFound
	}
	u.IsAdmin = admin != 0
	u.SystemAdmin = sysadmin != 0
	u.CreatedAt = parseTime(created)
	return u, nil
}

func (s *Store) GetUser(ctx context.Context, userID string) (model.User, error) {
	var u model.User
	var created string
	var admin, sysadmin int
	err := s.read.QueryRowContext(ctx, `SELECT id,account_id,email,is_admin,is_system_admin,created_at FROM users WHERE id=?`, userID).Scan(&u.ID, &u.AccountID, &u.Email, &admin, &sysadmin, &created)
	if err == sql.ErrNoRows {
		return model.User{}, ErrNotFound
	}
	if err != nil {
		return model.User{}, err
	}
	u.IsAdmin = admin != 0
	u.SystemAdmin = sysadmin != 0
	u.CreatedAt = parseTime(created)
	return u, nil
}

// GetUserByEmail looks a user up by login address, used by the operator
// password-reset command.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (model.User, error) {
	var u model.User
	var created string
	var admin, sysadmin int
	err := s.read.QueryRowContext(ctx, `SELECT id,account_id,email,is_admin,is_system_admin,created_at FROM users WHERE email=?`, normalizeAddress(email)).Scan(&u.ID, &u.AccountID, &u.Email, &admin, &sysadmin, &created)
	if err == sql.ErrNoRows {
		return model.User{}, ErrNotFound
	}
	if err != nil {
		return model.User{}, err
	}
	u.IsAdmin = admin != 0
	u.SystemAdmin = sysadmin != 0
	u.CreatedAt = parseTime(created)
	return u, nil
}

// UpdateAccountName changes an account's display name. The name is a label
// only; it never identifies the tenant, which is always accounts.id.
func (s *Store) UpdateAccountName(ctx context.Context, accountID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("account name is required")
	}
	if len(name) > 80 {
		return fmt.Errorf("account name must be 80 characters or fewer")
	}
	res, err := s.write.ExecContext(ctx, `UPDATE accounts SET name=? WHERE id=?`, name, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateUserEmail changes a user's login email after verifying the current
// password. Email uniqueness is enforced by the users table; the serialized
// writer connection makes the check-and-update race-free.
func (s *Store) UpdateUserEmail(ctx context.Context, userID, accountID, newEmail, currentPassword string) error {
	newEmail = normalizeAddress(newEmail)
	if newEmail == "" || !strings.Contains(newEmail, "@") {
		return fmt.Errorf("a valid email address is required")
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var ph string
	if err = tx.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id=? AND account_id=?`, userID, accountID).Scan(&ph); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return err
	}
	if !auth.CheckPassword(ph, currentPassword) {
		return ErrForbidden
	}
	var other string
	err = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE email=? AND id!=?`, newEmail, userID).Scan(&other)
	if err == nil {
		return ErrConflict
	}
	if err != sql.ErrNoRows {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE users SET email=? WHERE id=? AND account_id=?`, newEmail, userID, accountID); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateUserPassword verifies the current password, stores a new hash and
// revokes every other session for the user so a compromised session cannot
// survive a password change. keepSession is the raw session token to retain
// (the one making the change); pass "" to revoke all sessions.
func (s *Store) UpdateUserPassword(ctx context.Context, userID, currentPassword, newPassword, keepSession string) error {
	var ph string
	err := s.read.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id=?`, userID).Scan(&ph)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !auth.CheckPassword(ph, currentPassword) {
		return ErrForbidden
	}
	newHash, err := auth.HashPassword(newPassword)
	if err != nil {
		return err
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, newHash, userID); err != nil {
		return err
	}
	if keepSession != "" {
		if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=? AND id_hash!=?`, userID, auth.HashToken(keepSession)); err != nil {
			return err
		}
	} else if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// AdminResetPassword replaces a user's password without verifying the old one,
// for operator-driven recovery from the command line. It revokes every
// interactive session for the user and records a security audit event, but
// deliberately leaves API keys untouched because they may represent
// independent machine integrations. It returns ErrNotFound when no such user
// exists.
func (s *Store) AdminResetPassword(ctx context.Context, userID, newPassword string) error {
	newHash, err := auth.HashPassword(newPassword)
	if err != nil {
		return err
	}
	tx, err := s.write.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var accountID, email string
	var sysadmin int
	if err = tx.QueryRowContext(ctx, `SELECT account_id,email,is_system_admin FROM users WHERE id=?`, userID).Scan(&accountID, &email, &sysadmin); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return err
	}
	// The system administrator's credentials are owned by the deployment
	// configuration. A database reset would be silently overwritten on the next
	// restart, so refuse it and send the operator to the config instead.
	if sysadmin != 0 {
		return ErrSystemAdmin
	}
	if _, err = tx.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, newHash, userID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_log(account_id,kind,detail,created_at) VALUES(?,?,?,?)`, accountID, "admin.password_reset", email, nowText()); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeAPIKeysForAccount revokes every active API key on an account. It backs
// the operator `admin revoke-api-keys` recovery command, kept separate from a
// password reset so machine integrations are only torn down on request.
func (s *Store) RevokeAPIKeysForAccount(ctx context.Context, accountID string) (int64, error) {
	res, err := s.write.ExecContext(ctx, `UPDATE api_keys SET revoked_at=? WHERE account_id=? AND revoked_at IS NULL`, nowText(), accountID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *Store) CreateSession(ctx context.Context, userID string, ttl time.Duration) (token, csrf string, err error) {
	token, err = auth.RandomToken(32)
	if err != nil {
		return
	}
	csrf, err = auth.RandomToken(24)
	if err != nil {
		return
	}
	_, err = s.write.ExecContext(ctx, `INSERT INTO sessions(id_hash,user_id,csrf_token,expires_at,created_at) VALUES(?,?,?,?,?)`, auth.HashToken(token), userID, csrf, timeText(time.Now().UTC().Add(ttl)), nowText())
	return
}
func (s *Store) DeleteSession(ctx context.Context, token string) {
	_, _ = s.write.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash=?`, auth.HashToken(token))
}
func (s *Store) SessionPrincipal(ctx context.Context, token string) (model.Principal, string, error) {
	var p model.Principal
	var csrf, exp string
	var admin, sysadmin int
	err := s.read.QueryRowContext(ctx, `SELECT u.account_id,u.id,u.is_admin,u.is_system_admin,s.csrf_token,s.expires_at FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.id_hash=?`, auth.HashToken(token)).Scan(&p.AccountID, &p.UserID, &admin, &sysadmin, &csrf, &exp)
	if err == sql.ErrNoRows {
		return p, "", ErrNotFound
	}
	if err != nil {
		return p, "", err
	}
	if parseTime(exp).Before(time.Now().UTC()) {
		s.DeleteSession(ctx, token)
		return p, "", ErrNotFound
	}
	p.Admin = admin != 0
	p.SystemAdmin = sysadmin != 0
	p.ViaSession = true
	p.SessionHash = auth.HashToken(token)
	p.MailboxRoles = map[string]string{}
	if !p.Admin {
		roles, err := s.userMailboxRoles(ctx, p.UserID)
		if err != nil {
			return p, "", err
		}
		p.MailboxRoles = roles
	}
	return p, csrf, nil
}

// userMailboxRoles returns a non-admin user's per-inbox roles. A user with no
// assignments returns an empty map, which grants access to no mailbox.
func (s *Store) userMailboxRoles(ctx context.Context, userID string) (map[string]string, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT inbox_id,role FROM user_mailbox_roles WHERE user_id=?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var inboxID, role string
		if err = rows.Scan(&inboxID, &role); err != nil {
			return nil, err
		}
		out[inboxID] = role
	}
	return out, rows.Err()
}

func (s *Store) CreateAPIKey(ctx context.Context, accountID, name string, admin bool, roles map[string]string) (model.APIKey, string, error) {
	plain, err := auth.RandomToken(32)
	if err != nil {
		return model.APIKey{}, "", err
	}
	plain = "mmm_" + plain
	id := idgen.New("key")
	prefix := plain
	if len(prefix) > 14 {
		prefix = prefix[:14]
	}
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return model.APIKey{}, "", err
	}
	defer tx.Rollback()
	now := nowText()
	if _, err = tx.ExecContext(ctx, `INSERT INTO api_keys(id,account_id,name,key_prefix,key_hash,is_admin,created_at) VALUES(?,?,?,?,?,?,?)`, id, accountID, strings.TrimSpace(name), prefix, auth.HashToken(plain), boolInt(admin), now); err != nil {
		return model.APIKey{}, "", err
	}
	if !admin {
		for inboxID, role := range roles {
			role = strings.ToLower(role)
			if role != "read" && role != "assistant" && role != "owner" {
				return model.APIKey{}, "", fmt.Errorf("invalid role %q", role)
			}
			var n int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&n); err != nil || n != 1 {
				return model.APIKey{}, "", ErrForbidden
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO api_key_mailbox_roles(api_key_id,inbox_id,role) VALUES(?,?,?)`, id, inboxID, role); err != nil {
				return model.APIKey{}, "", err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return model.APIKey{}, "", err
	}
	return model.APIKey{ID: id, Name: name, Prefix: prefix, Admin: admin, Roles: roles, CreatedAt: parseTime(now)}, plain, nil
}

func (s *Store) APIKeyPrincipal(ctx context.Context, token string) (model.Principal, error) {
	var p model.Principal
	var revoked sql.NullString
	var admin int
	hash := auth.HashToken(token)
	err := s.read.QueryRowContext(ctx, `SELECT id,account_id,is_admin,revoked_at FROM api_keys WHERE key_hash=?`, hash).Scan(&p.APIKeyID, &p.AccountID, &admin, &revoked)
	if err == sql.ErrNoRows {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	if revoked.Valid {
		return p, ErrNotFound
	}
	p.Admin = admin != 0
	p.MailboxRoles = map[string]string{}
	if !p.Admin {
		rows, err := s.read.QueryContext(ctx, `SELECT inbox_id,role FROM api_key_mailbox_roles WHERE api_key_id=?`, p.APIKeyID)
		if err != nil {
			return p, err
		}
		defer rows.Close()
		for rows.Next() {
			var id, role string
			if err = rows.Scan(&id, &role); err != nil {
				return p, err
			}
			p.MailboxRoles[id] = role
		}
	}
	_, _ = s.write.ExecContext(ctx, `UPDATE api_keys SET last_used_at=? WHERE id=?`, nowText(), p.APIKeyID)
	return p, nil
}
func (s *Store) ListAPIKeys(ctx context.Context, accountID string) ([]model.APIKey, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT k.id,k.name,k.key_prefix,k.is_admin,k.created_at,r.inbox_id,r.role FROM api_keys k LEFT JOIN api_key_mailbox_roles r ON r.api_key_id=k.id WHERE k.account_id=? AND k.revoked_at IS NULL ORDER BY k.created_at DESC, k.id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.APIKey{}
	index := map[string]int{}
	for rows.Next() {
		var k model.APIKey
		var admin int
		var created string
		var inboxID, role sql.NullString
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &admin, &created, &inboxID, &role); err != nil {
			return nil, err
		}
		idx, ok := index[k.ID]
		if !ok {
			k.Admin = admin != 0
			k.CreatedAt = parseTime(created)
			k.Roles = map[string]string{}
			out = append(out, k)
			idx = len(out) - 1
			index[k.ID] = idx
		}
		if inboxID.Valid && role.Valid {
			out[idx].Roles[inboxID.String] = role.String
		}
	}
	return out, rows.Err()
}
func (s *Store) RevokeAPIKey(ctx context.Context, accountID, keyID string) error {
	res, err := s.write.ExecContext(ctx, `UPDATE api_keys SET revoked_at=? WHERE id=? AND account_id=?`, nowText(), keyID, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RotateAPIKey issues a new secret for an existing key while keeping its id,
// name, and permissions. The previous secret stops working immediately.
func (s *Store) RotateAPIKey(ctx context.Context, accountID, keyID string) (string, error) {
	plain, err := auth.RandomToken(32)
	if err != nil {
		return "", err
	}
	plain = "mmm_" + plain
	prefix := plain
	if len(prefix) > 14 {
		prefix = prefix[:14]
	}
	res, err := s.write.ExecContext(ctx, `UPDATE api_keys SET key_hash=?, key_prefix=?, last_used_at=NULL WHERE id=? AND account_id=? AND revoked_at IS NULL`, auth.HashToken(plain), prefix, keyID, accountID)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrNotFound
	}
	return plain, nil
}

// UpdateAPIKey changes a key's name and permissions. Admin keys carry no
// per-mailbox roles, so any supplied roles are ignored and cleared for them.
func (s *Store) UpdateAPIKey(ctx context.Context, accountID, keyID, name string, admin bool, roles map[string]string) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE api_keys SET name=?, is_admin=? WHERE id=? AND account_id=? AND revoked_at IS NULL`, strings.TrimSpace(name), boolInt(admin), keyID, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM api_key_mailbox_roles WHERE api_key_id=?`, keyID); err != nil {
		return err
	}
	if !admin {
		for inboxID, role := range roles {
			role = strings.ToLower(role)
			if role != "read" && role != "assistant" && role != "owner" {
				return fmt.Errorf("invalid role %q", role)
			}
			var n int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&n); err != nil || n != 1 {
				return ErrForbidden
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO api_key_mailbox_roles(api_key_id,inbox_id,role) VALUES(?,?,?)`, keyID, inboxID, role); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
