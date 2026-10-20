package store

import (
	"context"
	"database/sql"
	"errors"
)

// DomainDKIM is the per-domain DKIM signing material. PrivateKey is the encrypted
// PEM exactly as stored; the app layer decrypts it.
type DomainDKIM struct {
	PrivateKey string
	PublicKey  string
	Selector   string
	Domain     string
}

// GetDomainDKIM reads a domain's DKIM signing material. A domain that exists but
// has no key yet returns the zero value with no error (the startup pass fills it
// in); a missing or foreign domain returns ErrNotFound.
func (s *Store) GetDomainDKIM(ctx context.Context, accountID, domainID string) (DomainDKIM, error) {
	var out DomainDKIM
	var exists int
	err := s.read.QueryRowContext(ctx, `SELECT 1 FROM domains WHERE id=? AND account_id=?`, domainID, accountID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return DomainDKIM{}, ErrNotFound
	}
	if err != nil {
		return DomainDKIM{}, err
	}
	err = s.read.QueryRowContext(ctx, `SELECT dkim_private_key, dkim_public_key, dkim_selector, dkim_domain FROM domains WHERE id=? AND account_id=?`, domainID, accountID).
		Scan(&out.PrivateKey, &out.PublicKey, &out.Selector, &out.Domain)
	if err != nil {
		return DomainDKIM{}, err
	}
	return out, nil
}

// SetDomainDKIM stores a domain's DKIM signing material. It is a whole-value
// write; the caller is responsible for decrypting and re-deriving the public key
// so the stored public half can never disagree with the private half. A missing
// or foreign domain returns ErrNotFound.
func (s *Store) SetDomainDKIM(ctx context.Context, accountID, domainID string, k DomainDKIM) error {
	res, err := s.write.ExecContext(ctx, `UPDATE domains SET dkim_private_key=?, dkim_public_key=?, dkim_selector=?, dkim_domain=? WHERE id=? AND account_id=?`,
		k.PrivateKey, k.PublicKey, k.Selector, k.Domain, domainID, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DomainsMissingDKIM lists the (account_id, id, name) of every domain with no
// stored private signing key, for the startup backfill.
func (s *Store) DomainsMissingDKIM(ctx context.Context) ([]DomainRef, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT id, account_id, name FROM domains WHERE dkim_private_key='' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DomainRef
	for rows.Next() {
		var d DomainRef
		if err := rows.Scan(&d.ID, &d.AccountID, &d.Name); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DomainRef is a minimal domain reference (id, owning account, name) used by the
// DKIM backfill.
type DomainRef struct {
	ID        string
	AccountID string
	Name      string
}
