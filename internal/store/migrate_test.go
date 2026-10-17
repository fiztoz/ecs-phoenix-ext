package store

import (
	"context"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	ecsphoenixext "github.com/fiztoz/ecs-phoenix-ext"
)

// v4Migrations are the migration files that existed before derived activity
// landed. A deployed database is at this version, so 005 has to upgrade it in
// place rather than assume a fresh schema.
var v4Migrations = []struct {
	version int
	file    string
}{
	{1, "migrations/001_init.up.sql"},
	{2, "migrations/002_drop_sample_time.up.sql"},
	{3, "migrations/003_namespace_quota.up.sql"},
	{4, "migrations/004_observation_state.up.sql"},
}

func TestMigrateUpgradesVersion4DatabaseInPlace(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "v4.db")
	s, err := OpenSQLite(dsn, ecsphoenixext.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Build the pre-activity schema and pretend the pod recorded it.
	for _, m := range v4Migrations {
		body, err := fs.ReadFile(ecsphoenixext.Migrations, m.file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", m.file, err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO ext_ecs_usage_schema_migrations (version, applied_at) VALUES (?, ?)`,
			m.version, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	// A row written by the old code: no mpu_parts column existed.
	oldPolled := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO ext_ecs_usage_state
  (namespace, bucket, used_bytes, objects, mpu_bytes, uptodate_till,
   polled_at, over_streak, confirmed_over, block_known, notify_known)
VALUES ('prod-ns', 'legacy-bkt', 2048, 12, 0, ?, ?, 0, 0, 0, 0)`,
		oldPolled, oldPolled); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	if hasColumn(t, s, "ext_ecs_usage_state", "mpu_parts") {
		t.Fatal("setup: the v4 schema must not have mpu_parts yet")
	}

	// The upgrade the real deployment performs on start.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate v4 -> latest: %v", err)
	}

	var version int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM ext_ecs_usage_schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 5 {
		t.Fatalf("schema version = %d, want 5", version)
	}
	if !hasColumn(t, s, "ext_ecs_usage_state", "mpu_parts") {
		t.Fatal("migration 005 did not add mpu_parts to the existing state table")
	}
	// The pre-existing row survives and defaults to "no parts observed".
	rows, err := s.States(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Bucket != "legacy-bkt" || rows[0].UsedBytes != 2048 {
		t.Fatalf("legacy row did not survive the upgrade: %+v (err %v)", rows, err)
	}
	if rows[0].MPUParts != 0 {
		t.Fatalf("legacy mpu_parts = %d, want the 0 default", rows[0].MPUParts)
	}

	// New writes carry the column, and the activity table is queryable.
	if err := s.ReplaceObservation(ctx, Observation{
		Namespace: "prod-ns",
		NamespaceState: NamespaceState{
			Namespace: "prod-ns", UsedBytes: 4096, Objects: 20, PolledAt: oldPolled.Add(time.Hour),
		},
		Buckets: []StateRow{{
			Namespace: "prod-ns", Bucket: "legacy-bkt", UsedBytes: 4096, Objects: 20,
			MPUParts: 7, PolledAt: oldPolled.Add(time.Hour),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	rows, err = s.States(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].MPUParts != 7 {
		t.Fatalf("mpu_parts after upgrade = %+v, want 7", rows)
	}
	hours, err := s.ActivityHours(ctx, "prod-ns", time.Time{})
	if err != nil {
		t.Fatalf("activity table not usable after upgrade: %v", err)
	}
	if len(hours) != 0 {
		t.Fatalf("fresh activity table returned rows: %+v", hours)
	}
	// Idempotent: a second start changes nothing.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func hasColumn(t *testing.T, s *SQLite, table, column string) bool {
	t.Helper()
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return false
}
