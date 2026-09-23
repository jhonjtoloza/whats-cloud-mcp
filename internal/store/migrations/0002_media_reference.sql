-- Lazy media: store the reference, never the bytes.
--
-- Media is downloaded on demand and never on receipt. Eagerly downloading every
-- image, video and sticker would fill a small shared server with data nobody
-- reads, so a message row records only what whatsmeow's
-- DownloadMediaWithPath needs to fetch the bytes later. The tradeoff, handled
-- rather than hidden, is that WhatsApp expires media server-side: a reference
-- may outlive its bytes, which is what media_status reports.
--
-- media_key, file_enc_sha256 and file_sha256 ARE SECRETS. They decrypt the
-- attachment on WhatsApp's CDN. Storing them is not a new exposure class here —
-- message bodies already live in this table in plaintext — but anything that
-- dumps, exports or logs this table is handling key material, not metadata.
--
-- media_path and media_type from 0001 are kept as they were.

ALTER TABLE messages ADD COLUMN direct_path      TEXT;
ALTER TABLE messages ADD COLUMN media_key        BLOB;
ALTER TABLE messages ADD COLUMN file_enc_sha256  BLOB;
ALTER TABLE messages ADD COLUMN file_sha256      BLOB;
ALTER TABLE messages ADD COLUMN file_length      INTEGER;
ALTER TABLE messages ADD COLUMN mime_type        TEXT;
ALTER TABLE messages ADD COLUMN mms_type         TEXT;

-- media_status is NULL until somebody asks for the bytes:
--   NULL          never requested
--   available     the bytes are on disk at media_path
--   unavailable   WhatsApp answered 403/404/410; the media is gone for good
--   failed        something else went wrong and the fetch may be retried
ALTER TABLE messages ADD COLUMN media_status     TEXT;
ALTER TABLE messages ADD COLUMN media_error      TEXT;
ALTER TABLE messages ADD COLUMN media_fetched_at TIMESTAMP;
