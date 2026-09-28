package store

import (
	"context"
	"database/sql"
)

func (s *Store) syncLegacyAPIKey(ctx context.Context, tx *sql.Tx, id string) error {
	var accountID, name, prefix, hash, created string
	var admin int
	var revoked, lastUsed sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT c.account_id,c.name,k.key_prefix,k.key_hash,k.is_admin,c.created_at,c.revoked_at,k.last_used_at FROM clients c JOIN client_api_keys k ON k.client_id=c.id WHERE c.id=?`, id).Scan(&accountID, &name, &prefix, &hash, &admin, &created, &revoked, &lastUsed)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO api_keys(id,account_id,name,key_prefix,key_hash,is_admin,created_at,last_used_at,revoked_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,key_prefix=excluded.key_prefix,key_hash=excluded.key_hash,is_admin=excluded.is_admin,revoked_at=excluded.revoked_at,last_used_at=excluded.last_used_at`, id, accountID, name, prefix, hash, admin, created, lastUsed, revoked)
	return err
}
