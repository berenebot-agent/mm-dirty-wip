package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/dellarb/mailmoose/internal/idgen"
	"github.com/dellarb/mailmoose/internal/model"
)

// ErrLastAuthMethod is returned when deleting a passkey would leave the user
// with no way to sign in (no password and no other passkey).
var ErrLastAuthMethod = errors.New("cannot remove the only sign-in method")

// WebAuthnCredentialsForUser returns a user's registered passkeys, newest
// first, for display on the account page. No secret material beyond the public
// key is included, and the public key is not marshalled to JSON.
func (s *Store) WebAuthnCredentialsForUser(ctx context.Context, userID string) ([]model.WebAuthnCredential, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT id,user_id,credential_id,public_key,sign_count,transports,name,created_at,last_used_at FROM webauthn_credentials WHERE user_id=? ORDER BY created_at DESC, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.WebAuthnCredential{}
	for rows.Next() {
		var c model.WebAuthnCredential
		var transports, created string
		var lastUsed sql.NullString
		if err := rows.Scan(&c.ID, &c.UserID, &c.CredentialID, &c.PublicKey, &c.SignCount, &transports, &c.Name, &created, &lastUsed); err != nil {
			return nil, err
		}
		c.Transports = splitTransports(transports)
		c.CreatedAt = parseTime(created)
		if lastUsed.Valid && lastUsed.String != "" {
			c.LastUsedAt = parseTime(lastUsed.String)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// WebAuthnCredentialByCredentialID resolves a raw authenticator credential id
// to its stored row and owning user. It is used during the login ceremony to
// find the public key to verify against.
func (s *Store) WebAuthnCredentialByCredentialID(ctx context.Context, credentialID []byte) (model.WebAuthnCredential, error) {
	var c model.WebAuthnCredential
	var transports, created string
	var lastUsed sql.NullString
	err := s.read.QueryRowContext(ctx, `SELECT id,user_id,credential_id,public_key,sign_count,transports,name,created_at,last_used_at FROM webauthn_credentials WHERE credential_id=?`, credentialID).
		Scan(&c.ID, &c.UserID, &c.CredentialID, &c.PublicKey, &c.SignCount, &transports, &c.Name, &created, &lastUsed)
	if err == sql.ErrNoRows {
		return model.WebAuthnCredential{}, ErrNotFound
	}
	if err != nil {
		return model.WebAuthnCredential{}, err
	}
	c.Transports = splitTransports(transports)
	c.CreatedAt = parseTime(created)
	if lastUsed.Valid && lastUsed.String != "" {
		c.LastUsedAt = parseTime(lastUsed.String)
	}
	return c, nil
}

// AddWebAuthnCredential stores a freshly registered passkey for a user. The
// credential id is unique across the installation; a duplicate insert is a
// conflict rather than silently overwriting an existing key.
func (s *Store) AddWebAuthnCredential(ctx context.Context, userID string, c model.WebAuthnCredential, attestationType, aaguid string, backupEligible, backupState bool) error {
	if len(c.CredentialID) == 0 || len(c.PublicKey) == 0 {
		return fmt.Errorf("invalid webauthn credential")
	}
	name := strings.TrimSpace(c.Name)
	if name == "" {
		name = "Passkey"
	}
	_, err := s.write.ExecContext(ctx,
		`INSERT INTO webauthn_credentials(id,user_id,credential_id,public_key,attestation_type,aaguid,sign_count,transports,name,backup_eligible,backup_state,created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		idgen.New("wac"), userID, c.CredentialID, c.PublicKey, attestationType, aaguid, c.SignCount,
		strings.Join(c.Transports, ","), name, boolInt(backupEligible), boolInt(backupState), nowText())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrConflict
		}
		return err
	}
	return nil
}

// UpdateWebAuthnCredentialUse records a successful assertion: the new signature
// counter and backup state, plus the last-used time. A counter that did not
// advance (or regressed) is a possible cloned-authenticator signal; callers may
// use the returned bool to warn, but the update is still persisted so a
// legitimate backup-synced passkey keeps working.
func (s *Store) UpdateWebAuthnCredentialUse(ctx context.Context, credentialID []byte, signCount uint32, backupState bool) (cloneWarning bool, err error) {
	var current uint32
	if err := s.read.QueryRowContext(ctx, `SELECT sign_count FROM webauthn_credentials WHERE credential_id=?`, credentialID).Scan(&current); err != nil {
		if err == sql.ErrNoRows {
			return false, ErrNotFound
		}
		return false, err
	}
	cloneWarning = signCount != 0 && current != 0 && signCount <= current
	_, err = s.write.ExecContext(ctx, `UPDATE webauthn_credentials SET sign_count=?,backup_state=?,last_used_at=? WHERE credential_id=?`,
		signCount, boolInt(backupState), nowText(), credentialID)
	return cloneWarning, err
}

// RenameWebAuthnCredential changes the human-readable label shown on the
// account page. It is scoped to the owning user.
func (s *Store) RenameWebAuthnCredential(ctx context.Context, userID, credentialRowID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("passkey name is required")
	}
	if len(name) > 80 {
		return fmt.Errorf("passkey name must be 80 characters or fewer")
	}
	res, err := s.write.ExecContext(ctx, `UPDATE webauthn_credentials SET name=? WHERE id=? AND user_id=?`, name, credentialRowID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteWebAuthnCredential removes one passkey owned by the user. It refuses
// when the passkey is the user's only remaining way to sign in (no password and
// no other passkey), so a user cannot lock themselves out.
func (s *Store) DeleteWebAuthnCredential(ctx context.Context, userID, credentialRowID string) error {
	tx, err := s.write.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var pwEnabled int
	if err = tx.QueryRowContext(ctx, `SELECT password_auth_enabled FROM users WHERE id=?`, userID).Scan(&pwEnabled); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM webauthn_credentials WHERE user_id=?`, userID).Scan(&count); err != nil {
		return err
	}
	if pwEnabled == 0 && count <= 1 {
		return ErrLastAuthMethod
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM webauthn_credentials WHERE id=? AND user_id=?`, credentialRowID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// UserLoginMethods describes how a user can sign in, used by the account page
// and by recovery logic that needs to know whether a password exists.
type UserLoginMethods struct {
	PasswordEnabled bool
	PasskeyCount    int
}

// LoginMethods reports the user's available sign-in methods.
func (s *Store) LoginMethods(ctx context.Context, userID string) (UserLoginMethods, error) {
	var m UserLoginMethods
	var pwEnabled int
	if err := s.read.QueryRowContext(ctx, `SELECT password_auth_enabled FROM users WHERE id=?`, userID).Scan(&pwEnabled); err != nil {
		if err == sql.ErrNoRows {
			return m, ErrNotFound
		}
		return m, err
	}
	m.PasswordEnabled = pwEnabled != 0
	if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM webauthn_credentials WHERE user_id=?`, userID).Scan(&m.PasskeyCount); err != nil {
		return m, err
	}
	return m, nil
}

func splitTransports(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// SetPasswordAuth enables or disables password authentication for a user,
// scoped to the owning account. Disabling it makes the account passkey-only; it
// refuses when the user has no passkey, so an account can never be left with no
// way to sign in. It is the store primitive behind the "make this my only
// sign-in method" flow.
func (s *Store) SetPasswordAuth(ctx context.Context, userID, accountID string, enabled bool) error {
	if !enabled {
		var passkeys int
		if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM webauthn_credentials WHERE user_id=?`, userID).Scan(&passkeys); err != nil {
			return err
		}
		if passkeys == 0 {
			return ErrLastAuthMethod
		}
	}
	res, err := s.write.ExecContext(ctx, `UPDATE users SET password_auth_enabled=? WHERE id=? AND account_id=?`, boolInt(enabled), userID, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
