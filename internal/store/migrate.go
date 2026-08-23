package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"strings"
	"time"
)

const schemaTable = "ext_ecs_usage_schema_migrations"

// Migrate applies the embedded migrations up to the latest version. The
// plugin owns its schema; Phoenix migrations never grow these tables.
func Migrate(ctx context.Context, db *sql.DB, migrations fs.FS) error {
	boot := `CREATE TABLE IF NOT EXISTS ` + schemaTable + ` (
	  version INT NOT NULL PRIMARY KEY,
	  applied_at TIMESTAMP NOT NULL
	)`
	if _, err := db.ExecContext(ctx, boot); err != nil {
		return fmt.Errorf("store: create migrations table: %w", err)
	}

	var current int
	row := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM `+schemaTable)
	if err := row.Scan(&current); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}

	// migrations are sequential: 001_init.up.sql, 002_*.up.sql, ...
	type migration struct {
		version int
		file    string
	}
	all := []migration{
		{1, "migrations/001_init.up.sql"},
		{2, "migrations/002_drop_sample_time.up.sql"},
	}
	for _, m := range all {
		if current >= m.version {
			continue
		}
		sqlBytes, err := fs.ReadFile(migrations, m.file)
		if err != nil {
			return fmt.Errorf("store: read %s: %w", m.file, err)
		}
		if _, err := db.ExecContext(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("store: apply %s: %w", m.file, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO `+schemaTable+` (version, applied_at) VALUES (?, ?)`,
			m.version, time.Now().UTC(),
		); err != nil {
			return fmt.Errorf("store: record schema version %d: %w", m.version, err)
		}
	}
	return nil
}

func bindUTC(t time.Time) time.Time { return t.UTC() }

// bindNullUTC maps the zero time to SQL NULL so ECS's "no timestamp" never
// lands in a TIMESTAMP column (MariaDB zero dates break parseTime=true).
func bindNullUTC(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC()
}

func scanTime(v any) (*time.Time, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case time.Time:
		t := x.UTC()
		return &t, nil
	case []byte:
		return parseScanTime(string(x))
	case string:
		return parseScanTime(x)
	default:
		return nil, fmt.Errorf("store: unexpected time column type %T", v)
	}
}

func parseScanTime(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "0000-00-00") {
		return nil, nil // legacy zero-date rows mean "no timestamp"
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			t = t.UTC()
			return &t, nil
		}
	}
	return nil, fmt.Errorf("store: unparseable time column %q", s)
}
