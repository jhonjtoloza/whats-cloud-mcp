package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type tenantRepo struct{ db *sql.DB }

func (r *tenantRepo) Create(ctx context.Context, t Tenant) error {
	if t.ID == "" {
		return errors.New("store: tenant id is required")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO tenants (id, name, created_at) VALUES (?, ?, ?)`,
		t.ID, t.Name, t.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("store: create tenant: %w", err)
	}
	return nil
}

func (r *tenantRepo) Get(ctx context.Context, id string) (Tenant, error) {
	var t Tenant
	err := r.db.QueryRowContext(ctx,
		`SELECT id, name, created_at FROM tenants WHERE id = ?`, id).
		Scan(&t.ID, &t.Name, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("store: get tenant: %w", err)
	}
	return t, nil
}

func (r *tenantRepo) List(ctx context.Context) ([]Tenant, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, name, created_at FROM tenants ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list tenants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Tenant
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan tenant: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
