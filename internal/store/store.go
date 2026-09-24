// Package store owns persistence. It uses SQLite through the pure-Go
// modernc.org/sqlite driver (registered as "sqlite"), which keeps the build
// cgo-free so the gateway can ship as a static binary in a distroless image.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationsTable is deliberately namespaced with a wc_ prefix so it can never
// collide with the whatsmeow_* tables that whatsmeow's sqlstore.Container
// creates and upgrades in the very same database file.
const migrationsTable = "wc_schema_migrations"

// DB is the handle to the gateway database plus its repositories.
type DB struct {
	db *sql.DB

	tenants  *tenantRepo
	apiKeys  *apiKeyRepo
	sessions *sessionRepo
	messages *messageRepo
}

// Open connects to the SQLite database at path, creating it if needed.
//
// An empty path, ":memory:" or a path ending in ":memory:" opens a shared
// in-memory database instead, which is useful for tests.
func Open(path string) (*DB, error) {
	dsn, err := buildDSN(path)
	if err != nil {
		return nil, err
	}

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}

	// SQLite takes a database-wide write lock. A single writer plus WAL keeps
	// "database is locked" errors away on a small shared server, and the
	// workload here is nowhere near needing a connection pool.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	if err := sqlDB.PingContext(context.Background()); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("store: ping database: %w", err)
	}

	db := &DB{db: sqlDB}
	db.tenants = &tenantRepo{db: sqlDB}
	db.apiKeys = &apiKeyRepo{db: sqlDB}
	db.sessions = &sessionRepo{db: sqlDB}
	db.messages = &messageRepo{db: sqlDB}
	return db, nil
}

func buildDSN(path string) (string, error) {
	pragmas := url.Values{}
	pragmas.Add("_pragma", "busy_timeout(5000)")
	pragmas.Add("_pragma", "foreign_keys(1)")
	pragmas.Add("_pragma", "journal_mode(WAL)")
	pragmas.Add("_pragma", "synchronous(NORMAL)")

	if path == "" || strings.HasSuffix(path, ":memory:") {
		return "file::memory:?cache=shared&" + pragmas.Encode(), nil
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("store: resolve database path: %w", err)
	}
	return "file:" + abs + "?" + pragmas.Encode(), nil
}

// SQL exposes the underlying handle. It exists so whatsmeow's sqlstore can
// share the exact same database file and connection, and for tests.
func (d *DB) SQL() *sql.DB { return d.db }

// Close releases the database handle.
func (d *DB) Close() error { return d.db.Close() }

// Tenants returns the tenant repository.
func (d *DB) Tenants() Tenants { return d.tenants }

// APIKeys returns the API key repository.
func (d *DB) APIKeys() APIKeys { return d.apiKeys }

// Sessions returns the session repository.
func (d *DB) Sessions() Sessions { return d.sessions }

// Messages returns the message repository.
func (d *DB) Messages() Messages { return d.messages }

// Migrate applies every embedded migration that has not run yet. It is safe to
// call on every startup.
func (d *DB) Migrate(ctx context.Context) (err error) {
	// A migration may need to read whatsmeow's LID index, which lives in this
	// same file but is created by whatsmeow's own upgrade — and that runs
	// AFTER ours. See ensureLIDMapReadable.
	done, err := d.ensureLIDMapReadable(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := done(); cleanupErr != nil && err == nil {
			err = cleanupErr
		}
	}()

	if _, err := d.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+migrationsTable+` (
		name       TEXT PRIMARY KEY,
		applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("store: create migrations table: %w", err)
	}

	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("store: read migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if err := d.applyMigration(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

// ensureLIDMapReadable makes whatsmeow's LID index selectable for the duration
// of Migrate, and returns the cleanup that undoes it.
//
// The gateway migrates its own schema BEFORE whatsmeow upgrades its own, so on
// a fresh database whatsmeow_lid_map does not exist yet and a migration that
// joins it could not even be prepared, let alone run. A fresh database also has
// no addresses to canonicalise, so an empty stand-in is not a workaround but
// the honest answer: there are no mappings, because there is nothing to map.
//
// It is a TEMP VIEW rather than a table. whatsmeow owns every whatsmeow_*
// table and we never create one, and the view is dropped again before Migrate
// returns, so it can never shadow the real table once whatsmeow appears.
func (d *DB) ensureLIDMapReadable(ctx context.Context) (func() error, error) {
	present, err := tableExists(ctx, d.db, lidMapTable)
	if err != nil {
		return nil, err
	}
	if present {
		return func() error { return nil }, nil
	}

	if _, err := d.db.ExecContext(ctx,
		`CREATE TEMP VIEW `+lidMapTable+` (lid, pn) AS SELECT NULL, NULL WHERE 0`); err != nil {
		return nil, fmt.Errorf("store: create lid map stand-in: %w", err)
	}
	return func() error {
		if _, err := d.db.ExecContext(ctx, `DROP VIEW temp.`+lidMapTable); err != nil {
			return fmt.Errorf("store: drop lid map stand-in: %w", err)
		}
		return nil
	}, nil
}

func (d *DB) applyMigration(ctx context.Context, name string) error {
	var applied int
	if err := d.db.QueryRowContext(ctx,
		`SELECT count(*) FROM `+migrationsTable+` WHERE name = ?`, name).Scan(&applied); err != nil {
		return fmt.Errorf("store: check migration %s: %w", name, err)
	}
	if applied > 0 {
		return nil
	}

	body, err := migrationFS.ReadFile("migrations/" + name)
	if err != nil {
		return fmt.Errorf("store: read migration %s: %w", name, err)
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, string(body)); err != nil {
		return fmt.Errorf("store: apply migration %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO `+migrationsTable+` (name) VALUES (?)`, name); err != nil {
		return fmt.Errorf("store: record migration %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %s: %w", name, err)
	}
	return nil
}
