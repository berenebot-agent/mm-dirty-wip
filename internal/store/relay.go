package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/dellarb/mailmoose/internal/auth"
	"github.com/dellarb/mailmoose/internal/idgen"
)

type HermesConnection struct {
	ID, AccountID, InboxID, Name, GatewayID, SecretEncrypted, DeliveryKeyEncrypted string
	// OutboundRole is "owner" (relay sends directly) or "assistant" (the relay
	// creates a draft and requests approval instead of sending).
	OutboundRole    string
	LastAckEventID  int64
	CreatedAt       time.Time
	LastConnectedAt *time.Time
}

func (s *Store) CreateHermesEnrollToken(ctx context.Context, accountID, inboxID, name string, ttl time.Duration) (string, error) {
	var n int
	if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&n); err != nil || n != 1 {
		return "", ErrForbidden
	}
	tok, err := auth.RandomToken(32)
	if err != nil {
		return "", err
	}
	id := idgen.New("hen")
	_, err = s.write.ExecContext(ctx, `INSERT INTO hermes_enroll_tokens(id,account_id,inbox_id,name,token_hash,expires_at,created_at) VALUES(?,?,?,?,?,?,?)`, id, accountID, inboxID, name, auth.HashToken(tok), timeText(time.Now().UTC().Add(ttl)), nowText())
	return tok, err
}

type EnrollRecord struct{ AccountID, InboxID, Name string }

// EnrollHermesConnection consumes a one-time enrollment token and creates or
// replaces the gateway connection atomically. A gateway id owned by another
// account is rejected without consuming the token.
func (s *Store) EnrollHermesConnection(ctx context.Context, token, gatewayID, secretEnc, deliveryEnc string) (HermesConnection, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return HermesConnection{}, err
	}
	defer tx.Rollback()
	var id, accountID, inboxID, name, exp string
	var used sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id,account_id,inbox_id,name,expires_at,used_at FROM hermes_enroll_tokens WHERE token_hash=?`, auth.HashToken(token)).Scan(&id, &accountID, &inboxID, &name, &exp, &used)
	if err == sql.ErrNoRows {
		return HermesConnection{}, ErrNotFound
	}
	if err != nil {
		return HermesConnection{}, err
	}
	if used.Valid || parseTime(exp).Before(time.Now().UTC()) {
		return HermesConnection{}, ErrForbidden
	}
	rec := EnrollRecord{AccountID: accountID, InboxID: inboxID, Name: name}
	now := nowText()
	connID, err := s.upsertHermesConnectionTx(ctx, tx, rec, gatewayID, secretEnc, deliveryEnc, now)
	if err != nil {
		return HermesConnection{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE hermes_enroll_tokens SET used_at=? WHERE id=?`, now, id); err != nil {
		return HermesConnection{}, err
	}
	if err = tx.Commit(); err != nil {
		return HermesConnection{}, err
	}
	return hermesConnectionFrom(connID, rec, gatewayID, secretEnc, deliveryEnc, now), nil
}

// CreateHermesConnection creates or rotates a connection for an already
// authorized account (the admin-generated path). Cross-account collisions are
// rejected.
func (s *Store) CreateHermesConnection(ctx context.Context, r EnrollRecord, gatewayID, secretEnc, deliveryEnc string) (HermesConnection, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return HermesConnection{}, err
	}
	defer tx.Rollback()
	now := nowText()
	id, err := s.upsertHermesConnectionTx(ctx, tx, r, gatewayID, secretEnc, deliveryEnc, now)
	if err != nil {
		return HermesConnection{}, err
	}
	if err = tx.Commit(); err != nil {
		return HermesConnection{}, err
	}
	return hermesConnectionFrom(id, r, gatewayID, secretEnc, deliveryEnc, now), nil
}

func (s *Store) upsertHermesConnectionTx(ctx context.Context, tx *sql.Tx, r EnrollRecord, gatewayID, secretEnc, deliveryEnc, now string) (string, error) {
	var existingID, existingAccount string
	err := tx.QueryRowContext(ctx, `SELECT c.id,c.account_id FROM clients c JOIN client_push p ON p.client_id=c.id WHERE p.gateway_id=? AND c.type='hermes'`, gatewayID).Scan(&existingID, &existingAccount)
	switch {
	case err == sql.ErrNoRows:
		id := idgen.New("hrm")
		if _, err = tx.ExecContext(ctx, `INSERT INTO clients(id,account_id,type,name,created_at) VALUES(?,?,'hermes',?,?)`, id, r.AccountID, r.Name, now); err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO client_push(client_id,inbox_id,gateway_id,secret_encrypted,delivery_key_encrypted) VALUES(?,?,?,?,?)`, id, r.InboxID, gatewayID, secretEnc, deliveryEnc); err != nil {
			return "", err
		}
		return id, nil
	case err != nil:
		return "", err
	case existingAccount != r.AccountID:
		return "", ErrForbidden
	default:
		if _, err = tx.ExecContext(ctx, `UPDATE clients SET account_id=?,name=?,created_at=? WHERE id=?`, r.AccountID, r.Name, now, existingID); err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE client_push SET inbox_id=?,secret_encrypted=?,delivery_key_encrypted=?,last_ack_event_id=0 WHERE client_id=?`, r.InboxID, secretEnc, deliveryEnc, existingID); err != nil {
			return "", err
		}
		return existingID, nil
	}
}

func hermesConnectionFrom(id string, r EnrollRecord, gatewayID, secretEnc, deliveryEnc, now string) HermesConnection {
	return HermesConnection{ID: id, AccountID: r.AccountID, InboxID: r.InboxID, Name: r.Name, GatewayID: gatewayID, SecretEncrypted: secretEnc, DeliveryKeyEncrypted: deliveryEnc, OutboundRole: "owner", CreatedAt: parseTime(now)}
}
func scanHermes(row interface{ Scan(...any) error }) (HermesConnection, error) {
	var h HermesConnection
	var cr string
	var lc sql.NullString
	err := row.Scan(&h.ID, &h.AccountID, &h.InboxID, &h.Name, &h.GatewayID, &h.SecretEncrypted, &h.DeliveryKeyEncrypted, &h.LastAckEventID, &cr, &lc, &h.OutboundRole)
	if err != nil {
		return h, err
	}
	if h.OutboundRole == "" {
		h.OutboundRole = "owner"
	}
	h.CreatedAt = parseTime(cr)
	h.LastConnectedAt = nullableTime(lc)
	return h, nil
}
func (s *Store) GetHermesConnectionByGateway(ctx context.Context, gatewayID string) (HermesConnection, error) {
	h, err := scanHermes(s.read.QueryRowContext(ctx, `SELECT c.id,c.account_id,p.inbox_id,c.name,p.gateway_id,p.secret_encrypted,p.delivery_key_encrypted,p.last_ack_event_id,c.created_at,p.last_connected_at,p.outbound_role FROM clients c JOIN client_push p ON p.client_id=c.id WHERE p.gateway_id=? AND c.type='hermes'`, gatewayID))
	if err == sql.ErrNoRows {
		return h, ErrNotFound
	}
	return h, err
}
func (s *Store) ListHermesConnections(ctx context.Context, accountID string) ([]HermesConnection, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT c.id,c.account_id,p.inbox_id,c.name,p.gateway_id,p.secret_encrypted,p.delivery_key_encrypted,p.last_ack_event_id,c.created_at,p.last_connected_at,p.outbound_role FROM clients c JOIN client_push p ON p.client_id=c.id WHERE c.account_id=? AND c.type='hermes' ORDER BY c.created_at DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HermesConnection{}
	for rows.Next() {
		h, err := scanHermes(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
func (s *Store) MarkHermesConnected(ctx context.Context, id string) {
	_, _ = s.write.ExecContext(ctx, `UPDATE client_push SET last_connected_at=? WHERE client_id=?`, nowText(), id)
}
func (s *Store) AckHermesEvent(ctx context.Context, id string, eventID int64) error {
	_, err := s.write.ExecContext(ctx, `UPDATE client_push SET last_ack_event_id=MAX(last_ack_event_id,?) WHERE client_id=?`, eventID, id)
	return err
}
func (s *Store) UpdateHermesConnectionName(ctx context.Context, accountID, id, name string) error {
	res, err := s.write.ExecContext(ctx, `UPDATE clients SET name=? WHERE id=? AND account_id=? AND type='hermes'`, strings.TrimSpace(name), id, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetHermesOutboundRole sets a relay connection's outbound authority. Only
// "owner" and "assistant" are accepted; anything else is rejected.
func (s *Store) SetHermesOutboundRole(ctx context.Context, accountID, id, role string) error {
	role = strings.ToLower(strings.TrimSpace(role))
	if role != "owner" && role != "assistant" {
		return ErrForbidden
	}
	res, err := s.write.ExecContext(ctx, `UPDATE client_push SET outbound_role=? WHERE client_id=? AND EXISTS (SELECT 1 FROM clients c WHERE c.id=client_push.client_id AND c.account_id=? AND c.type='hermes')`, role, id, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteHermesConnection(ctx context.Context, accountID, id string) error {
	res, err := s.write.ExecContext(ctx, `DELETE FROM clients WHERE id=? AND account_id=? AND type='hermes'`, id, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
