package store

import (
	"context"
	"database/sql"
	"errors"
)

// MXSettings is the single installation-wide MX receiver configuration. Before
// this table existed the receiver mode, remote URL and shared bearer key came
// from the process environment (MX_ENABLE, MX_RECEIVER_URL, DIALMX_CORE_KEY);
// those are now imported once and thereafter this row is authoritative.
//
// The row is a singleton: ID is always "mx". Mode and ReceiverURL are stored in
// cleartext so a status read (which must not need the application key) can name
// the configured mode. EncryptedSecret holds the encrypted bearer credential
// and EncryptedConfig holds the encrypted auxiliary configuration (a private CA
// bundle and any SMTP limit overrides); both are meaningless without the
// application key and are redacted on read. Revision is the CAS token a save
// must present and the runtime reconciliation generation.
type MXSettings struct {
	ID              string
	Mode            string
	ReceiverURL     string
	EncryptedSecret string
	EncryptedConfig string
	Revision        int64
	CreatedAt       string
	UpdatedAt       string
}

// MXSettingsID is the fixed primary key of the singleton row.
const MXSettingsID = "mx"

// MX receiver modes as persisted. An empty mode means "no receiver configured".
const (
	MXModeIncluded = "included"
	MXModeRemote   = "remote"
)

// GetMXSettings returns the installation MX configuration, or ErrNotFound when
// it has never been saved.
func (s *Store) GetMXSettings(ctx context.Context) (MXSettings, error) {
	var m MXSettings
	err := s.read.QueryRowContext(ctx, `SELECT id,mode,receiver_url,encrypted_secret,encrypted_config,revision,created_at,updated_at FROM mx_settings WHERE id=?`, MXSettingsID).Scan(
		&m.ID, &m.Mode, &m.ReceiverURL, &m.EncryptedSecret, &m.EncryptedConfig, &m.Revision, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MXSettings{}, ErrNotFound
	}
	if err != nil {
		return MXSettings{}, err
	}
	return m, nil
}

// SaveMXSettingsCAS creates the singleton (zero expected) or updates it under
// optimistic concurrency. A zero ConfigVersion means "create only" and fails
// with ErrConflict if a row already exists. A non-zero version must match the
// stored revision. The read-back and write happen in one transaction.
func (s *Store) SaveMXSettingsCAS(ctx context.Context, mode, receiverURL, encryptedSecret, encryptedConfig string, expected ConfigVersion) (MXSettings, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return MXSettings{}, err
	}
	defer tx.Rollback()
	now := nowText()
	if expected.Revision == 0 {
		var n int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM mx_settings WHERE id=?`, MXSettingsID).Scan(&n); err != nil {
			return MXSettings{}, err
		}
		if n != 0 {
			return MXSettings{}, ErrConflict
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO mx_settings(id,mode,receiver_url,encrypted_secret,encrypted_config,revision,created_at,updated_at) VALUES(?,?,?,?,?,1,?,?)`,
			MXSettingsID, mode, receiverURL, encryptedSecret, encryptedConfig, now, now); err != nil {
			return MXSettings{}, err
		}
	} else {
		res, err := tx.ExecContext(ctx, `UPDATE mx_settings SET mode=?,receiver_url=?,encrypted_secret=?,encrypted_config=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`,
			mode, receiverURL, encryptedSecret, encryptedConfig, now, MXSettingsID, expected.Revision)
		if err != nil {
			return MXSettings{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return MXSettings{}, ErrConflict
		}
	}
	m, err := scanMXSettings(tx.QueryRowContext(ctx, `SELECT id,mode,receiver_url,encrypted_secret,encrypted_config,revision,created_at,updated_at FROM mx_settings WHERE id=?`, MXSettingsID))
	if err != nil {
		return MXSettings{}, err
	}
	if err = tx.Commit(); err != nil {
		return MXSettings{}, err
	}
	return m, nil
}

// ClearMXSettingsCAS removes the configured receiver while keeping the row, so
// the singleton remains "initialized": a cleared configuration is a deliberate
// decision and must not re-arm the one-time environment import. Mode and
// ReceiverURL are emptied and the encrypted fields dropped; revision advances so
// the runtime reconciliation observes the change. Clearing an already-empty
// configuration with a zero expected version is a no-op; a non-zero expected
// version must match the stored revision.
func (s *Store) ClearMXSettingsCAS(ctx context.Context, expected ConfigVersion) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var revision int64
	var mode string
	err = tx.QueryRowContext(ctx, `SELECT revision,mode FROM mx_settings WHERE id=?`, MXSettingsID).Scan(&revision, &mode)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if expected.Revision != 0 && expected.Revision != revision {
		return ErrConflict
	}
	if mode == "" {
		return tx.Commit()
	}
	res, err := tx.ExecContext(ctx, `UPDATE mx_settings SET mode='',receiver_url='',encrypted_secret='',encrypted_config='',revision=revision+1,updated_at=? WHERE id=? AND revision=?`,
		nowText(), MXSettingsID, revision)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConflict
	}
	return tx.Commit()
}

// MXSettingsInitialized reports whether the installation has a saved MX
// configuration. It is used to decide whether the one-time environment import
// should run: once a row exists, the stored truth wins and the environment is
// never consulted again. A deliberately cleared configuration still leaves a row
// (mode empty), so clearing does not re-arm the import.
func (s *Store) MXSettingsInitialized(ctx context.Context) (bool, error) {
	var n int
	if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM mx_settings WHERE id=?`, MXSettingsID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func scanMXSettings(row interface{ Scan(...any) error }) (MXSettings, error) {
	var m MXSettings
	if err := row.Scan(&m.ID, &m.Mode, &m.ReceiverURL, &m.EncryptedSecret, &m.EncryptedConfig, &m.Revision, &m.CreatedAt, &m.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MXSettings{}, ErrNotFound
		}
		return MXSettings{}, err
	}
	return m, nil
}
