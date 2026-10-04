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
	// Kind discriminates the product connector: "hermes" or "openclaw". Both
	// share the same relay transport and persistence.
	Kind string
	// OutboundRole is "owner" (relay sends directly) or "assistant" (the relay
	// creates a draft and requests approval instead of sending).
	OutboundRole    string
	LastAckEventID  int64
	CreatedAt       time.Time
	LastConnectedAt *time.Time
}

// RelayKind is a relay connector type. The zero value is invalid.
type RelayKind string

const (
	KindHermes   RelayKind = "hermes"
	KindOpenClaw RelayKind = "openclaw"
)

// relayKindsSQL is the type-filter every relay query shares. A relay
// connection is a clients row whose type is one of the relay connector kinds.
const relayKindsSQL = `'hermes','openclaw'`

func validRelayKind(kind RelayKind) bool {
	return kind == KindHermes || kind == KindOpenClaw
}

func (s *Store) CreateHermesEnrollToken(ctx context.Context, accountID, inboxID, name string, ttl time.Duration) (string, error) {
	return s.CreateRelayEnrollToken(ctx, accountID, inboxID, name, KindHermes, ttl)
}

// CreateRelayEnrollToken mints a one-time setup code for a relay connector of
// the given kind. The code is only an authority: the claimant supplies the
// MailMoose URL, and the code remembers which connector it creates.
func (s *Store) CreateRelayEnrollToken(ctx context.Context, accountID, inboxID, name string, kind RelayKind, ttl time.Duration) (string, error) {
	if !validRelayKind(kind) {
		return "", ErrForbidden
	}
	var n int
	if err := s.read.QueryRowContext(ctx, `SELECT count(*) FROM inboxes WHERE id=? AND account_id=?`, inboxID, accountID).Scan(&n); err != nil || n != 1 {
		return "", ErrForbidden
	}
	tok, err := auth.RandomToken(32)
	if err != nil {
		return "", err
	}
	id := idgen.New("hen")
	_, err = s.write.ExecContext(ctx, `INSERT INTO hermes_enroll_tokens(id,account_id,inbox_id,name,kind,token_hash,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?)`, id, accountID, inboxID, name, string(kind), auth.HashToken(tok), timeText(time.Now().UTC().Add(ttl)), nowText())
	return tok, err
}

type EnrollRecord struct {
	AccountID, InboxID, Name string
	Kind                     RelayKind
}

// EnrollHermesConnection consumes a one-time enrollment token and creates or
// replaces the gateway connection atomically. A gateway id owned by another
// account is rejected without consuming the token.
func (s *Store) EnrollHermesConnection(ctx context.Context, token, gatewayID, secretEnc, deliveryEnc string) (HermesConnection, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return HermesConnection{}, err
	}
	defer tx.Rollback()
	var id, accountID, inboxID, name, kind, exp string
	var used sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id,account_id,inbox_id,name,kind,expires_at,used_at FROM hermes_enroll_tokens WHERE token_hash=?`, auth.HashToken(token)).Scan(&id, &accountID, &inboxID, &name, &kind, &exp, &used)
	if err == sql.ErrNoRows {
		return HermesConnection{}, ErrNotFound
	}
	if err != nil {
		return HermesConnection{}, err
	}
	if used.Valid || parseTime(exp).Before(time.Now().UTC()) {
		return HermesConnection{}, ErrForbidden
	}
	if !validRelayKind(RelayKind(kind)) {
		kind = string(KindHermes)
	}
	rec := EnrollRecord{AccountID: accountID, InboxID: inboxID, Name: name, Kind: RelayKind(kind)}
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
// rejected. The record's Kind selects the connector type; empty means Hermes.
func (s *Store) CreateHermesConnection(ctx context.Context, r EnrollRecord, gatewayID, secretEnc, deliveryEnc string) (HermesConnection, error) {
	if r.Kind == "" {
		r.Kind = KindHermes
	}
	if !validRelayKind(r.Kind) {
		return HermesConnection{}, ErrForbidden
	}
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
	err := tx.QueryRowContext(ctx, `SELECT c.id,c.account_id FROM clients c JOIN client_push p ON p.client_id=c.id WHERE p.gateway_id=? AND c.type IN (`+relayKindsSQL+`)`, gatewayID).Scan(&existingID, &existingAccount)
	switch {
	case err == sql.ErrNoRows:
		id := idgen.New("hrm")
		if _, err = tx.ExecContext(ctx, `INSERT INTO clients(id,account_id,type,name,created_at) VALUES(?,?,?,?,?)`, id, r.AccountID, string(r.Kind), r.Name, now); err != nil {
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
		if _, err = tx.ExecContext(ctx, `UPDATE clients SET account_id=?,type=?,name=?,created_at=? WHERE id=?`, r.AccountID, string(r.Kind), r.Name, now, existingID); err != nil {
			return "", err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE client_push SET inbox_id=?,secret_encrypted=?,delivery_key_encrypted=?,last_ack_event_id=0 WHERE client_id=?`, r.InboxID, secretEnc, deliveryEnc, existingID); err != nil {
			return "", err
		}
		return existingID, nil
	}
}

func hermesConnectionFrom(id string, r EnrollRecord, gatewayID, secretEnc, deliveryEnc, now string) HermesConnection {
	kind := r.Kind
	if kind == "" {
		kind = KindHermes
	}
	return HermesConnection{ID: id, AccountID: r.AccountID, InboxID: r.InboxID, Name: r.Name, GatewayID: gatewayID, SecretEncrypted: secretEnc, DeliveryKeyEncrypted: deliveryEnc, Kind: string(kind), OutboundRole: "owner", CreatedAt: parseTime(now)}
}
func scanHermes(row interface{ Scan(...any) error }) (HermesConnection, error) {
	var h HermesConnection
	var cr string
	var lc sql.NullString
	err := row.Scan(&h.ID, &h.AccountID, &h.InboxID, &h.Name, &h.Kind, &h.GatewayID, &h.SecretEncrypted, &h.DeliveryKeyEncrypted, &h.LastAckEventID, &cr, &lc, &h.OutboundRole)
	if err != nil {
		return h, err
	}
	if h.Kind == "" {
		h.Kind = string(KindHermes)
	}
	if h.OutboundRole == "" {
		h.OutboundRole = "owner"
	}
	h.CreatedAt = parseTime(cr)
	h.LastConnectedAt = nullableTime(lc)
	return h, nil
}
func (s *Store) GetHermesConnectionByGateway(ctx context.Context, gatewayID string) (HermesConnection, error) {
	h, err := scanHermes(s.read.QueryRowContext(ctx, `SELECT c.id,c.account_id,p.inbox_id,c.name,c.type,p.gateway_id,p.secret_encrypted,p.delivery_key_encrypted,p.last_ack_event_id,c.created_at,p.last_connected_at,p.outbound_role FROM clients c JOIN client_push p ON p.client_id=c.id WHERE p.gateway_id=? AND c.type IN (`+relayKindsSQL+`)`, gatewayID))
	if err == sql.ErrNoRows {
		return h, ErrNotFound
	}
	return h, err
}
func (s *Store) ListHermesConnections(ctx context.Context, accountID string) ([]HermesConnection, error) {
	return s.ListRelayConnections(ctx, accountID, "")
}

// ListRelayConnections lists relay connections, optionally narrowed to one
// connector kind. kind=="" lists every relay connector.
func (s *Store) ListRelayConnections(ctx context.Context, accountID string, kind RelayKind) ([]HermesConnection, error) {
	q := `SELECT c.id,c.account_id,p.inbox_id,c.name,c.type,p.gateway_id,p.secret_encrypted,p.delivery_key_encrypted,p.last_ack_event_id,c.created_at,p.last_connected_at,p.outbound_role FROM clients c JOIN client_push p ON p.client_id=c.id WHERE c.account_id=? AND c.type IN (` + relayKindsSQL + `)`
	args := []any{accountID}
	if kind != "" {
		if !validRelayKind(kind) {
			return nil, ErrForbidden
		}
		q += ` AND c.type=?`
		args = append(args, string(kind))
	}
	q += ` ORDER BY c.created_at DESC`
	rows, err := s.read.QueryContext(ctx, q, args...)
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
	return s.AckHermesEventLogged(ctx, id, eventID, 1)
}

// AckHermesEventLogged advances a relay's acknowledgement cursor and records the
// acknowledged outcome in the client delivery log in the same transaction, so
// the durable cursor and the visible log cannot disagree. attempts is the number
// of deliveries the gateway took to acknowledge (1 for a first-try ack).
func (s *Store) AckHermesEventLogged(ctx context.Context, id string, eventID int64, attempts int) error {
	if attempts < 1 {
		attempts = 1
	}
	now := nowText()
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE client_push SET last_ack_event_id=MAX(last_ack_event_id,?) WHERE client_id=?`, eventID, id); err != nil {
		return err
	}
	if err = recordDeliveryLog(ctx, tx, id, eventID, "acknowledged", attempts, "", "", now); err != nil {
		return err
	}
	if err = recordEventDeliveryTx(ctx, tx, id, eventID, now); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordHermesDeliveryPending records that a relay event was written to the
// socket and is awaiting the gateway's acknowledgement. It increments the
// attempt count and, on a reconnect before the acknowledgement arrives, leaves
// the durable cursor untouched so the event is delivered again. It is
// best-effort: a failure to record the pending state must never block delivery.
func (s *Store) RecordHermesDeliveryPending(ctx context.Context, clientID string, eventID int64) error {
	now := nowText()
	_, err := s.write.ExecContext(ctx, `INSERT INTO client_delivery_log(client_id,event_id,status,attempts,last_error,next_attempt_at,created_at,updated_at)
		VALUES(?,?,?,1,'','',?,?)
		ON CONFLICT(client_id,event_id) DO UPDATE SET
			status=CASE WHEN client_delivery_log.status IN ('acknowledged') THEN client_delivery_log.status ELSE 'pending' END,
			attempts=CASE WHEN client_delivery_log.status IN ('acknowledged') THEN client_delivery_log.attempts ELSE client_delivery_log.attempts+1 END,
			updated_at=CASE WHEN client_delivery_log.status IN ('acknowledged') THEN client_delivery_log.updated_at ELSE excluded.updated_at END`,
		clientID, eventID, "pending", now, now)
	return err
}

// RecordHermesDeliveryAcknowledged records that a relay event was acknowledged
// and, in the same transaction, advances the cursor. attempts is the attempt
// count already accumulated in the log, so the summary reflects how many
// deliveries it took.
func (s *Store) RecordHermesDeliveryAcknowledged(ctx context.Context, clientID string, eventID int64, attempts int) error {
	if attempts < 1 {
		attempts = 1
	}
	now := nowText()
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE client_push SET last_ack_event_id=MAX(last_ack_event_id,?) WHERE client_id=?`, eventID, clientID); err != nil {
		return err
	}
	if err = recordDeliveryLog(ctx, tx, clientID, eventID, "acknowledged", attempts, "", "", now); err != nil {
		return err
	}
	if err = recordEventDeliveryTx(ctx, tx, clientID, eventID, now); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordHermesDeliveryFailed records that a relay socket dropped while an event
// was awaiting acknowledgement and the delivery window closed. The event stays
// unacknowledged, so it is redelivered on the next connection; the log surfaces
// why the attempt did not settle.
func (s *Store) RecordHermesDeliveryFailed(ctx context.Context, clientID string, eventID int64, errText string) error {
	now := nowText()
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var attempts int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT attempts FROM client_delivery_log WHERE client_id=? AND event_id=?),0)+1`, clientID, eventID).Scan(&attempts); err != nil {
		return err
	}
	if err = recordDeliveryLog(ctx, tx, clientID, eventID, "failed", attempts, errText, "", now); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) UpdateHermesConnectionName(ctx context.Context, accountID, id, name string) error {
	res, err := s.write.ExecContext(ctx, `UPDATE clients SET name=? WHERE id=? AND account_id=? AND type IN (`+relayKindsSQL+`)`, strings.TrimSpace(name), id, accountID)
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
	res, err := s.write.ExecContext(ctx, `UPDATE client_push SET outbound_role=? WHERE client_id=? AND EXISTS (SELECT 1 FROM clients c WHERE c.id=client_push.client_id AND c.account_id=? AND c.type IN (`+relayKindsSQL+`))`, role, id, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteHermesConnection(ctx context.Context, accountID, id string) error {
	res, err := s.write.ExecContext(ctx, `DELETE FROM clients WHERE id=? AND account_id=? AND type IN (`+relayKindsSQL+`)`, id, accountID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
