package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// defaultMessageLimit applies when a caller passes a non-positive limit.
	defaultMessageLimit = 50
	// maxMessageLimit caps how much one request can pull out of the database.
	maxMessageLimit = 500
)

type messageRepo struct{ db *sql.DB }

// messageColumns is the read projection, and also the insert projection: the
// media_* bookkeeping columns at the end are written by UpdateMedia rather than
// by an insert, because a fresh row has never been fetched.
const messageColumns = `id, tenant_id, chat_jid, sender_jid, wa_message_id, direction, body,
	 media_type, media_path, direct_path, media_key, file_enc_sha256, file_sha256, file_length,
	 mime_type, mms_type, media_status, media_error, media_fetched_at, timestamp, created_at`

// insertColumns is messageColumns without the three fetch-bookkeeping columns,
// which an insert always leaves NULL.
const insertColumns = `id, tenant_id, chat_jid, sender_jid, wa_message_id, direction, body,
	 media_type, media_path, direct_path, media_key, file_enc_sha256, file_sha256, file_length,
	 mime_type, mms_type, timestamp, created_at`

// insertMessage is the single INSERT both Append and AppendBatch run. The
// conflict clause is what makes a redelivery — by the live stream or by a
// history sync — a no-op.
const insertMessage = `INSERT INTO messages (` + insertColumns + `)
	 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	 ON CONFLICT (tenant_id, wa_message_id) DO NOTHING`

// prepareMessage validates a message and fills in the fields a caller may leave
// to the repository.
func prepareMessage(m Message) (Message, error) {
	switch {
	case m.TenantID == "":
		return Message{}, errors.New("store: message tenant id is required")
	case m.ChatJID == "":
		return Message{}, errors.New("store: message chat jid is required")
	case m.WAMessageID == "":
		return Message{}, errors.New("store: message wa_message_id is required")
	case !m.Direction.Valid():
		return Message{}, fmt.Errorf("store: invalid message direction %q", string(m.Direction))
	}
	if m.ID == "" {
		m.ID = NewID()
	}
	if m.Timestamp.IsZero() {
		m.Timestamp = time.Now().UTC()
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	return m, nil
}

// messageArgs lays a message out in the order insertMessage expects.
func messageArgs(m Message) []any {
	return []any{
		m.ID, m.TenantID, m.ChatJID, m.SenderJID, m.WAMessageID, string(m.Direction),
		m.Body, m.MediaType, m.MediaPath, m.DirectPath, m.MediaKey, m.FileEncSHA256,
		m.FileSHA256, m.FileLength, m.MimeType, m.MMSType,
		m.Timestamp.UTC(), m.CreatedAt.UTC(),
	}
}

// Append stores a message, ignoring a redelivery of the same wa_message_id.
func (r *messageRepo) Append(ctx context.Context, m Message) error {
	m, err := prepareMessage(m)
	if err != nil {
		return err
	}

	if _, err := r.db.ExecContext(ctx, insertMessage, messageArgs(m)...); err != nil {
		return fmt.Errorf("store: append message: %w", err)
	}
	return nil
}

// AppendBatch stores many messages in one transaction and reports how many rows
// were new.
//
// Every row is validated before anything is written, so a malformed message in
// the middle of a history chunk cannot leave half a conversation stored.
func (r *messageRepo) AppendBatch(ctx context.Context, messages []Message) (int, error) {
	if len(messages) == 0 {
		return 0, nil
	}

	prepared := make([]Message, 0, len(messages))
	for i, m := range messages {
		ready, err := prepareMessage(m)
		if err != nil {
			return 0, fmt.Errorf("store: append batch: message %d: %w", i, err)
		}
		prepared = append(prepared, ready)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin message batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, insertMessage)
	if err != nil {
		return 0, fmt.Errorf("store: prepare message batch: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	inserted := 0
	for _, m := range prepared {
		res, err := stmt.ExecContext(ctx, messageArgs(m)...)
		if err != nil {
			return 0, fmt.Errorf("store: append batch: %w", err)
		}
		// DO NOTHING reports zero affected rows for a message we already hold,
		// which is exactly the "new history" count callers want.
		affected, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("store: append batch: %w", err)
		}
		inserted += int(affected)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit message batch: %w", err)
	}
	return inserted, nil
}

// OldestByChat returns the oldest stored message of a chat.
func (r *messageRepo) OldestByChat(ctx context.Context, tenantID, chatJID string) (Message, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+messageColumns+`
		 FROM messages
		 WHERE tenant_id = ? AND chat_jid = ?
		 ORDER BY timestamp ASC, id ASC
		 LIMIT 1`,
		tenantID, chatJID)
	if err != nil {
		return Message{}, fmt.Errorf("store: oldest message: %w", err)
	}
	found, err := collectMessages(rows)
	if err != nil {
		return Message{}, err
	}
	if len(found) == 0 {
		return Message{}, ErrNotFound
	}
	return found[0], nil
}

// SearchChats returns the chats whose address or sender matches the query.
//
// Like Search, this is a LIKE scan rather than FTS5, for the reason documented
// on the Messages interface.
func (r *messageRepo) SearchChats(ctx context.Context, tenantID, query string, limit int) ([]string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	pattern := "%" + escapeLike(query) + "%"

	rows, err := r.db.QueryContext(ctx,
		`SELECT chat_jid
		 FROM messages
		 WHERE tenant_id = ?
		   AND (chat_jid LIKE ? ESCAPE '\' OR sender_jid LIKE ? ESCAPE '\')
		 GROUP BY chat_jid
		 ORDER BY max(timestamp) DESC, chat_jid ASC
		 LIMIT ?`,
		tenantID, pattern, pattern, normalizeLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: search chats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var chatJID string
		if err := rows.Scan(&chatJID); err != nil {
			return nil, fmt.Errorf("store: scan chat jid: %w", err)
		}
		out = append(out, chatJID)
	}
	return out, rows.Err()
}

func (r *messageRepo) ListByChat(ctx context.Context, tenantID, chatJID string, limit int) ([]Message, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+messageColumns+`
		 FROM messages
		 WHERE tenant_id = ? AND chat_jid = ?
		 ORDER BY timestamp DESC, id DESC
		 LIMIT ?`,
		tenantID, chatJID, normalizeLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: list messages: %w", err)
	}
	return collectMessages(rows)
}

func (r *messageRepo) ListChats(ctx context.Context, tenantID string, limit int) ([]Chat, error) {
	// A window function picks the newest row per chat. max() with bare columns
	// would be shorter, but an aggregate drops the column's declared type and
	// the driver would then hand back the timestamp as a string.
	rows, err := r.db.QueryContext(ctx,
		`SELECT chat_jid, timestamp, body, direction, message_count
		 FROM (
		     SELECT chat_jid, timestamp, body, direction,
		            count(*) OVER (PARTITION BY chat_jid) AS message_count,
		            row_number() OVER (PARTITION BY chat_jid ORDER BY timestamp DESC, id DESC) AS row_num
		     FROM messages
		     WHERE tenant_id = ?
		 )
		 WHERE row_num = 1
		 ORDER BY timestamp DESC, chat_jid ASC
		 LIMIT ?`,
		tenantID, normalizeLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: list chats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Chat
	for rows.Next() {
		var (
			c         Chat
			direction string
		)
		if err := rows.Scan(&c.ChatJID, &c.LastMessageAt, &c.LastMessageBody, &direction, &c.MessageCount); err != nil {
			return nil, fmt.Errorf("store: scan chat: %w", err)
		}
		c.LastDirection = Direction(direction)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Search substring-matches message bodies with LIKE.
//
// LIKE is the deliberate interim choice; see the Messages interface for why
// FTS5 is not used yet. Callers are unaffected when that changes.
func (r *messageRepo) Search(ctx context.Context, tenantID, query string, limit int) ([]Message, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT `+messageColumns+`
		 FROM messages
		 WHERE tenant_id = ? AND body LIKE ? ESCAPE '\'
		 ORDER BY timestamp DESC, id DESC
		 LIMIT ?`,
		tenantID, "%"+escapeLike(query)+"%", normalizeLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: search messages: %w", err)
	}
	return collectMessages(rows)
}

func collectMessages(rows *sql.Rows) ([]Message, error) {
	defer func() { _ = rows.Close() }()

	var out []Message
	for rows.Next() {
		var (
			m              Message
			direction      string
			mediaType      sql.NullString
			mediaPath      sql.NullString
			directPath     sql.NullString
			fileLength     sql.NullInt64
			mimeType       sql.NullString
			mmsType        sql.NullString
			mediaStatus    sql.NullString
			mediaError     sql.NullString
			mediaFetchedAt sql.NullTime
		)
		if err := rows.Scan(&m.ID, &m.TenantID, &m.ChatJID, &m.SenderJID, &m.WAMessageID,
			&direction, &m.Body, &mediaType, &mediaPath, &directPath, &m.MediaKey,
			&m.FileEncSHA256, &m.FileSHA256, &fileLength, &mimeType, &mmsType,
			&mediaStatus, &mediaError, &mediaFetchedAt, &m.Timestamp, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan message: %w", err)
		}
		m.Direction = Direction(direction)
		m.MediaType = nullableString(mediaType)
		m.MediaPath = nullableString(mediaPath)
		m.DirectPath = nullableString(directPath)
		m.MimeType = nullableString(mimeType)
		m.MMSType = nullableString(mmsType)
		m.MediaStatus = nullableString(mediaStatus)
		m.MediaError = nullableString(mediaError)
		if fileLength.Valid {
			v := fileLength.Int64
			m.FileLength = &v
		}
		if mediaFetchedAt.Valid {
			v := mediaFetchedAt.Time.UTC()
			m.MediaFetchedAt = &v
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// nullableString turns a nullable column into the pointer the model uses, so a
// SQL NULL and an absent value stay the same thing all the way up.
func nullableString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

// GetByID returns one message of a tenant, addressed by either identifier.
//
// Callers reach a message through whatever id they were handed: MCP tools and
// the HTTP listing expose the gateway's own id, while anything derived from a
// WhatsApp event carries the wa_message_id. Accepting both here keeps that
// detail out of every caller. The tenant is always part of the lookup, so an id
// belonging to somebody else simply does not exist.
func (r *messageRepo) GetByID(ctx context.Context, tenantID, id string) (Message, error) {
	if tenantID == "" || id == "" {
		return Message{}, ErrNotFound
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT `+messageColumns+`
		 FROM messages
		 WHERE tenant_id = ? AND (id = ? OR wa_message_id = ?)
		 ORDER BY timestamp DESC, id DESC
		 LIMIT 1`,
		tenantID, id, id)
	if err != nil {
		return Message{}, fmt.Errorf("store: get message: %w", err)
	}
	found, err := collectMessages(rows)
	if err != nil {
		return Message{}, err
	}
	if len(found) == 0 {
		return Message{}, ErrNotFound
	}
	return found[0], nil
}

// UpdateMedia records the outcome of a fetch attempt.
//
// Only the media bookkeeping columns are written. The message, its body and its
// download reference are all left exactly as they were, so a download that
// fails — or one that comes back 410 because WhatsApp dropped the file — costs
// nothing but the attempt.
func (r *messageRepo) UpdateMedia(ctx context.Context, tenantID, id string, upd MediaUpdate) error {
	if tenantID == "" || id == "" {
		return ErrNotFound
	}
	if upd.FetchedAt.IsZero() {
		upd.FetchedAt = time.Now().UTC()
	}

	var mediaError any
	if upd.Error != "" {
		mediaError = upd.Error
	}
	// A successful fetch is the only thing that sets a path; every other
	// outcome leaves the previous one alone rather than inventing a NULL.
	var mediaPath any
	if upd.Path != nil {
		mediaPath = *upd.Path
	}

	res, err := r.db.ExecContext(ctx,
		`UPDATE messages
		 SET media_path       = coalesce(?, media_path),
		     media_status     = ?,
		     media_error      = ?,
		     media_fetched_at = ?
		 WHERE tenant_id = ? AND id = ?`,
		mediaPath, string(upd.Status), mediaError, upd.FetchedAt.UTC(), tenantID, id)
	if err != nil {
		return fmt.Errorf("store: update message media: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update message media: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func normalizeLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultMessageLimit
	case limit > maxMessageLimit:
		return maxMessageLimit
	default:
		return limit
	}
}

// escapeLike neutralises LIKE metacharacters so a user query is matched
// literally instead of turning into a pattern.
func escapeLike(s string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(s)
}
