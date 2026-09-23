-- Initial schema for whats-cloud-mcp.
--
-- These tables live in the same SQLite file as whatsmeow's own sqlstore
-- schema. whatsmeow owns every table named whatsmeow_*; we own everything
-- below and track our own versions in wc_schema_migrations.

CREATE TABLE IF NOT EXISTS tenants (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS api_keys (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    key_hash     TEXT NOT NULL UNIQUE,
    key_prefix   TEXT NOT NULL,
    scopes       TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMP NOT NULL,
    last_used_at TIMESTAMP,
    revoked_at   TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_api_keys_tenant ON api_keys (tenant_id);

-- One WhatsApp session per tenant. Re-pairing mutates this row only; it never
-- touches api_keys, because keys belong to the tenant, not to the session.
CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL UNIQUE REFERENCES tenants (id) ON DELETE CASCADE,
    wa_jid     TEXT,
    status     TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    chat_jid      TEXT NOT NULL,
    sender_jid    TEXT NOT NULL,
    wa_message_id TEXT NOT NULL,
    direction     TEXT NOT NULL,
    body          TEXT NOT NULL DEFAULT '',
    media_type    TEXT,
    media_path    TEXT,
    timestamp     TIMESTAMP NOT NULL,
    created_at    TIMESTAMP NOT NULL
);

-- The index that answers "the last messages of chat X for tenant T".
CREATE INDEX IF NOT EXISTS idx_messages_tenant_chat_time
    ON messages (tenant_id, chat_jid, timestamp DESC);

-- WhatsApp redelivers messages; the same wa_message_id must land once per tenant.
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_tenant_wa_id
    ON messages (tenant_id, wa_message_id);

-- Content search currently runs as a LIKE scan over this table. FTS5 is
-- deliberately not used yet: its availability in the pure-Go driver is
-- unverified and a cgo dependency would break the static build.
