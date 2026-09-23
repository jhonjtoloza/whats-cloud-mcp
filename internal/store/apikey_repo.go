package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/auth"
)

type apiKeyRepo struct{ db *sql.DB }

const apiKeyColumns = `id, tenant_id, name, key_hash, key_prefix, scopes, created_at, last_used_at, revoked_at`

func (r *apiKeyRepo) Create(ctx context.Context, k APIKey) error {
	switch {
	case k.ID == "":
		return errors.New("store: api key id is required")
	case k.TenantID == "":
		return errors.New("store: api key tenant id is required")
	case k.KeyHash == "":
		return errors.New("store: api key hash is required")
	}
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now().UTC()
	}

	_, err := r.db.ExecContext(ctx,
		`INSERT INTO api_keys (id, tenant_id, name, key_hash, key_prefix, scopes, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.TenantID, k.Name, k.KeyHash, k.KeyPrefix, auth.FormatScopes(k.Scopes), k.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("store: create api key: %w", err)
	}
	return nil
}

// GetByHash only ever returns live keys: a revoked key is indistinguishable
// from a key that never existed.
func (r *apiKeyRepo) GetByHash(ctx context.Context, keyHash string) (APIKey, error) {
	if keyHash == "" {
		return APIKey{}, ErrNotFound
	}
	row := r.db.QueryRowContext(ctx,
		`SELECT `+apiKeyColumns+` FROM api_keys WHERE key_hash = ? AND revoked_at IS NULL`, keyHash)

	k, err := scanAPIKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKey{}, ErrNotFound
	}
	if err != nil {
		return APIKey{}, fmt.Errorf("store: get api key: %w", err)
	}
	return k, nil
}

func (r *apiKeyRepo) ListByTenant(ctx context.Context, tenantID string) ([]APIKey, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+apiKeyColumns+` FROM api_keys WHERE tenant_id = ? ORDER BY created_at ASC, id ASC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list api keys: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan api key: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Revoke is scoped by tenant so one tenant can never revoke another's key.
func (r *apiKeyRepo) Revoke(ctx context.Context, tenantID, keyID string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = ? WHERE id = ? AND tenant_id = ? AND revoked_at IS NULL`,
		time.Now().UTC(), keyID, tenantID)
	if err != nil {
		return fmt.Errorf("store: revoke api key: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: revoke api key: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *apiKeyRepo) MarkUsed(ctx context.Context, keyID string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE api_keys SET last_used_at = ? WHERE id = ?`, time.Now().UTC(), keyID)
	if err != nil {
		return fmt.Errorf("store: mark api key used: %w", err)
	}
	return nil
}

// scanner is implemented by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanAPIKey(s scanner) (APIKey, error) {
	var (
		k          APIKey
		scopes     string
		lastUsedAt sql.NullTime
		revokedAt  sql.NullTime
	)
	if err := s.Scan(&k.ID, &k.TenantID, &k.Name, &k.KeyHash, &k.KeyPrefix,
		&scopes, &k.CreatedAt, &lastUsedAt, &revokedAt); err != nil {
		return APIKey{}, err
	}

	k.Scopes = auth.ParseScopes(scopes)
	if lastUsedAt.Valid {
		t := lastUsedAt.Time
		k.LastUsedAt = &t
	}
	if revokedAt.Valid {
		t := revokedAt.Time
		k.RevokedAt = &t
	}
	return k, nil
}
