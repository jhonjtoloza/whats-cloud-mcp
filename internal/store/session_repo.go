package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type sessionRepo struct{ db *sql.DB }

const sessionColumns = `id, tenant_id, wa_jid, status, created_at, updated_at`

// EnsureForTenant returns the tenant's session, creating a pending one the
// first time. It never creates a second session for a tenant.
func (r *sessionRepo) EnsureForTenant(ctx context.Context, tenantID string) (Session, error) {
	existing, err := r.GetByTenant(ctx, tenantID)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Session{}, err
	}

	now := time.Now().UTC()
	session := Session{
		ID:        NewID(),
		TenantID:  tenantID,
		Status:    SessionPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO sessions (id, tenant_id, wa_jid, status, created_at, updated_at)
		 VALUES (?, ?, NULL, ?, ?, ?)
		 ON CONFLICT (tenant_id) DO NOTHING`,
		session.ID, session.TenantID, string(session.Status), session.CreatedAt, session.UpdatedAt); err != nil {
		return Session{}, fmt.Errorf("store: create session: %w", err)
	}

	// Re-read so a concurrent creator wins cleanly instead of returning a row
	// that was never inserted.
	return r.GetByTenant(ctx, tenantID)
}

func (r *sessionRepo) GetByTenant(ctx context.Context, tenantID string) (Session, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE tenant_id = ?`, tenantID)

	s, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("store: get session: %w", err)
	}
	return s, nil
}

// UpdateStatus mutates the session lifecycle only.
//
// Pairing, re-pairing and logging out all land here and nowhere else: api_keys
// is never touched, which is what guarantees that re-pairing a number keeps
// every tenant credential valid.
func (r *sessionRepo) UpdateStatus(ctx context.Context, tenantID string, status SessionStatus, waJID string) error {
	if err := status.validate(); err != nil {
		return err
	}

	var (
		res sql.Result
		err error
	)
	if waJID == "" {
		// An empty JID means "unknown right now", not "forget the pairing".
		res, err = r.db.ExecContext(ctx,
			`UPDATE sessions SET status = ?, updated_at = ? WHERE tenant_id = ?`,
			string(status), time.Now().UTC(), tenantID)
	} else {
		res, err = r.db.ExecContext(ctx,
			`UPDATE sessions SET status = ?, wa_jid = ?, updated_at = ? WHERE tenant_id = ?`,
			string(status), waJID, time.Now().UTC(), tenantID)
	}
	if err != nil {
		return fmt.Errorf("store: update session status: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update session status: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *sessionRepo) List(ctx context.Context) ([]Session, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan session: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func scanSession(s scanner) (Session, error) {
	var (
		session Session
		waJID   sql.NullString
		status  string
	)
	if err := s.Scan(&session.ID, &session.TenantID, &waJID, &status,
		&session.CreatedAt, &session.UpdatedAt); err != nil {
		return Session{}, err
	}
	session.Status = SessionStatus(status)
	if waJID.Valid && waJID.String != "" {
		v := waJID.String
		session.WAJID = &v
	}
	return session, nil
}
