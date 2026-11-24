package postgres

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationsFS is the DDL, embedded so the binary is self-contained: an operator
// who sets STORE=postgres does not need to mount SQL files (DATA-MODEL.md §5).
//
// The files are copied verbatim from docs/sql by scripts/ (see README), so the
// schema the code expects and the schema the docs describe cannot drift.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// advisoryLockKey is an arbitrary constant that identifies "codraw migrations".
// Any instance starting at the same time takes the same lock, so exactly one runs
// the DDL and the others wait and then see it as applied.
const advisoryLockKey int64 = 0x636F6472 // "codr"

// migration is one ordered DDL file.
type migration struct {
	version  int
	name     string
	sql      string
	checksum string
}

// roomTTLKey is the context key carrying the configured room TTL.
type roomTTLKey struct{}

// WithRoomTTL attaches the room TTL to ctx so CreateRoom can set expires_at
// without threading config through the store interface.
func WithRoomTTL(ctx context.Context, ttl time.Duration) context.Context {
	return context.WithValue(ctx, roomTTLKey{}, ttl)
}

// roomTTLFromContext returns the configured TTL, defaulting to 30 days.
func roomTTLFromContext(ctx context.Context) time.Duration {
	if v, ok := ctx.Value(roomTTLKey{}).(time.Duration); ok && v > 0 {
		return v
	}
	return 720 * time.Hour
}

// migrate applies every embedded migration inside an advisory lock, recording a
// checksum so a hand-edited applied file is detected rather than silently ignored
// (DATA-MODEL.md §5 rules 1–5).
func (s *Store) migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("postgres: acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return fmt.Errorf("postgres: acquire migration lock: %w", err)
	}
	defer func() {
		// Best effort: the lock is released when the session ends anyway.
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    int         PRIMARY KEY,
			name       text        NOT NULL,
			checksum   text        NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("postgres: create schema_migrations: %w", err)
	}

	applied, err := loadApplied(ctx, conn)
	if err != nil {
		return err
	}
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if prev, ok := applied[m.version]; ok {
			if prev != m.checksum {
				// Editing an applied migration is always a bug: the running schema
				// no longer matches the file, on this instance or any other.
				return fmt.Errorf("postgres: migration %s was modified after being applied (checksum %s != %s)",
					m.name, m.checksum, prev)
			}
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("postgres: begin migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("postgres: apply migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
			m.version, m.name, m.checksum); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("postgres: record migration %s: %w", m.name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("postgres: commit migration %s: %w", m.name, err)
		}
	}
	return nil
}

// loadApplied returns version → checksum for applied migrations.
func loadApplied(ctx context.Context, conn *pgxpool.Conn) (map[int]string, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("postgres: read schema_migrations: %w", err)
	}
	defer rows.Close()
	out := make(map[int]string)
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			return nil, fmt.Errorf("postgres: scan schema_migrations: %w", err)
		}
		out[v] = sum
	}
	return out, rows.Err()
}

// loadMigrations reads and orders the embedded files.
func loadMigrations() ([]migration, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("postgres: read migrations dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	out := make([]migration, 0, len(names))
	for _, name := range names {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, fmt.Errorf("postgres: read %s: %w", name, err)
		}
		version, err := versionFromName(name)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{
			version: version, name: name, sql: string(body), checksum: hex.EncodeToString(sum[:]),
		})
	}
	return out, nil
}

// versionFromName parses the numeric prefix of "0001_init.sql".
func versionFromName(name string) (int, error) {
	prefix, _, ok := strings.Cut(name, "_")
	if !ok {
		return 0, fmt.Errorf("postgres: migration %q has no version prefix", name)
	}
	var v int
	if _, err := fmt.Sscanf(prefix, "%d", &v); err != nil || v <= 0 {
		return 0, fmt.Errorf("postgres: migration %q has an invalid version prefix", name)
	}
	return v, nil
}

// ErrNoMigration is returned when no migration files are embedded at all, which
// would mean the build is broken rather than the database.
var ErrNoMigration = errors.New("postgres: no migrations embedded")
