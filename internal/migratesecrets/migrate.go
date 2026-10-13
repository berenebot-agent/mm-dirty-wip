// Package migratesecrets migrates ciphertext written before stored secrets
// carried an AAD binding: it scans every encrypted column, opens each pre-AAD
// ("unprefixed") value with the instance key (falling back to the pre-PBKDF2
// derivation), and writes it back in the current AAD-bound ("v2.") envelope.
//
// It exists because the application read paths refuse unbound ciphertext, so an
// instance created before the binding must be migrated once, with the
// application stopped. It is idempotent: an already-bound value is skipped, so a
// second run reports zero migrations.
//
// It deliberately duplicates the AAD strings and the legacy envelope rather than
// importing internal/app, so the application read paths stay free of any
// pre-AAD knowledge. Keep the descriptor table in step with every encrypted
// column: a new one that opens through DecryptSecretAAD belongs here too.
package migratesecrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/pbkdf2"
)

// envelopePrefix marks an AAD-bound ciphertext; keep in step with cryptox.
const envelopePrefix = "v2."

// kdfSalt and kdfIterations mirror cryptox for the passphrase key fallback.
var kdfSalt = []byte("github.com/dellarb/mailmoose/app-encryption-key/v2")

const kdfIterations = 210_000

// Report summarises a run. Migrated is keyed by descriptor label; Failures names
// each row that could not be opened or written, so an operator can see which
// credential needs re-entry.
type Report struct {
	Migrated map[string]int
	Failures []string
}

// Total returns the number of values rewritten.
func (r Report) Total() int {
	n := 0
	for _, v := range r.Migrated {
		n += v
	}
	return n
}

// column describes one encrypted column and how to rebuild its AAD from the row.
type column struct {
	label string
	// selectSQL returns the key columns followed by the secret column, its last
	// result. Rows whose secret is empty or already bound are filtered in the
	// engine.
	selectSQL string
	// cols names the SELECT columns in order. The last is the secret.
	cols []string
	// aad builds the binding from the selected row.
	aad func(row map[string]string) string
	// updateSQL sets the secret (one placeholders slot) then matches the key
	// columns.
	updateSQL string
	// keyCols are taken from the row, in order, as the update's trailing args.
	keyCols []string
}

// Run migrates every known encrypted column in db. rawKey is the instance
// APP_ENCRYPTION_KEY exactly as configured. When dryRun is set no row is
// written; the Report still lists what would change.
func Run(ctx context.Context, db *sql.DB, rawKey string, dryRun bool) (Report, error) {
	key, err := deriveKey(rawKey)
	if err != nil {
		return Report{}, err
	}
	legacy := legacyKey(rawKey)
	keys := [][]byte{key}
	if legacy != nil {
		keys = append(keys, legacy)
	}

	rep := Report{Migrated: map[string]int{}}
	for _, c := range columns() {
		if err := migrateColumn(ctx, db, c, key, keys, dryRun, &rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// Labels returns every column label the migrator knows, in descriptor order.
func Labels() []string {
	cols := columns()
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.label
	}
	return out
}

func migrateColumn(ctx context.Context, db *sql.DB, c column, key []byte, openKeys [][]byte, dryRun bool, rep *Report) error {
	rows, err := db.QueryContext(ctx, c.selectSQL)
	if err != nil {
		// A table absent from an older schema is not fatal; report and move on.
		rep.Failures = append(rep.Failures, fmt.Sprintf("%s: query: %v", c.label, err))
		return nil
	}
	defer rows.Close()

	type pending struct {
		sealed string
		keys   []string
	}
	var todo []pending
	for rows.Next() {
		vals := make([]sql.NullString, len(c.cols))
		dest := make([]any, len(c.cols))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return fmt.Errorf("%s: scan: %w", c.label, err)
		}
		row := map[string]string{}
		for i, name := range c.cols {
			row[name] = vals[i].String
		}
		secret := row[c.cols[len(c.cols)-1]]
		if secret == "" || strings.HasPrefix(secret, envelopePrefix) {
			continue
		}
		plain, ok := openLegacy(openKeys, secret)
		if !ok {
			rep.Failures = append(rep.Failures, fmt.Sprintf("%s: cannot open row %v", c.label, rowKeys(row, c.keyCols)))
			continue
		}
		sealed, serr := seal(key, plain, []byte(c.aad(row)))
		if serr != nil {
			rep.Failures = append(rep.Failures, fmt.Sprintf("%s: seal row %v: %v", c.label, rowKeys(row, c.keyCols), serr))
			continue
		}
		todo = append(todo, pending{sealed: sealed, keys: rowKeys(row, c.keyCols)})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%s: rows: %w", c.label, err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, p := range todo {
		if dryRun {
			rep.Migrated[c.label]++
			continue
		}
		args := append([]any{p.sealed}, stringArgs(p.keys)...)
		res, err := db.ExecContext(ctx, c.updateSQL, args...)
		if err != nil {
			rep.Failures = append(rep.Failures, fmt.Sprintf("%s: update %v: %v", c.label, p.keys, err))
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			rep.Failures = append(rep.Failures, fmt.Sprintf("%s: update matched no row %v", c.label, p.keys))
			continue
		}
		rep.Migrated[c.label]++
	}
	return nil
}

func rowKeys(row map[string]string, cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = row[c]
	}
	return out
}

func stringArgs(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// columns is the full descriptor set. Every encrypted column that a read path
// opens through DecryptSecretAAD/DecryptWithAAD must appear here.
func columns() []column {
	return []column{
		{
			label:     "domains.dkim_private_key",
			selectSQL: `SELECT id,account_id,dkim_private_key FROM domains WHERE dkim_private_key<>''`,
			cols:      []string{"id", "account_id", "secret"},
			aad:       func(r map[string]string) string { return "domain_dkim:" + r["account_id"] + ":" + r["id"] },
			updateSQL: `UPDATE domains SET dkim_private_key=? WHERE id=?`,
			keyCols:   []string{"id"},
		},
		{
			label:     "domain_sending_configs.encrypted_config",
			selectSQL: `SELECT id,account_id,domain_id,encrypted_config FROM domain_sending_configs WHERE encrypted_config<>''`,
			cols:      []string{"id", "account_id", "domain_id", "secret"},
			aad:       func(r map[string]string) string { return "cfg:" + r["account_id"] + ":" + r["domain_id"] },
			updateSQL: `UPDATE domain_sending_configs SET encrypted_config=? WHERE id=?`,
			keyCols:   []string{"id"},
		},
		{
			label:     "domain_receiving_configs.encrypted_config",
			selectSQL: `SELECT id,account_id,domain_id,encrypted_config FROM domain_receiving_configs WHERE encrypted_config<>''`,
			cols:      []string{"id", "account_id", "domain_id", "secret"},
			aad:       func(r map[string]string) string { return "cfg:" + r["account_id"] + ":" + r["domain_id"] },
			updateSQL: `UPDATE domain_receiving_configs SET encrypted_config=? WHERE id=?`,
			keyCols:   []string{"id"},
		},
		{
			// The table has no account_id; the AAD uses the owning domain's.
			label:     "dialmx_domain_credentials.encrypted_private_seed",
			selectSQL: `SELECT c.domain_id,d.account_id,c.encrypted_private_seed FROM dialmx_domain_credentials c JOIN domains d ON d.id=c.domain_id WHERE c.encrypted_private_seed<>''`,
			cols:      []string{"domain_id", "account_id", "secret"},
			aad:       func(r map[string]string) string { return "dialmx_seed:" + r["account_id"] + ":" + r["domain_id"] },
			updateSQL: `UPDATE dialmx_domain_credentials SET encrypted_private_seed=? WHERE domain_id=?`,
			keyCols:   []string{"domain_id"},
		},
		{
			label:     "mx_settings.encrypted_secret",
			selectSQL: `SELECT id,encrypted_secret FROM mx_settings WHERE encrypted_secret<>''`,
			cols:      []string{"id", "secret"},
			aad:       func(map[string]string) string { return "mx_settings" },
			updateSQL: `UPDATE mx_settings SET encrypted_secret=? WHERE id=?`,
			keyCols:   []string{"id"},
		},
		{
			label:     "mx_settings.encrypted_config",
			selectSQL: `SELECT id,encrypted_config FROM mx_settings WHERE encrypted_config<>''`,
			cols:      []string{"id", "secret"},
			aad:       func(map[string]string) string { return "mx_settings" },
			updateSQL: `UPDATE mx_settings SET encrypted_config=? WHERE id=?`,
			keyCols:   []string{"id"},
		},
		{
			label:     "account_mx_receivers.encrypted_secret",
			selectSQL: `SELECT account_id,encrypted_secret FROM account_mx_receivers WHERE encrypted_secret<>''`,
			cols:      []string{"account_id", "secret"},
			aad:       func(r map[string]string) string { return "account_mx:" + r["account_id"] },
			updateSQL: `UPDATE account_mx_receivers SET encrypted_secret=? WHERE account_id=?`,
			keyCols:   []string{"account_id"},
		},
		{
			label:     "account_mx_receivers.encrypted_config",
			selectSQL: `SELECT account_id,encrypted_config FROM account_mx_receivers WHERE encrypted_config<>''`,
			cols:      []string{"account_id", "secret"},
			aad:       func(r map[string]string) string { return "account_mx:" + r["account_id"] },
			updateSQL: `UPDATE account_mx_receivers SET encrypted_config=? WHERE account_id=?`,
			keyCols:   []string{"account_id"},
		},
		{
			label:     "client_push.signing_secret_encrypted",
			selectSQL: `SELECT p.client_id,c.account_id,p.inbox_id,p.signing_secret_encrypted FROM client_push p JOIN clients c ON c.id=p.client_id WHERE c.type='webhook' AND p.signing_secret_encrypted<>''`,
			cols:      []string{"client_id", "account_id", "inbox_id", "secret"},
			aad:       func(r map[string]string) string { return "whk:" + r["account_id"] + ":" + r["inbox_id"] },
			updateSQL: `UPDATE client_push SET signing_secret_encrypted=? WHERE client_id=?`,
			keyCols:   []string{"client_id"},
		},
		{
			label:     "client_push.secret_encrypted",
			selectSQL: `SELECT p.client_id,p.gateway_id,p.secret_encrypted FROM client_push p JOIN clients c ON c.id=p.client_id WHERE c.type IN ('hermes','openclaw') AND p.secret_encrypted<>''`,
			cols:      []string{"client_id", "gateway_id", "secret"},
			aad:       func(r map[string]string) string { return "relay:" + r["gateway_id"] },
			updateSQL: `UPDATE client_push SET secret_encrypted=? WHERE client_id=?`,
			keyCols:   []string{"client_id"},
		},
		{
			label:     "client_push.delivery_key_encrypted",
			selectSQL: `SELECT p.client_id,p.gateway_id,p.delivery_key_encrypted FROM client_push p JOIN clients c ON c.id=p.client_id WHERE c.type IN ('hermes','openclaw') AND p.delivery_key_encrypted<>''`,
			cols:      []string{"client_id", "gateway_id", "secret"},
			aad:       func(r map[string]string) string { return "relay:" + r["gateway_id"] },
			updateSQL: `UPDATE client_push SET delivery_key_encrypted=? WHERE client_id=?`,
			keyCols:   []string{"client_id"},
		},
		{
			label:     "inbox_remote_credentials.encrypted_imap",
			selectSQL: `SELECT inbox_id,account_id,encrypted_imap FROM inbox_remote_credentials WHERE encrypted_imap<>''`,
			cols:      []string{"inbox_id", "account_id", "secret"},
			aad:       func(r map[string]string) string { return "remote_cred:" + r["account_id"] + ":" + r["inbox_id"] },
			updateSQL: `UPDATE inbox_remote_credentials SET encrypted_imap=? WHERE inbox_id=?`,
			keyCols:   []string{"inbox_id"},
		},
		{
			label:     "inbox_remote_credentials.encrypted_smtp",
			selectSQL: `SELECT inbox_id,account_id,encrypted_smtp FROM inbox_remote_credentials WHERE encrypted_smtp<>''`,
			cols:      []string{"inbox_id", "account_id", "secret"},
			aad:       func(r map[string]string) string { return "remote_cred:" + r["account_id"] + ":" + r["inbox_id"] },
			updateSQL: `UPDATE inbox_remote_credentials SET encrypted_smtp=? WHERE inbox_id=?`,
			keyCols:   []string{"inbox_id"},
		},
		{
			label:     "inbox_google_connections.encrypted_secret",
			selectSQL: `SELECT inbox_id,account_id,encrypted_secret FROM inbox_google_connections WHERE encrypted_secret<>''`,
			cols:      []string{"inbox_id", "account_id", "secret"},
			aad:       func(r map[string]string) string { return "google:" + r["account_id"] + ":" + r["inbox_id"] },
			updateSQL: `UPDATE inbox_google_connections SET encrypted_secret=? WHERE inbox_id=?`,
			keyCols:   []string{"inbox_id"},
		},
		{
			label:     "inbox_google_connections.encrypted_token",
			selectSQL: `SELECT inbox_id,account_id,encrypted_token FROM inbox_google_connections WHERE encrypted_token<>''`,
			cols:      []string{"inbox_id", "account_id", "secret"},
			aad:       func(r map[string]string) string { return "google:" + r["account_id"] + ":" + r["inbox_id"] },
			updateSQL: `UPDATE inbox_google_connections SET encrypted_token=? WHERE inbox_id=?`,
			keyCols:   []string{"inbox_id"},
		},
		{
			label:     "google_oauth_attempts.encrypted_pending",
			selectSQL: `SELECT state_hash,account_id,encrypted_pending FROM google_oauth_attempts WHERE encrypted_pending<>''`,
			cols:      []string{"state_hash", "account_id", "secret"},
			aad:       func(r map[string]string) string { return "google_attempt:" + r["account_id"] + ":" + r["state_hash"] },
			updateSQL: `UPDATE google_oauth_attempts SET encrypted_pending=? WHERE state_hash=?`,
			keyCols:   []string{"state_hash"},
		},
		{
			label:     "inbox_microsoft_connections.encrypted_token",
			selectSQL: `SELECT inbox_id,account_id,encrypted_token FROM inbox_microsoft_connections WHERE encrypted_token<>''`,
			cols:      []string{"inbox_id", "account_id", "secret"},
			aad:       func(r map[string]string) string { return "microsoft:" + r["account_id"] + ":" + r["inbox_id"] },
			updateSQL: `UPDATE inbox_microsoft_connections SET encrypted_token=? WHERE inbox_id=?`,
			keyCols:   []string{"inbox_id"},
		},
		{
			label:     "microsoft_push.encrypted_state",
			selectSQL: `SELECT inbox_id,account_id,encrypted_state FROM microsoft_push WHERE encrypted_state<>''`,
			cols:      []string{"inbox_id", "account_id", "secret"},
			aad:       func(r map[string]string) string { return "microsoft_push:" + r["account_id"] + ":" + r["inbox_id"] },
			updateSQL: `UPDATE microsoft_push SET encrypted_state=? WHERE inbox_id=?`,
			keyCols:   []string{"inbox_id"},
		},
		{
			label:     "microsoft_push.encrypted_token",
			selectSQL: `SELECT inbox_id,account_id,encrypted_token FROM microsoft_push WHERE encrypted_token<>''`,
			cols:      []string{"inbox_id", "account_id", "secret"},
			aad:       func(r map[string]string) string { return "microsoft:" + r["account_id"] + ":" + r["inbox_id"] },
			updateSQL: `UPDATE microsoft_push SET encrypted_token=? WHERE inbox_id=?`,
			keyCols:   []string{"inbox_id"},
		},
		{
			label:     "microsoft_oauth_attempts.encrypted_pending",
			selectSQL: `SELECT state_hash,account_id,encrypted_pending FROM microsoft_oauth_attempts WHERE encrypted_pending<>''`,
			cols:      []string{"state_hash", "account_id", "secret"},
			aad: func(r map[string]string) string {
				return "microsoft_attempt:" + r["account_id"] + ":" + r["state_hash"]
			},
			updateSQL: `UPDATE microsoft_oauth_attempts SET encrypted_pending=? WHERE state_hash=?`,
			keyCols:   []string{"state_hash"},
		},
		{
			// The selection-phase token binds to the selection state hash, which
			// is the row's own state_hash column.
			label:     "microsoft_oauth_attempts.encrypted_token",
			selectSQL: `SELECT state_hash,account_id,encrypted_token FROM microsoft_oauth_attempts WHERE encrypted_token<>''`,
			cols:      []string{"state_hash", "account_id", "secret"},
			aad:       func(r map[string]string) string { return "microsoft_select:" + r["account_id"] + ":" + r["state_hash"] },
			updateSQL: `UPDATE microsoft_oauth_attempts SET encrypted_token=? WHERE state_hash=?`,
			keyCols:   []string{"state_hash"},
		},
		{
			label:     "inbox_one_bindings.encrypted_connection_key",
			selectSQL: `SELECT inbox_id,account_id,encrypted_connection_key FROM inbox_one_bindings WHERE encrypted_connection_key<>''`,
			cols:      []string{"inbox_id", "account_id", "secret"},
			aad:       func(r map[string]string) string { return "one_binding:" + r["account_id"] + ":" + r["inbox_id"] },
			updateSQL: `UPDATE inbox_one_bindings SET encrypted_connection_key=? WHERE inbox_id=?`,
			keyCols:   []string{"inbox_id"},
		},
		{
			// The binding stores its own AAD in key_aad; the read path uses it
			// verbatim, so the migrator must too.
			label:     "inbox_one_bindings.encrypted_key",
			selectSQL: `SELECT inbox_id,key_aad,encrypted_key FROM inbox_one_bindings WHERE encrypted_key<>''`,
			cols:      []string{"inbox_id", "key_aad", "secret"},
			aad:       func(r map[string]string) string { return r["key_aad"] },
			updateSQL: `UPDATE inbox_one_bindings SET encrypted_key=? WHERE inbox_id=?`,
			keyCols:   []string{"inbox_id"},
		},
		{
			label:     "one_import_setups.encrypted_key",
			selectSQL: `SELECT id,account_id,encrypted_key FROM one_import_setups WHERE encrypted_key<>''`,
			cols:      []string{"id", "account_id", "secret"},
			aad:       func(r map[string]string) string { return "one_import:" + r["account_id"] + ":" + r["id"] },
			updateSQL: `UPDATE one_import_setups SET encrypted_key=? WHERE id=?`,
			keyCols:   []string{"id"},
		},
	}
}

// Verify re-reads every known column and confirms each non-empty secret both
// carries the AAD-bound prefix and opens with open, which must be the
// production read path (cryptox.DecryptWithAAD). It returns the number of
// values confirmed and a description of each that failed, so an AAD mismatch or
// an unreadable value is caught before the application reads it.
//
// In dry-run mode an unprefixed value is not a failure: it is reported by Run
// as one that would be migrated, so Verify only confirms the values already
// bound.
func Verify(ctx context.Context, db *sql.DB, key []byte, open func(key []byte, encoded string, aad []byte) ([]byte, error), dryRun bool) (int, []string, error) {
	ok := 0
	var failures []string
	for _, c := range columns() {
		rows, err := db.QueryContext(ctx, c.selectSQL)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: query: %v", c.label, err))
			continue
		}
		for rows.Next() {
			vals := make([]sql.NullString, len(c.cols))
			dest := make([]any, len(c.cols))
			for i := range vals {
				dest[i] = &vals[i]
			}
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				return ok, failures, fmt.Errorf("%s: scan: %w", c.label, err)
			}
			row := map[string]string{}
			for i, name := range c.cols {
				row[name] = vals[i].String
			}
			secret := row[c.cols[len(c.cols)-1]]
			if secret == "" {
				continue
			}
			keys := rowKeys(row, c.keyCols)
			if !strings.HasPrefix(secret, envelopePrefix) {
				if !dryRun {
					failures = append(failures, fmt.Sprintf("%s: row %v is still unprefixed", c.label, keys))
				}
				continue
			}
			if _, err := open(key, secret, []byte(c.aad(row))); err != nil {
				failures = append(failures, fmt.Sprintf("%s: row %v does not open: %v", c.label, keys, err))
				continue
			}
			ok++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return ok, failures, fmt.Errorf("%s: rows: %w", c.label, err)
		}
		rows.Close()
	}
	return ok, failures, nil
}

// deriveKey mirrors cryptox.DeriveKey: a 32-byte base64/hex key is used
// directly, otherwise the passphrase is stretched with PBKDF2.
func deriveKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty APP_ENCRYPTION_KEY")
	}
	if b, ok := decode32(raw); ok {
		return b, nil
	}
	if len(raw) < 24 {
		return nil, fmt.Errorf("APP_ENCRYPTION_KEY must be at least 24 characters or a 32-byte base64/hex key")
	}
	return pbkdf2.Key([]byte(raw), kdfSalt, kdfIterations, 32, sha256.New), nil
}

// legacyKey returns the pre-PBKDF2 derivation (a bare SHA-256), or nil for a
// 32-byte key, which never had one.
func legacyKey(raw string) []byte {
	raw = strings.TrimSpace(raw)
	if _, ok := decode32(raw); ok {
		return nil
	}
	if len(raw) < 24 {
		return nil
	}
	h := sha256.Sum256([]byte(raw))
	return h[:]
}

func decode32(raw string) ([]byte, bool) {
	if b, err := base64.RawStdEncoding.DecodeString(raw); err == nil && len(b) == 32 {
		return b, true
	}
	if b, err := base64.StdEncoding.DecodeString(raw); err == nil && len(b) == 32 {
		return b, true
	}
	if b, err := hex.DecodeString(raw); err == nil && len(b) == 32 {
		return b, true
	}
	return nil, false
}

// openLegacy opens a pre-AAD ciphertext (no prefix, nil AAD) with the first key
// that works.
func openLegacy(keys [][]byte, encoded string) ([]byte, bool) {
	for _, k := range keys {
		if b, err := open(k, encoded, nil); err == nil {
			return b, true
		}
	}
	return nil, false
}

func open(key []byte, encoded string, aad []byte) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, fmt.Errorf("ciphertext too short")
	}
	return gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], aad)
}

// seal writes the current AAD-bound envelope.
func seal(key, plaintext, aad []byte) (string, error) {
	if len(aad) == 0 {
		return "", fmt.Errorf("empty aad")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nil, nonce, plaintext, aad)
	return envelopePrefix + base64.RawURLEncoding.EncodeToString(append(nonce, ct...)), nil
}
