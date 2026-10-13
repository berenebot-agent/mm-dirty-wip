package app

import (
	"context"
	"log/slog"
	"strings"

	"github.com/dellarb/mailmoose/internal/cryptox"
)

// RelaySecretAAD binds a relay connection's secret and delivery key to its
// gateway id, which the claiming host supplies at enrollment and which is
// unique and immutable for the connection's life. It is the only row identity
// available when the ciphertext is created, because the connection row itself
// does not exist yet.
func RelaySecretAAD(gatewayID string) string { return "relay:" + gatewayID }

// WebhookSecretAAD binds a webhook client's signing secret to its owning
// account and inbox. Neither the client row id nor any other row-scoped
// identifier exists when the handler encrypts the secret, and a webhook client
// never moves inbox, so the immutable (account, inbox) pair is its identity.
func WebhookSecretAAD(accountID, inboxID string) string { return "whk:" + accountID + ":" + inboxID }

// boundSecret reports whether a stored value is already AAD-bound.
func boundSecret(encoded string) bool {
	return encoded == "" || strings.HasPrefix(encoded, "v2.")
}

// ReencryptLegacySecrets rewrites every pre-AAD ciphertext in client_push: the
// webhook signing secret, and a relay connection's secret and delivery key.
// Those values were written before every stored secret carried an AAD binding,
// so the read paths no longer open them; this one-time walk converts them in
// place, using the current derived key and, where a passphrase key predates the
// PBKDF2 change, the legacy derivation as a fallback.
//
// It is idempotent and safe to call on every start: a value that is already
// bound is left untouched. cmd/server calls it once at startup, before any
// worker can read a secret. It returns the number of rows it rewrote and the
// number it could not (a row that cannot be opened stays unreadable, and is
// reported rather than silently dropped).
func (s *Service) ReencryptLegacySecrets(ctx context.Context) (migrated, failed int) {
	log := s.Log
	if log == nil {
		log = slog.Default()
	}
	legacyKey, hasLegacyKey := cryptox.DeprecatedLegacyKey(s.Config.AppEncryptionKey)

	// openAny unwraps a pre-AAD blob with the current key, then with the
	// pre-PBKDF2 derivation.
	openAny := func(encoded string) ([]byte, bool) {
		if b, err := cryptox.DeprecatedOpenLegacy(s.EncryptionKey, encoded); err == nil {
			return b, true
		}
		if hasLegacyKey {
			if b, err := cryptox.DeprecatedOpenLegacy(legacyKey, encoded); err == nil {
				return b, true
			}
		}
		return nil, false
	}

	hooks, err := s.Store.ListLegacyWebhookSecrets(ctx)
	if err != nil {
		failed++
		log.Warn("legacy secret scan failed", "kind", "webhook", "error", err)
	}
	for _, h := range hooks {
		plain, ok := openAny(h.Encrypted)
		if !ok {
			failed++
			log.Warn("legacy webhook secret could not be opened", "client_id", h.ClientID)
			continue
		}
		enc, eerr := s.EncryptSecretAAD(WebhookSecretAAD(h.AccountID, h.InboxID), plain)
		if eerr == nil {
			eerr = s.Store.RewriteWebhookSecret(ctx, h.ClientID, enc)
		}
		if eerr != nil {
			failed++
			log.Warn("legacy webhook secret could not be rewritten", "client_id", h.ClientID, "error", eerr)
			continue
		}
		migrated++
	}

	relays, err := s.Store.ListLegacyRelaySecrets(ctx)
	if err != nil {
		failed++
		log.Warn("legacy secret scan failed", "kind", "relay", "error", err)
	}
	for _, r := range relays {
		aad := RelaySecretAAD(r.GatewayID)
		secretEnc, deliveryEnc := r.SecretEncrypted, r.DeliveryKeyEncrypted
		for _, col := range []struct {
			name  string
			value *string
		}{
			{"secret", &secretEnc},
			{"delivery_key", &deliveryEnc},
		} {
			if boundSecret(*col.value) {
				continue
			}
			plain, ok := openAny(*col.value)
			if !ok {
				failed++
				log.Warn("legacy relay secret could not be opened", "client_id", r.ClientID, "column", col.name)
				continue
			}
			enc, eerr := s.EncryptSecretAAD(aad, plain)
			if eerr != nil {
				failed++
				log.Warn("legacy relay secret could not be rewritten", "client_id", r.ClientID, "column", col.name, "error", eerr)
				continue
			}
			*col.value = enc
		}
		if secretEnc == r.SecretEncrypted && deliveryEnc == r.DeliveryKeyEncrypted {
			continue
		}
		if werr := s.Store.RewriteRelaySecrets(ctx, r.ClientID, secretEnc, deliveryEnc); werr != nil {
			failed++
			log.Warn("legacy relay secrets could not be written back", "client_id", r.ClientID, "error", werr)
			continue
		}
		migrated++
	}
	return migrated, failed
}
