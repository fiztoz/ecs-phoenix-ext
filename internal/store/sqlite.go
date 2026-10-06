package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"strings"

	_ "modernc.org/sqlite"
)

// SQLite is the Store implementation backed by modernc.org/sqlite (CGO off).
type SQLite struct {
	db         *sql.DB
	migrations fs.FS
}

// OpenSQLite opens a file: DSN with sane pragmas appended.
func OpenSQLite(dsn string, migrations fs.FS) (*SQLite, error) {
	dsn = ensureSQLiteParams(dsn)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}
	// Single writer keeps SQLite honest; ecs-phoenix-ext is a single replica.
	db.SetMaxOpenConns(1)
	return &SQLite{db: db, migrations: migrations}, nil
}

func ensureSQLiteParams(dsn string) string {
	var add []string
	if !strings.Contains(dsn, "_pragma=journal_mode") {
		add = append(add, "_pragma=journal_mode(WAL)")
	}
	if !strings.Contains(dsn, "_pragma=busy_timeout") {
		add = append(add, "_pragma=busy_timeout(5000)")
	}
	if len(add) == 0 {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + strings.Join(add, "&")
}

func (s *SQLite) Migrate(ctx context.Context) error {
	return Migrate(ctx, s.db, s.migrations)
}

const upsertStateSQLite = `
INSERT INTO ext_ecs_usage_state
  (namespace, bucket, used_bytes, objects, mpu_bytes, uptodate_till,
   polled_at, over_streak, confirmed_over, last_error,
   block_bytes, block_known, notify_bytes, notify_known)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(namespace, bucket) DO UPDATE SET
  used_bytes = excluded.used_bytes,
  objects = excluded.objects,
  mpu_bytes = excluded.mpu_bytes,
  uptodate_till = excluded.uptodate_till,
  polled_at = excluded.polled_at,
  over_streak = excluded.over_streak,
  confirmed_over = excluded.confirmed_over,
  last_error = excluded.last_error,
  block_bytes = excluded.block_bytes,
  block_known = excluded.block_known,
  notify_bytes = excluded.notify_bytes,
  notify_known = excluded.notify_known`

const upsertNamespaceSQLite = `
INSERT INTO ext_ecs_usage_namespace_state
  (namespace, used_bytes, objects, polled_at, over_streak, confirmed_over,
   default_block_bytes, default_known, block_bytes, block_known, notify_bytes, notify_known)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(namespace) DO UPDATE SET
  used_bytes = excluded.used_bytes,
  objects = excluded.objects,
  polled_at = excluded.polled_at,
  over_streak = excluded.over_streak,
  confirmed_over = excluded.confirmed_over,
  default_block_bytes = excluded.default_block_bytes,
  default_known = excluded.default_known,
  block_bytes = excluded.block_bytes,
  block_known = excluded.block_known,
  notify_bytes = excluded.notify_bytes,
  notify_known = excluded.notify_known`

func (s *SQLite) ReplaceObservation(ctx context.Context, obs Observation) error {
	return replaceObservation(ctx, s.db, upsertStateSQLite, upsertNamespaceSQLite, obs)
}

func (s *SQLite) States(ctx context.Context) ([]StateRow, error) {
	return queryStates(ctx, s.db)
}

func (s *SQLite) NamespaceStates(ctx context.Context) ([]NamespaceState, error) {
	return queryNamespaceStates(ctx, s.db)
}

func (s *SQLite) Close() error { return s.db.Close() }
