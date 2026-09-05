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

func TestSQLiteStateRoundtripUTC(t *testing.T) {
	s := openTestSQLite(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// Bind a non-UTC zone; the store must normalise to UTC.
	loc := time.FixedZone("ICT", 7*3600)
	ut := time.Date(2026, 8, 18, 16, 50, 0, 0, loc)
	polled := time.Date(2026, 8, 18, 17, 0, 0, 0, loc)

	row := StateRow{
		Namespace: "ns", Bucket: "bkt",
		UsedBytes: 11811160064, Objects: 2, MPUBytes: 0,
		UptodateTill: &ut, PolledAt: polled,
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
	if got.UptodateTill == nil {
		t.Fatal("nullable times must round-trip")
	}
	for name, tt := range map[string]time.Time{
		"uptodate_till": *got.UptodateTill,
		"polled_at":     got.PolledAt,
	} {
		if tt.Location() != time.UTC {
			t.Errorf("%s Location() = %v, want UTC", name, tt.Location())
		}
	}
	if !got.UptodateTill.Equal(ut) {
		t.Errorf("uptodate instant shifted: %v vs %v", got.UptodateTill, ut)
	}
}

func TestSQLiteZeroTimesBindAsNull(t *testing.T) {
	s := openTestSQLite(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	row := StateRow{
		Namespace: "ns", Bucket: "bkt",
		UsedBytes: 1, Objects: 1, MPUBytes: 0,
		UptodateTill: &time.Time{}, // zero means "no timestamp"
		PolledAt:     time.Now().UTC(),
	}
	if err := s.UpsertStates(ctx, []StateRow{row}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.States(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].UptodateTill != nil {
		t.Fatalf("zero/absent times must come back nil, got %+v", rows[0])
	}
}

func TestSQLiteNamespaceQuotaRoundtrip(t *testing.T) {
	s := openTestSQLite(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// Initially nil.
	q, err := s.NamespaceQuota(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if q != nil {
		t.Fatalf("expected nil, got %+v", q)
	}

	// Set.
	if err := s.SetNamespaceQuota(ctx, "ns", 5000); err != nil {
		t.Fatal(err)
	}
	q, err = s.NamespaceQuota(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if q == nil || q.QuotaBytes != 5000 {
		t.Fatalf("quota = %+v, want 5000", q)
	}

	// Replace.
	if err := s.SetNamespaceQuota(ctx, "ns", 9999); err != nil {
		t.Fatal(err)
	}
	q, _ = s.NamespaceQuota(ctx, "ns")
	if q == nil || q.QuotaBytes != 9999 {
		t.Fatalf("quota = %+v, want 9999", q)
	}

	// Delete.
	if err := s.DeleteNamespaceQuota(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNamespaceQuota(ctx, "ns"); err != nil {
		t.Fatalf("deleting a missing namespace quota must not error: %v", err)
	}
	q, _ = s.NamespaceQuota(ctx, "ns")
	if q != nil {
		t.Fatalf("namespace quota after delete = %+v", q)
	}
}
