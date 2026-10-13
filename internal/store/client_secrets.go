package store

import (
	"context"
)

// The scans and rewrite helpers below exist only for the one-time re-encrypt walk in internal/app (Service.ReencryptLegacySecrets),
// which rewrites pre-AAD ciphertext in client_push. They are not part of the
// normal client surface, and they go away with the walk.

// LegacyWebhookSecret is a webhook client whose signing secret is still stored
// in the pre-AAD format, with the account/inbox pair that binds it.
type LegacyWebhookSecret struct {
	ClientID, AccountID, InboxID, Encrypted string
}

// ListLegacyWebhookSecrets returns every webhook client whose stored signing
// secret has no AAD binding (no "v2." prefix).
func (s *Store) ListLegacyWebhookSecrets(ctx context.Context) ([]LegacyWebhookSecret, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT c.id,c.account_id,p.inbox_id,p.signing_secret_encrypted FROM clients c JOIN client_push p ON p.client_id=c.id WHERE c.type='webhook' AND p.signing_secret_encrypted!='' AND p.signing_secret_encrypted NOT LIKE 'v2.%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LegacyWebhookSecret{}
	for rows.Next() {
		var r LegacyWebhookSecret
		if err := rows.Scan(&r.ClientID, &r.AccountID, &r.InboxID, &r.Encrypted); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LegacyRelaySecret is a relay connection whose secret or delivery key is still
// stored in the pre-AAD format, with the gateway id that binds it. An empty
// value means that column is absent or already bound; the walk leaves it alone.
type LegacyRelaySecret struct {
	ClientID, GatewayID, SecretEncrypted, DeliveryKeyEncrypted string
}

// ListLegacyRelaySecrets returns every relay connection with at least one
// pre-AAD secret column.
func (s *Store) ListLegacyRelaySecrets(ctx context.Context) ([]LegacyRelaySecret, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT c.id,COALESCE(p.gateway_id,''),p.secret_encrypted,p.delivery_key_encrypted FROM clients c JOIN client_push p ON p.client_id=c.id WHERE c.type IN (`+relayKindsSQL+`) AND ((p.secret_encrypted!='' AND p.secret_encrypted NOT LIKE 'v2.%') OR (p.delivery_key_encrypted!='' AND p.delivery_key_encrypted NOT LIKE 'v2.%'))`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LegacyRelaySecret{}
	for rows.Next() {
		var r LegacyRelaySecret
		if err := rows.Scan(&r.ClientID, &r.GatewayID, &r.SecretEncrypted, &r.DeliveryKeyEncrypted); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RewriteWebhookSecret replaces a webhook client's stored signing secret. It is
// the walk's write half.
func (s *Store) RewriteWebhookSecret(ctx context.Context, clientID, encrypted string) error {
	_, err := s.write.ExecContext(ctx, `UPDATE client_push SET signing_secret_encrypted=? WHERE client_id=?`, encrypted, clientID)
	return err
}

// RewriteRelaySecrets replaces a relay connection's stored secret and delivery
// key. It is the walk's write half; a caller passes the existing value back for
// a column that is already AAD-bound.
func (s *Store) RewriteRelaySecrets(ctx context.Context, clientID, secretEnc, deliveryEnc string) error {
	_, err := s.write.ExecContext(ctx, `UPDATE client_push SET secret_encrypted=?,delivery_key_encrypted=? WHERE client_id=?`, secretEnc, deliveryEnc, clientID)
	return err
}
