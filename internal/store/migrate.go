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
	if current >= 1 {
		return nil
	}

	sqlBytes, err := fs.ReadFile(migrations, "migrations/001_init.up.sql")
	if err != nil {
		return fmt.Errorf("store: read 001_init.up.sql: %w", err)
	}
	if _, err := db.ExecContext(ctx, string(sqlBytes)); err != nil {
		return fmt.Errorf("store: apply 001_init: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO `+schemaTable+` (version, applied_at) VALUES (?, ?)`,
		1, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("store: record schema version: %w", err)
	}
	return nil
}

func bindUTC(t time.Time) time.Time { return t.UTC() }

func bindNullUTC(t *time.Time) any {
	if t == nil {
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
	if s == "" {
		return nil, nil
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
