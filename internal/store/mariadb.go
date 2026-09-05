package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"strings"

	_ "github.com/go-sql-driver/mysql"
)

// MariaDB is the Store implementation backed by go-sql-driver/mysql.
type MariaDB struct {
	db         *sql.DB
	migrations fs.FS
}

// OpenMariaDB opens the DSN, adding parseTime/multiStatements when absent.
func OpenMariaDB(dsn string, migrations fs.FS) (*MariaDB, error) {
	dsn = ensureMariaParams(dsn)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open mariadb: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	return &MariaDB{db: db, migrations: migrations}, nil
}

func ensureMariaParams(dsn string) string {
	var add []string
	if !strings.Contains(dsn, "parseTime=") {
		add = append(add, "parseTime=true")
	}
	if !strings.Contains(dsn, "multiStatements=") {
		add = append(add, "multiStatements=true")
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

func (m *MariaDB) Migrate(ctx context.Context) error {
	return Migrate(ctx, m.db, m.migrations)
}

const upsertStateMaria = `
INSERT INTO ext_ecs_usage_state
  (namespace, bucket, used_bytes, objects, mpu_bytes, uptodate_till,
   polled_at, over_streak, confirmed_over, last_error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
  used_bytes = VALUES(used_bytes),
  objects = VALUES(objects),
  mpu_bytes = VALUES(mpu_bytes),
  uptodate_till = VALUES(uptodate_till),
  polled_at = VALUES(polled_at),
  over_streak = VALUES(over_streak),
  confirmed_over = VALUES(confirmed_over),
  last_error = VALUES(last_error)`

func (m *MariaDB) UpsertStates(ctx context.Context, rows []StateRow) error {
	for _, r := range rows {
		if _, err := m.db.ExecContext(ctx, upsertStateMaria,
			r.Namespace, r.Bucket, r.UsedBytes, r.Objects, r.MPUBytes,
			bindNullUTC(r.UptodateTill),
			bindUTC(r.PolledAt), r.OverStreak, r.ConfirmedOver, nullString(r.LastError),
		); err != nil {
			return fmt.Errorf("store: upsert state %s/%s: %w", r.Namespace, r.Bucket, err)
		}
	}
	return nil
}

const selectStates = `
SELECT namespace, bucket, used_bytes, objects, mpu_bytes, uptodate_till,
       polled_at, over_streak, confirmed_over, COALESCE(last_error, '')
FROM ext_ecs_usage_state`

func (m *MariaDB) States(ctx context.Context) ([]StateRow, error) {
	return queryStates(ctx, m.db)
}

func (m *MariaDB) Close() error { return m.db.Close() }

// --- shared query helpers ---

func queryStates(ctx context.Context, db *sql.DB) ([]StateRow, error) {
	rows, err := db.QueryContext(ctx, selectStates)
	if err != nil {
		return nil, fmt.Errorf("store: query states: %w", err)
	}
	defer rows.Close()
	var out []StateRow
	for rows.Next() {
		var r StateRow
		var uptodate, polled any
		var confirmed int
		if err := rows.Scan(&r.Namespace, &r.Bucket, &r.UsedBytes, &r.Objects, &r.MPUBytes,
			&uptodate, &polled, &r.OverStreak, &confirmed, &r.LastError); err != nil {
			return nil, fmt.Errorf("store: scan state: %w", err)
		}
		if r.UptodateTill, err = scanTime(uptodate); err != nil {
			return nil, err
		}
		pt, err := scanTime(polled)
		if err != nil {
			return nil, err
		}
		if pt != nil {
			r.PolledAt = *pt
		}
		r.ConfirmedOver = confirmed != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
