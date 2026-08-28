package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"strings"
	"time"

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
   polled_at, over_streak, confirmed_over, last_error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(namespace, bucket) DO UPDATE SET
  used_bytes = excluded.used_bytes,
  objects = excluded.objects,
  mpu_bytes = excluded.mpu_bytes,
  uptodate_till = excluded.uptodate_till,
  polled_at = excluded.polled_at,
  over_streak = excluded.over_streak,
  confirmed_over = excluded.confirmed_over,
  last_error = excluded.last_error`

func (s *SQLite) UpsertStates(ctx context.Context, rows []StateRow) error {
	for _, r := range rows {
		if _, err := s.db.ExecContext(ctx, upsertStateSQLite,
			r.Namespace, r.Bucket, r.UsedBytes, r.Objects, r.MPUBytes,
			bindNullUTC(r.UptodateTill),
			bindUTC(r.PolledAt), r.OverStreak, r.ConfirmedOver, nullString(r.LastError),
		); err != nil {
			return fmt.Errorf("store: upsert state %s/%s: %w", r.Namespace, r.Bucket, err)
		}
	}
	return nil
}

func (s *SQLite) States(ctx context.Context) ([]StateRow, error) {
	return queryStates(ctx, s.db)
}

func (s *SQLite) SetQuota(ctx context.Context, namespace, bucket string, quotaBytes int64) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO ext_ecs_usage_quotas (namespace, bucket, quota_bytes, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(namespace, bucket) DO UPDATE SET
  quota_bytes = excluded.quota_bytes, updated_at = excluded.updated_at`,
		namespace, bucket, quotaBytes, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("store: set quota %s/%s: %w", namespace, bucket, err)
	}
	return nil
}

func (s *SQLite) DeleteQuota(ctx context.Context, namespace, bucket string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM ext_ecs_usage_quotas WHERE namespace = ? AND bucket = ?`,
		namespace, bucket)
	if err != nil {
		return fmt.Errorf("store: delete quota %s/%s: %w", namespace, bucket, err)
	}
	return nil
}

func (s *SQLite) Quotas(ctx context.Context, namespace string) (map[string]QuotaRow, error) {
	return queryQuotas(ctx, s.db, namespace)
}

func (s *SQLite) SetNamespaceQuota(ctx context.Context, namespace string, quotaBytes int64) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO ext_ecs_usage_namespace_quotas (namespace, quota_bytes, updated_at)
VALUES (?, ?, ?)
ON CONFLICT(namespace) DO UPDATE SET
  quota_bytes = excluded.quota_bytes, updated_at = excluded.updated_at`,
		namespace, quotaBytes, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("store: set namespace quota %s: %w", namespace, err)
	}
	return nil
}

func (s *SQLite) DeleteNamespaceQuota(ctx context.Context, namespace string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM ext_ecs_usage_namespace_quotas WHERE namespace = ?`,
		namespace)
	if err != nil {
		return fmt.Errorf("store: delete namespace quota %s: %w", namespace, err)
	}
	return nil
}

func (s *SQLite) NamespaceQuota(ctx context.Context, namespace string) (*NamespaceQuotaRow, error) {
	return queryNamespaceQuota(ctx, s.db, namespace)
}

func (s *SQLite) Close() error { return s.db.Close() }
