package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	ecsphoenixext "github.com/fiztoz/ecs-phoenix-ext"
)

func i64(v int64) *int64 { return &v }

func TestReplaceObservationReconcilesNamespace(t *testing.T) {
	engines := []struct {
		name string
		open func(t *testing.T) (Store, func())
	}{
		{name: "sqlite", open: openReconcileSQLite},
	}
	if dsn := os.Getenv("TEST_MARIADB_DSN"); dsn != "" {
		engines = append(engines, struct {
			name string
			open func(t *testing.T) (Store, func())
		}{name: "mariadb", open: func(t *testing.T) (Store, func()) {
			return openReconcileMaria(t, dsn)
		}})
	} else {
		t.Log("TEST_MARIADB_DSN unset; MariaDB reconcile leg skipped")
	}

	for _, eng := range engines {
		t.Run(eng.name, func(t *testing.T) {
			st, done := eng.open(t)
			t.Cleanup(done)
			assertReconcile(t, st)
		})
	}
}

func openReconcileSQLite(t *testing.T) (Store, func()) {
	t.Helper()
	s := openTestSQLite(t)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, func() {}
}

func openReconcileMaria(t *testing.T, dsn string) (Store, func()) {
	t.Helper()
	s, err := OpenMariaDB(dsn, ecsphoenixext.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, func() { _ = s.Close() }
}

func assertReconcile(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 5, 1, 2, 3, 0, time.UTC)
	block := i64(100)
	nsA := "reconcile-a"
	nsB := "reconcile-b"
	t.Cleanup(func() {
		_ = st.ReplaceObservation(context.Background(), Observation{
			Namespace:      nsA,
			NamespaceState: NamespaceState{Namespace: nsA, PolledAt: now},
		})
		_ = st.ReplaceObservation(context.Background(), Observation{
			Namespace:      nsB,
			NamespaceState: NamespaceState{Namespace: nsB, PolledAt: now},
		})
	})

	if err := st.ReplaceObservation(ctx, Observation{
		Namespace: nsA,
		NamespaceState: NamespaceState{
			Namespace: nsA, UsedBytes: 600, Objects: 2, PolledAt: now,
			OverStreak: 2, ConfirmedOver: true,
			Block:        Observed(true, block),
			DefaultBlock: Observed(true, nil),
			Notify:       Observed(false, nil),
		},
		Buckets: []StateRow{
			rowWithLimit(nsA, "keep", now, true),
			rowWithLimit(nsA, "drop", now, true),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceObservation(ctx, Observation{
		Namespace:      nsB,
		NamespaceState: NamespaceState{Namespace: nsB, UsedBytes: 1, PolledAt: now, Block: Observed(true, block)},
		Buckets:        []StateRow{rowWithLimit(nsB, "other", now, false)},
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.ReplaceObservation(ctx, Observation{
		Namespace:      nsA,
		NamespaceState: NamespaceState{Namespace: nsA, UsedBytes: 10, PolledAt: now, Block: Observed(true, nil), ConfirmedOver: false},
		Buckets:        []StateRow{rowWithLimit(nsA, "keep", now, false)},
	}); err != nil {
		t.Fatal(err)
	}
	byNS := bucketsByNamespace(t, st)
	if _, ok := byNS[nsA]["drop"]; ok {
		t.Fatal("reduced snapshot left a removed bucket")
	}
	if _, ok := byNS[nsA]["keep"]; !ok {
		t.Fatal("reduced snapshot dropped a retained bucket")
	}
	if _, ok := byNS[nsB]["other"]; !ok {
		t.Fatal("unrelated namespace was modified")
	}

	if err := st.ReplaceObservation(ctx, Observation{
		Namespace:      nsA,
		NamespaceState: NamespaceState{Namespace: nsA, UsedBytes: 0, PolledAt: now, Block: Observed(true, nil)},
	}); err != nil {
		t.Fatal(err)
	}
	byNS = bucketsByNamespace(t, st)
	if len(byNS[nsA]) != 0 {
		t.Fatalf("empty snapshot must delete namespace buckets, got %+v", byNS[nsA])
	}
	if _, ok := byNS[nsB]["other"]; !ok {
		t.Fatal("empty snapshot touched an unrelated namespace")
	}
	ns := namespaceByName(t, st)
	if got := ns[nsA]; !got.Block.Known || got.Block.Bytes != nil {
		t.Fatalf("explicit quota-off must stay distinct from unknown, got %+v", got.Block)
	}
	if got := ns[nsB]; !got.Block.Known || got.Block.Bytes == nil || *got.Block.Bytes != 100 {
		t.Fatalf("unrelated namespace threshold changed: %+v", got.Block)
	}

	if err := st.ReplaceObservation(ctx, Observation{
		Namespace: nsA,
		Buckets:   []StateRow{{Namespace: "other", Bucket: "nope", PolledAt: now}},
	}); err == nil {
		t.Fatal("mismatched bucket namespace must be rejected")
	}
	byNS = bucketsByNamespace(t, st)
	if _, ok := byNS[nsB]["other"]; !ok {
		t.Fatal("rejected replace changed durable state")
	}
}

func rowWithLimit(ns, bucket string, now time.Time, confirmed bool) StateRow {
	return StateRow{
		Namespace: ns, Bucket: bucket, UsedBytes: 200, Objects: 1,
		PolledAt: now, OverStreak: 2, ConfirmedOver: confirmed,
		Block: Observed(true, i64(100)),
	}
}

func bucketsByNamespace(t *testing.T, st Store) map[string]map[string]StateRow {
	t.Helper()
	rows, err := st.States(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]StateRow{}
	for _, r := range rows {
		if out[r.Namespace] == nil {
			out[r.Namespace] = map[string]StateRow{}
		}
		out[r.Namespace][r.Bucket] = r
	}
	return out
}

func namespaceByName(t *testing.T, st Store) map[string]NamespaceState {
	t.Helper()
	rows, err := st.NamespaceStates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]NamespaceState{}
	for _, r := range rows {
		out[r.Namespace] = r
	}
	return out
}

func TestSQLiteReconcileSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "reopen.db")
	ctx := context.Background()
	open := func() *SQLite {
		t.Helper()
		s, err := OpenSQLite(dsn, ecsphoenixext.Migrations)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := open()
	now := time.Date(2026, 10, 5, 4, 0, 0, 0, time.UTC)
	if err := s.ReplaceObservation(ctx, Observation{
		Namespace: "prod-ns",
		NamespaceState: NamespaceState{
			Namespace: "prod-ns", UsedBytes: 600, PolledAt: now,
			OverStreak: 2, ConfirmedOver: true, Block: Observed(true, i64(500)),
		},
		Buckets: []StateRow{rowWithLimit("prod-ns", "gone", now, true)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceObservation(ctx, Observation{
		Namespace: "prod-ns",
		NamespaceState: NamespaceState{
			Namespace: "prod-ns", UsedBytes: 0, PolledAt: now, Block: Observed(true, nil),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = open()
	t.Cleanup(func() { _ = s.Close() })
	rows, err := s.States(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("removed bucket resurrected after reopen: %+v", rows)
	}
	ns, err := s.NamespaceStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 1 || ns[0].ConfirmedOver || !ns[0].Block.Known || ns[0].Block.Bytes != nil {
		t.Fatalf("namespace observation did not survive reopen: %+v", ns)
	}
}
