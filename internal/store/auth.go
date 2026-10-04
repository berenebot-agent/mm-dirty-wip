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
	"github.com/dellarb/mailmoose/internal/timezone"
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
// with the database. The configured email identifies the system administrator:
//
//   - If a user already has that email, it is adopted in place — it keeps its
//     account and is forced to account Admin and system Admin. This lets the
//     operator point ADMIN_EMAIL at an existing login.
//   - Otherwise, an existing system administrator is renamed to the new email.
//   - Otherwise, a new account and Admin user are created.
//
// The configured password is applied when it differs, and the affected user's
// sessions are revoked whenever anything changes, so a rotated deployment
// secret takes effect on restart. The error never contains the password.
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

	// The current system administrator, if any.
	var sysUID, sysAccount, sysEmail, sysHash, sysCreated string
	var sysAdmin int
	sysErr := tx.QueryRowContext(ctx, `SELECT id,account_id,email,password_hash,is_admin,created_at FROM users WHERE is_system_admin=1 ORDER BY created_at LIMIT 1`).Scan(&sysUID, &sysAccount, &sysEmail, &sysHash, &sysAdmin, &sysCreated)
	if sysErr != nil && sysErr != sql.ErrNoRows {
		return model.User{}, false, sysErr
	}
	// A user already using the configured email, if any.
	var byEmailUID, byEmailAccount, byEmailHash, byEmailCreated string
	var byEmailAdmin int
	emailErr := tx.QueryRowContext(ctx, `SELECT id,account_id,password_hash,is_admin,created_at FROM users WHERE email=?`, email).Scan(&byEmailUID, &byEmailAccount, &byEmailHash, &byEmailAdmin, &byEmailCreated)
	if emailErr != nil && emailErr != sql.ErrNoRows {
		return model.User{}, false, emailErr
	}

	if emailErr == sql.ErrNoRows && sysErr == sql.ErrNoRows {
		// Fresh installation: create the system administrator's own account.
		aid, uid := idgen.New("acct"), idgen.New("usr")
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

	// Resolve the target user: the existing owner of the configured email when
	// there is one, otherwise the current system administrator (renamed).
	targetUID, targetAccount, targetHash, targetCreated, targetAdmin := byEmailUID, byEmailAccount, byEmailHash, byEmailCreated, byEmailAdmin
	targetEmail := email
	if emailErr == sql.ErrNoRows {
		targetUID, targetAccount, targetHash, targetCreated, targetAdmin = sysUID, sysAccount, sysHash, sysCreated, sysAdmin
		targetEmail = sysEmail
	}

	passwordChanged := !auth.CheckPassword(targetHash, password)
	mustBeAdmin := targetAdmin == 0
	// The target gains the system-administrator role unless it already held it.
	grantSystem := sysErr == sql.ErrNoRows || sysUID != targetUID
	emailChanged := targetEmail != email
	// A pre-existing system administrator other than the target loses the role.
	demote := sysErr == nil && sysUID != targetUID

	set := []string{"is_admin=1", "is_system_admin=1", "password_auth_enabled=1"}
	if emailChanged {
		set = append(set, "email=?")
	}
	if passwordChanged {
		set = append(set, "password_hash=?")
	}
	args := []any{}
	if emailChanged {
		args = append(args, email)
	}
	if passwordChanged {
		args = append(args, ph)
	}
	args = append(args, targetUID)
	if _, err = tx.ExecContext(ctx, `UPDATE users SET `+strings.Join(set, ",")+` WHERE id=?`, args...); err != nil {
		return model.User{}, false, err
	}
	var demoteUID string
	if demote {
		if _, err = tx.ExecContext(ctx, `UPDATE users SET is_system_admin=0 WHERE id=?`, sysUID); err != nil {
			return model.User{}, false, err
		}
		demoteUID = sysUID
	}
	changed := grantSystem || mustBeAdmin || emailChanged || passwordChanged || demote
	if changed {
		if demoteUID != "" {
			if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id IN (?,?)`, targetUID, demoteUID); err != nil {
				return model.User{}, false, err
			}
		} else if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, targetUID); err != nil {
			return model.User{}, false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return model.User{}, false, err
	}
	return model.User{ID: targetUID, AccountID: targetAccount, Email: email, IsAdmin: true, SystemAdmin: true, CreatedAt: parseTime(targetCreated)}, changed, nil
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
	var admin, sysadmin, pwEnabled int
	err := s.read.QueryRowContext(ctx, `SELECT id,account_id,email,password_hash,is_admin,is_system_admin,created_at,timezone,password_auth_enabled FROM users WHERE email=?`, normalizeAddress(email)).Scan(&u.ID, &u.AccountID, &u.Email, &ph, &admin, &sysadmin, &created, &u.Timezone, &pwEnabled)
	if err == sql.ErrNoRows {
		// Equalize the work done for an unknown account so login timing cannot
		// be used to enumerate accounts.
		auth.DummyPasswordCheck(password)
		return model.User{}, ErrNotFound
	}
	if err != nil {
		return model.User{}, err
	}
	u.PasswordEnabled = pwEnabled != 0
	if !u.PasswordEnabled {
		// A passkey-only user has no password. Equalize timing and fail the
		// password path; the passkey ceremony is their login method.
		auth.DummyPasswordCheck(password)
		return model.User{}, ErrNotFound
	}
	if !auth.CheckPassword(ph, password) {
		return model.User{}, ErrNotFound
	}
	// Lazy migration: a successful verify of a legacy pbkdf2-sha256 hash (or an
	// Argon2id hash below the current cost) upgrades the stored hash to the
	// current algorithm. Best-effort; a failure only means the row is upgraded
	// on a later login.
	if auth.NeedsRehash(ph) {
		if upgraded, herr := auth.HashPassword(password); herr == nil {
			_, _ = s.write.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=? AND password_hash=?`, upgraded, u.ID, ph)
		}
	}
	u.IsAdmin = admin != 0
	u.SystemAdmin = sysadmin != 0
	u.CreatedAt = parseTime(created)
	return u, nil
}

func (s *Store) GetUser(ctx context.Context, userID string) (model.User, error) {
	var u model.User
	var created string
	var admin, sysadmin, pwEnabled int
	err := s.read.QueryRowContext(ctx, `SELECT id,account_id,email,is_admin,is_system_admin,created_at,timezone,password_auth_enabled FROM users WHERE id=?`, userID).Scan(&u.ID, &u.AccountID, &u.Email, &admin, &sysadmin, &created, &u.Timezone, &pwEnabled)
	if err == sql.ErrNoRows {
		return model.User{}, ErrNotFound
	}
	if err != nil {
		return model.User{}, err
	}
	u.IsAdmin = admin != 0
	u.SystemAdmin = sysadmin != 0
	u.PasswordEnabled = pwEnabled != 0
	u.CreatedAt = parseTime(created)
	return u, nil
}

// GetUserByEmail looks a user up by login address, used by the operator
// password-reset command.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (model.User, error) {
	var u model.User
	var created string
	var admin, sysadmin, pwEnabled int
	err := s.read.QueryRowContext(ctx, `SELECT id,account_id,email,is_admin,is_system_admin,created_at,timezone,password_auth_enabled FROM users WHERE email=?`, normalizeAddress(email)).Scan(&u.ID, &u.AccountID, &u.Email, &admin, &sysadmin, &created, &u.Timezone, &pwEnabled)
	if err == sql.ErrNoRows {
		return model.User{}, ErrNotFound
	}
	if err != nil {
		return model.User{}, err
	}
	u.IsAdmin = admin != 0
	u.SystemAdmin = sysadmin != 0
	u.PasswordEnabled = pwEnabled != 0
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
	if _, err = tx.ExecContext(ctx, `UPDATE users SET password_hash=?,password_auth_enabled=1 WHERE id=?`, newHash, userID); err != nil {
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
	if _, err = tx.ExecContext(ctx, `UPDATE users SET password_hash=?,password_auth_enabled=1 WHERE id=?`, newHash, userID); err != nil {
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
	now := nowText()
	res, err := s.write.ExecContext(ctx, `UPDATE clients SET revoked_at=? WHERE account_id=? AND type='api_key' AND revoked_at IS NULL`, now, accountID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err = s.write.ExecContext(ctx, `UPDATE api_keys SET revoked_at=? WHERE account_id=? AND revoked_at IS NULL`, now, accountID); err != nil {
		return 0, err
	}
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
	var csrf, exp, userTZ, acctTZ string
	var admin, sysadmin int
	err := s.read.QueryRowContext(ctx, `SELECT u.account_id,u.id,u.is_admin,u.is_system_admin,s.csrf_token,s.expires_at,u.timezone,a.timezone FROM sessions s JOIN users u ON u.id=s.user_id JOIN accounts a ON a.id=u.account_id WHERE s.id_hash=?`, auth.HashToken(token)).Scan(&p.AccountID, &p.UserID, &admin, &sysadmin, &csrf, &exp, &userTZ, &acctTZ)
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
	p.Timezone = timezone.Resolve(userTZ, acctTZ).String()
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO clients(id,account_id,type,name,created_at) VALUES(?,?,'api_key',?,?)`, id, accountID, strings.TrimSpace(name), now); err != nil {
		return model.APIKey{}, "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO client_api_keys(client_id,key_prefix,key_hash,is_admin) VALUES(?,?,?,?)`, id, prefix, auth.HashToken(plain), boolInt(admin)); err != nil {
		return model.APIKey{}, "", err
	}
	// Mirror into the legacy table before the role rows, which carry a
	// foreign key to it, so the dual-write keeps both schemas valid.
	if err = s.syncLegacyAPIKey(ctx, tx, id); err != nil {
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
			if _, err = tx.ExecContext(ctx, `INSERT INTO client_inbox_bindings(client_id,inbox_id,role) VALUES(?,?,?)`, id, inboxID, role); err != nil {
				return model.APIKey{}, "", err
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
	err := s.read.QueryRowContext(ctx, `SELECT c.id,c.account_id,k.is_admin,c.revoked_at FROM clients c JOIN client_api_keys k ON k.client_id=c.id WHERE k.key_hash=? AND c.type='api_key'`, hash).Scan(&p.APIKeyID, &p.AccountID, &admin, &revoked)
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
		rows, err := s.read.QueryContext(ctx, `SELECT inbox_id,role FROM client_inbox_bindings WHERE client_id=?`, p.APIKeyID)
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
	_, _ = s.write.ExecContext(ctx, `UPDATE client_api_keys SET last_used_at=? WHERE client_id=?`, nowText(), p.APIKeyID)
	return p, nil
}
func (s *Store) ListAPIKeys(ctx context.Context, accountID string) ([]model.APIKey, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT c.id,c.name,k.key_prefix,k.is_admin,c.created_at,r.inbox_id,r.role FROM clients c JOIN client_api_keys k ON k.client_id=c.id LEFT JOIN client_inbox_bindings r ON r.client_id=c.id WHERE c.account_id=? AND c.type='api_key' AND c.revoked_at IS NULL ORDER BY c.created_at DESC, c.id`, accountID)
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
	res, err := s.write.ExecContext(ctx, `UPDATE clients SET revoked_at=? WHERE id=? AND account_id=? AND type='api_key'`, nowText(), keyID, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	_, err = s.write.ExecContext(ctx, `UPDATE api_keys SET revoked_at=? WHERE id=? AND account_id=?`, nowText(), keyID, accountID)
	return err
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
	res, err := s.write.ExecContext(ctx, `UPDATE client_api_keys SET key_hash=?, key_prefix=?, last_used_at=NULL WHERE client_id=? AND EXISTS (SELECT 1 FROM clients c WHERE c.id=client_api_keys.client_id AND c.account_id=? AND c.type='api_key' AND c.revoked_at IS NULL)`, auth.HashToken(plain), prefix, keyID, accountID)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrNotFound
	}
	_, err = s.write.ExecContext(ctx, `UPDATE api_keys SET key_hash=?,key_prefix=?,last_used_at=NULL WHERE id=? AND account_id=?`, auth.HashToken(plain), prefix, keyID, accountID)
	if err != nil {
		return "", err
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
	res, err := tx.ExecContext(ctx, `UPDATE clients SET name=? WHERE id=? AND account_id=? AND type='api_key' AND revoked_at IS NULL`, strings.TrimSpace(name), keyID, accountID)
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE client_api_keys SET is_admin=? WHERE client_id=?`, boolInt(admin), keyID)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM client_inbox_bindings WHERE client_id=?`, keyID); err != nil {
		return err
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
			if _, err = tx.ExecContext(ctx, `INSERT INTO client_inbox_bindings(client_id,inbox_id,role) VALUES(?,?,?)`, keyID, inboxID, role); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO api_key_mailbox_roles(api_key_id,inbox_id,role) VALUES(?,?,?)`, keyID, inboxID, role); err != nil {
				return err
			}
		}
	}
	if err = s.syncLegacyAPIKey(ctx, tx, keyID); err != nil {
		return err
	}
	return tx.Commit()
}
