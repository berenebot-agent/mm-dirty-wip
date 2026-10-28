package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/dellarb/mammothmail/internal/model"
)

type OneImportSetup struct {
	ID, AccountID, UserID, EncryptedKey, Fingerprint string
}

func (s *Store) SaveOneImportSetup(ctx context.Context, setup OneImportSetup) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM one_import_setups WHERE expires_at<=? OR (account_id=? AND user_id=?)`, nowText(), setup.AccountID, setup.UserID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO one_import_setups(id,account_id,user_id,encrypted_key,fingerprint,expires_at) SELECT ?,account_id,id,?,?,? FROM users WHERE account_id=? AND id=? AND is_admin=1`, setup.ID, setup.EncryptedKey, setup.Fingerprint, time.Now().UTC().Add(30*time.Minute).Format(time.RFC3339Nano), setup.AccountID, setup.UserID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrForbidden
	}
	return tx.Commit()
}

func (s *Store) OneImportSetup(ctx context.Context, account, user, id string) (OneImportSetup, error) {
	var setup OneImportSetup
	err := s.read.QueryRowContext(ctx, `SELECT id,account_id,user_id,encrypted_key,fingerprint FROM one_import_setups WHERE id=? AND account_id=? AND user_id=? AND expires_at>?`, id, account, user, nowText()).Scan(&setup.ID, &setup.AccountID, &setup.UserID, &setup.EncryptedKey, &setup.Fingerprint)
	if err == sql.ErrNoRows {
		err = ErrNotFound
	}
	return setup, err
}

func (s *Store) DeleteOneImportSetup(ctx context.Context, account, user, id string) error {
	_, err := s.write.ExecContext(ctx, `DELETE FROM one_import_setups WHERE account_id=? AND user_id=? AND id=?`, account, user, id)
	return err
}

func (s *Store) ListOneBindings(ctx context.Context, account string) ([]OneBinding, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT inbox_id FROM inbox_one_bindings WHERE account_id=? ORDER BY inbox_id`, account)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []OneBinding{}
	for _, id := range ids {
		b, err := s.OneBinding(ctx, account, id)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

func (s *Store) SaveOneBinding(ctx context.Context, b OneBinding, expected int64) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE inbox_one_bindings SET encrypted_key=?,key_aad=?,fingerprint=?,encrypted_connection_key=?,status='active',last_error='',failure_count=0,last_checked_at=?,revision=revision+1 WHERE account_id=? AND inbox_id=? AND revision=? AND connection_id=? AND gmail_email=?`, b.EncryptedKey, b.KeyAAD, b.Fingerprint, b.EncryptedConnectionKey, nowText(), b.AccountID, b.InboxID, expected, b.ConnectionID, b.GmailEmail)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE inboxes SET remote_configured=1 WHERE account_id=? AND id=?`, b.AccountID, b.InboxID); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteOneInbox removes a One-bound standalone inbox and every row the One
// import created for it, in one transaction. The binding, its Gmail sync
// progress and the detection row are account-scoped by inbox id and are deleted
// here so no credential or progress row outlives the inbox; the inbox row itself
// goes last so the ON DELETE CASCADE children (messages, folders, credentials)
// follow it.
//
// It deliberately does not touch anything in One or Gmail. Deleting the inbox is
// the supported way to detach a One connection: with the connection gone, the
// address is free to be imported again from the One setup screen.
func (s *Store) DeleteOneInbox(ctx context.Context, account, inbox string) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM inbox_one_bindings WHERE account_id=? AND inbox_id=?`, account, inbox)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM inbox_gmail_sync WHERE account_id=? AND inbox_id=?`, account, inbox); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM google_detection WHERE inbox_id=?`, inbox); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM inboxes WHERE account_id=? AND id=?`, account, inbox); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ImportOneInbox(ctx context.Context, b OneBinding, history string) (string, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	address, err := ValidateStandaloneAddress(b.GmailEmail)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(b.DisplayName)
	if name == "" {
		name = address
	}
	var existing, email string
	err = tx.QueryRowContext(ctx, `SELECT inbox_id,gmail_email FROM inbox_one_bindings WHERE account_id=? AND connection_id=?`, b.AccountID, b.ConnectionID).Scan(&existing, &email)
	if err == nil {
		if email != address {
			return "", ErrConflict
		}
		return existing, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	var collision int
	err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM inboxes WHERE account_id=? AND address=? COLLATE NOCASE)+(SELECT count(*) FROM inboxes i JOIN domains d ON d.id=i.domain_id WHERE i.account_id=? AND (i.local_part||'@'||d.name)=? COLLATE NOCASE)+(SELECT count(*) FROM inbox_aliases a JOIN domains d ON d.id=a.domain_id WHERE a.account_id=? AND (a.local_part||'@'||d.name)=? COLLATE NOCASE)`, b.AccountID, address, b.AccountID, address, b.AccountID, address).Scan(&collision)
	if err != nil {
		return "", err
	}
	if collision != 0 || b.EncryptedKey == "" || b.EncryptedConnectionKey == "" || b.InboxID == "" || b.KeyAAD == "" || b.Fingerprint == "" {
		return "", ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO inboxes(id,account_id,kind,domain_id,local_part,address,display_name,namespace,remote_host,remote_username,remote_security,remote_configured,remote_sent_copy_enabled,remote_poll_seconds,delivery_trigger,storage_used_bytes,created_at) VALUES(?,?,'standalone',NULL,'',?,?,'INBOX','api.withone.ai',?,'tls',1,0,300,?,0,?)`, b.InboxID, b.AccountID, address, name, address, DeliveryTriggerDefault, nowText()); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_remote_credentials(inbox_id,account_id,revision,created_at,updated_at) VALUES(?,?,0,?,?)`, b.InboxID, b.AccountID, nowText(), nowText()); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_one_bindings(inbox_id,account_id,connection_id,encrypted_connection_key,encrypted_key,key_aad,fingerprint,rate_group,gmail_email,last_checked_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, b.InboxID, b.AccountID, b.ConnectionID, b.EncryptedConnectionKey, b.EncryptedKey, b.KeyAAD, b.Fingerprint, b.RateGroup, address, nowText()); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO inbox_gmail_sync(inbox_id,account_id,history_id) VALUES(?,?,?)`, b.InboxID, b.AccountID, history); err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO google_detection(inbox_id,history_id) VALUES(?,?)`, b.InboxID, history); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return b.InboxID, err
	}
	s.logInbox(ctx, "inbox.created", model.Inbox{ID: b.InboxID, AccountID: b.AccountID, Kind: model.InboxKindStandalone, Address: address}, "one_import", "")
	return b.InboxID, nil
}

func (s *Store) UpdateOneDisplayName(ctx context.Context, account, inbox, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	_, err := s.write.ExecContext(ctx, `UPDATE inboxes SET display_name=? WHERE account_id=? AND id=? AND (display_name=address OR display_name=address||' · Gmail via One') AND EXISTS(SELECT 1 FROM inbox_one_bindings b WHERE b.inbox_id=inboxes.id AND b.account_id=inboxes.account_id)`, name, account, inbox)
	return err
}
