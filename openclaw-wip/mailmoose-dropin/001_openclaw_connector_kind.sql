-- WIP migration sketch only. Do not apply from this folder automatically.
--
-- Minimal path: preserve the existing Hermes relay table and add a product
-- connector-kind discriminator. Existing installations remain Hermes.
--
-- Before promotion, fold this into MailMoose's real migration mechanism and
-- add the matching Store/model field plus validation.

ALTER TABLE hermes_connections
ADD COLUMN connector_kind TEXT NOT NULL DEFAULT 'hermes'
CHECK (connector_kind IN ('hermes', 'openclaw'));

CREATE INDEX IF NOT EXISTS idx_hermes_connections_connector_kind
ON hermes_connections(account_id, connector_kind);
