-- Canonical addressing: one conversation, one address.
--
-- WhatsApp addresses the same person two ways -- by phone number
-- ("573004725680@s.whatsapp.net") and by LID ("15285906083900@lid") -- and the
-- live path stored whichever one arrived. One conversation therefore split into
-- two, and a query by phone number silently returned an older message rather
-- than the newest one. Nothing errored, which is what made it expensive.
--
-- There is a third way the same address splits: the device suffix. The tenant's
-- own address is stored as "573114276555:87@s.whatsapp.net", so every string
-- comparison against the plain address fails -- including the one that decides
-- whether a message is the tenant's own.
--
-- The write path stops new rows from splitting. This is the repair of the rows
-- already written: their addresses are repointed onto the canonical form, which
-- is the phone number with no device, and the direction of the tenant's own
-- messages is corrected.
--
-- whatsmeow_lid_map is whatsmeow's own table and is only ever READ here. Both
-- of its columns hold the bare user part of an address, never a full JID.

-- The guard. Repointing chat_jid is only safe while every WhatsApp message
-- exists exactly once per tenant, which idx_messages_tenant_wa_id guarantees:
-- that unique index does NOT include chat_jid, so the same message can never
-- already exist under both addresses and the merge cannot collide. On a
-- database where the index has been dropped, merging could put two rows on one
-- address and lose one of them, so the assumption is verified rather than
-- trusted. The CHECK fails the whole migration, which is the loud abort wanted
-- here: a data migration that cannot prove its precondition must not run.
CREATE TEMP TABLE wc_canonical_addresses_guard (
    no_duplicate_wa_message_ids INTEGER NOT NULL CHECK (no_duplicate_wa_message_ids = 1)
);

INSERT INTO wc_canonical_addresses_guard (no_duplicate_wa_message_ids)
SELECT CASE WHEN count(*) = 0 THEN 1 ELSE 0 END
FROM (
    SELECT tenant_id
    FROM messages
    GROUP BY tenant_id, wa_message_id
    HAVING count(*) > 1
);

-- The canonical address of every row, materialised BEFORE anything is written
-- so the UPDATE below can never read its own output.
--
-- An address that resolves to nothing keeps its LID. That is deliberate: a
-- phone number must never be invented, and a chat nobody can resolve is still
-- a chat that answers for its own messages. One conversation in the live
-- database is in exactly that state.
CREATE TEMP TABLE wc_canonical_addresses AS
WITH split AS (
    SELECT
        id,
        chat_jid,
        sender_jid,
        CASE WHEN instr(chat_jid, '@') > 0
             THEN substr(chat_jid, 1, instr(chat_jid, '@') - 1) ELSE '' END AS chat_user,
        CASE WHEN instr(chat_jid, '@') > 0
             THEN substr(chat_jid, instr(chat_jid, '@') + 1) ELSE '' END AS chat_server,
        CASE WHEN instr(sender_jid, '@') > 0
             THEN substr(sender_jid, 1, instr(sender_jid, '@') - 1) ELSE '' END AS sender_user,
        CASE WHEN instr(sender_jid, '@') > 0
             THEN substr(sender_jid, instr(sender_jid, '@') + 1) ELSE '' END AS sender_server
    FROM messages
),
-- The device suffix goes here: "573114276555:87" names the same person as
-- "573114276555", and only the second form can ever be compared or joined.
bare AS (
    SELECT
        id, chat_jid, sender_jid, chat_server, sender_server,
        CASE WHEN instr(chat_user, ':') > 0
             THEN substr(chat_user, 1, instr(chat_user, ':') - 1)
             ELSE chat_user END AS chat_user,
        CASE WHEN instr(sender_user, ':') > 0
             THEN substr(sender_user, 1, instr(sender_user, ':') - 1)
             ELSE sender_user END AS sender_user
    FROM split
)
SELECT
    bare.id AS id,
    -- A group, a broadcast list and a newsletter keep their own address: they
    -- are not people and have no phone-number form. Only the device goes.
    CASE
        WHEN bare.chat_server = '' THEN bare.chat_jid
        WHEN bare.chat_server = 'lid'
            THEN coalesce(chat_lid.pn || '@s.whatsapp.net', bare.chat_user || '@lid')
        ELSE bare.chat_user || '@' || bare.chat_server
    END AS chat_jid,
    CASE
        WHEN bare.sender_server = '' THEN bare.sender_jid
        WHEN bare.sender_server = 'lid'
            THEN coalesce(sender_lid.pn || '@s.whatsapp.net', bare.sender_user || '@lid')
        ELSE bare.sender_user || '@' || bare.sender_server
    END AS sender_jid
FROM bare
LEFT JOIN whatsmeow_lid_map AS chat_lid   ON chat_lid.lid = bare.chat_user
LEFT JOIN whatsmeow_lid_map AS sender_lid ON sender_lid.lid = bare.sender_user;

CREATE UNIQUE INDEX wc_canonical_addresses_id ON wc_canonical_addresses (id);

-- Only the rows that actually move are written, so a second run is a no-op
-- rather than a rewrite that happens to land on the same values.
UPDATE messages
SET chat_jid   = (SELECT c.chat_jid   FROM wc_canonical_addresses c WHERE c.id = messages.id),
    sender_jid = (SELECT c.sender_jid FROM wc_canonical_addresses c WHERE c.id = messages.id)
WHERE id IN (
    SELECT c.id
    FROM wc_canonical_addresses c
    JOIN messages m ON m.id = c.id
    WHERE c.chat_jid <> m.chat_jid OR c.sender_jid <> m.sender_jid
);

-- The tenant's own number, taken from the session and compared on the USER
-- part. sessions.wa_jid carries a device ("573114276555:87@s.whatsapp.net"),
-- so a raw string comparison against a message's sender would never match.
CREATE TEMP TABLE wc_canonical_own_numbers AS
SELECT
    tenant_id,
    CASE WHEN instr(wa_user, ':') > 0
         THEN substr(wa_user, 1, instr(wa_user, ':') - 1)
         ELSE wa_user END AS own_user
FROM (
    SELECT tenant_id,
           CASE WHEN instr(wa_jid, '@') > 0
                THEN substr(wa_jid, 1, instr(wa_jid, '@') - 1) ELSE '' END AS wa_user
    FROM sessions
    WHERE wa_jid IS NOT NULL AND wa_jid <> ''
)
WHERE wa_user <> '';

-- Messages the tenant sent from their own phone arrived through the same live
-- event as everything else and were stored as received, because that path
-- hardcoded the direction and never read IsFromMe. Senders are canonical by
-- now, so the comparison is exact.
UPDATE messages
SET direction = 'out'
WHERE direction = 'in'
  AND EXISTS (
      SELECT 1
      FROM wc_canonical_own_numbers o
      WHERE o.tenant_id = messages.tenant_id
        AND messages.sender_jid = o.own_user || '@s.whatsapp.net'
  );

DROP TABLE wc_canonical_own_numbers;
DROP TABLE wc_canonical_addresses;
DROP TABLE wc_canonical_addresses_guard;
