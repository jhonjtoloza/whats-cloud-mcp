package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type chatRepo struct {
	db *sql.DB
}

// upsertChat replaces the stored name of one conversation.
//
// The name is a cache of what WhatsApp owns, so the newest reading always wins
// rather than being merged with what was there.
const upsertChat = `INSERT INTO chats (tenant_id, chat_jid, name, updated_at)
	 VALUES (?, ?, ?, ?)
	 ON CONFLICT (tenant_id, chat_jid) DO UPDATE SET
	     name = excluded.name,
	     updated_at = excluded.updated_at`

func (r *chatRepo) Upsert(ctx context.Context, tenantID string, chat NamedChat) error {
	name := strings.TrimSpace(chat.Name)
	if name == "" {
		// A nameless conversation is not an error: plenty of groups have no
		// subject yet. Writing the blank would replace a name we already hold
		// and leave the reader with an empty string instead of the JID it
		// would otherwise fall back to.
		return nil
	}
	if _, err := r.db.ExecContext(ctx, upsertChat,
		tenantID, chat.ChatJID, name, time.Now().UTC()); err != nil {
		return fmt.Errorf("store: upsert chat name: %w", err)
	}
	return nil
}

func (r *chatRepo) UpsertBatch(ctx context.Context, tenantID string, chats []NamedChat) error {
	if len(chats) == 0 {
		return nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin chat name batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, upsertChat)
	if err != nil {
		return fmt.Errorf("store: prepare chat name batch: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	now := time.Now().UTC()
	for _, chat := range chats {
		name := strings.TrimSpace(chat.Name)
		if name == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, tenantID, chat.ChatJID, name, now); err != nil {
			return fmt.Errorf("store: upsert chat name %q: %w", chat.ChatJID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit chat name batch: %w", err)
	}
	return nil
}

func (r *chatRepo) Search(ctx context.Context, tenantID, query string, limit int) ([]NamedChat, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	// lower() on both sides rather than LIKE's own case folding: SQLite only
	// folds ASCII, and doing it symmetrically keeps an accented name matching
	// itself instead of matching nothing.
	rows, err := r.db.QueryContext(ctx,
		`SELECT chat_jid, name FROM chats
		 WHERE tenant_id = ? AND lower(name) LIKE lower(?) ESCAPE '\'
		 ORDER BY name ASC, chat_jid ASC
		 LIMIT ?`,
		tenantID, "%"+escapeLike(query)+"%", normalizeLimit(limit))
	if err != nil {
		return nil, fmt.Errorf("store: search chat names: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []NamedChat
	for rows.Next() {
		var chat NamedChat
		if err := rows.Scan(&chat.ChatJID, &chat.Name); err != nil {
			return nil, fmt.Errorf("store: scan chat name: %w", err)
		}
		out = append(out, chat)
	}
	return out, rows.Err()
}
