package app

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
