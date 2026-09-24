-- Chat names: a conversation the caller can ask for by name.
--
-- Everything the gateway stored so far addressed a conversation by JID. For a
-- person that is workable, because the address book carries a name for the
-- phone number. For a GROUP it is not: WhatsApp keeps the subject in the group
-- metadata, whatsmeow fetches it live and persists none of it, so a group was
-- only ever reachable as "120363424550223300@g.us". A caller asking for the
-- group by the name it reads on its phone got nothing back.
--
-- This table is that missing index. It holds one display name per conversation
-- per tenant, written from the group metadata whatsmeow hands us on connect,
-- on join and on rename.
--
-- The name is a CACHE of what WhatsApp owns, never a source of truth: it is
-- replaced wholesale whenever WhatsApp reports the current subject, and a row
-- that never arrives simply leaves the reader with the JID it already had.
CREATE TABLE IF NOT EXISTS chats (
    tenant_id  TEXT      NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    chat_jid   TEXT      NOT NULL,
    name       TEXT      NOT NULL,
    updated_at TIMESTAMP NOT NULL,

    -- The tenant is part of the identity, not a filter applied afterwards: two
    -- tenants may both be members of the same group, and each must see only
    -- the row it wrote.
    PRIMARY KEY (tenant_id, chat_jid)
) WITHOUT ROWID;

-- The lookup this table exists for is "find the chat called <name>", which is a
-- LIKE scan over the names of one tenant. The index keeps that scan inside the
-- tenant's own rows instead of the whole table.
CREATE INDEX IF NOT EXISTS idx_chats_tenant_name ON chats (tenant_id, name);
