package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	ecsphoenixext "github.com/fiztoz/ecs-phoenix-ext"
)

func openTestSQLite(t *testing.T) *SQLite {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db")
	s, err := OpenSQLite(dsn, ecsphoenixext.Migrations)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSQLiteMigrateIdempotent(t *testing.T) {
	s := openTestSQLite(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second migrate must be a no-op: %v", err)
	}
}

func TestSQLiteQuotaRoundtrip(t *testing.T) {
	s := openTestSQLite(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SetQuota(ctx, "ns", "bkt", 1234); err != nil {
		t.Fatal(err)
	}
	// Replace.
	if err := s.SetQuota(ctx, "ns", "bkt", 9999); err != nil {
		t.Fatal(err)
	}
	q, err := s.Quotas(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if q["bkt"].QuotaBytes != 9999 {
		t.Fatalf("quota = %+v, want 9999", q)
	}
	if err := s.DeleteQuota(ctx, "ns", "bkt"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteQuota(ctx, "ns", "bkt"); err != nil {
		t.Fatalf("deleting a missing quota must not error: %v", err)
	}
	q, _ = s.Quotas(ctx, "ns")
	if len(q) != 0 {
		t.Fatalf("quotas after delete = %+v", q)
	}
}

func TestSQLiteStateRoundtripUTC(t *testing.T) {
	s := openTestSQLite(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// Bind a non-UTC zone; the store must normalise to UTC.
	loc := time.FixedZone("ICT", 7*3600)
	sample := time.Date(2026, 8, 18, 16, 55, 0, 0, loc) // 09:55 UTC
	ut := time.Date(2026, 8, 18, 16, 50, 0, 0, loc)
	polled := time.Date(2026, 8, 18, 17, 0, 0, 0, loc)

	row := StateRow{
		Namespace: "ns", Bucket: "bkt",
		UsedBytes: 11811160064, Objects: 2, MPUBytes: 0,
		SampleTime: &sample, UptodateTill: &ut, PolledAt: polled,
		OverStreak: 1, ConfirmedOver: false, LastError: "",
	}
	if err := s.UpsertStates(ctx, []StateRow{row}); err != nil {
		t.Fatal(err)
	}
	// Upsert updates in place.
	row.OverStreak = 2
	row.ConfirmedOver = true
	if err := s.UpsertStates(ctx, []StateRow{row}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.States(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (upsert must not duplicate)", len(rows))
	}
	got := rows[0]
	if got.UsedBytes != 11811160064 || got.OverStreak != 2 || !got.ConfirmedOver {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.SampleTime == nil || got.UptodateTill == nil {
		t.Fatal("nullable times must round-trip")
	}
	for name, tt := range map[string]time.Time{
		"sample_time":   *got.SampleTime,
		"uptodate_till": *got.UptodateTill,
		"polled_at":     got.PolledAt,
	} {
		if tt.Location() != time.UTC {
			t.Errorf("%s Location() = %v, want UTC", name, tt.Location())
		}
	}
	if !got.SampleTime.Equal(sample) {
		t.Errorf("sample instant shifted: %v vs %v", got.SampleTime, sample)
	}
}
