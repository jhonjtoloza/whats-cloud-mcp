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

const messageColumns = `id, tenant_id, chat_jid, sender_jid, wa_message_id, direction, body, media_type, media_path, timestamp, created_at`

// Append stores a message, ignoring a redelivery of the same wa_message_id.
func (r *messageRepo) Append(ctx context.Context, m Message) error {
	switch {
	case m.TenantID == "":
		return errors.New("store: message tenant id is required")
	case m.ChatJID == "":
		return errors.New("store: message chat jid is required")
	case m.WAMessageID == "":
		return errors.New("store: message wa_message_id is required")
	case !m.Direction.Valid():
		return fmt.Errorf("store: invalid message direction %q", string(m.Direction))
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

	_, err := r.db.ExecContext(ctx,
		`INSERT INTO messages (`+messageColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (tenant_id, wa_message_id) DO NOTHING`,
		m.ID, m.TenantID, m.ChatJID, m.SenderJID, m.WAMessageID, string(m.Direction),
		m.Body, m.MediaType, m.MediaPath, m.Timestamp.UTC(), m.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("store: append message: %w", err)
	}
	return nil
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
			m         Message
			direction string
			mediaType sql.NullString
			mediaPath sql.NullString
		)
		if err := rows.Scan(&m.ID, &m.TenantID, &m.ChatJID, &m.SenderJID, &m.WAMessageID,
			&direction, &m.Body, &mediaType, &mediaPath, &m.Timestamp, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan message: %w", err)
		}
		m.Direction = Direction(direction)
		if mediaType.Valid {
			v := mediaType.String
			m.MediaType = &v
		}
		if mediaPath.Valid {
			v := mediaPath.String
			m.MediaPath = &v
		}
		out = append(out, m)
	}
	return out, rows.Err()
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
