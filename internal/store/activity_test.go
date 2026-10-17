package store

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

// uniqueNS keeps assertions exact on a shared MariaDB schema, where a previous
// run may have left rows behind under the same namespace.
func uniqueNS(prefix string) string {
	return prefix + "-" + strconv.FormatInt(time.Now().UTC().UnixNano(), 36)
}

type engineCase struct {
	name string
	open func(t *testing.T) (Store, func())
}

// activityEngines runs the activity assertions against SQLite always, and
// against MariaDB when the CI/local gate supplies a DSN. Both engines must
// fold hourly rows identically.
func activityEngines(t *testing.T) []engineCase {
	t.Helper()
	out := []engineCase{{name: "sqlite", open: openReconcileSQLite}}
	if dsn := os.Getenv("TEST_MARIADB_DSN"); dsn != "" {
		out = append(out, engineCase{name: "mariadb", open: func(t *testing.T) (Store, func()) {
			return openReconcileMaria(t, dsn)
		}})
	} else {
		t.Log("TEST_MARIADB_DSN unset; MariaDB activity leg skipped")
	}
	return out
}

func TestActivityHourlyRollup(t *testing.T) {
	for _, eng := range activityEngines(t) {
		t.Run(eng.name, func(t *testing.T) {
			st, done := eng.open(t)
			t.Cleanup(done)
			ctx := context.Background()
			ns := uniqueNS("activity-rollup")
			hour := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)

			// Two polls inside one hour must fold into one row, not two.
			mustRecord(t, st, ActivityDelta{
				Namespace: ns, Bucket: "bkt-a", HourBucket: hour.Add(5 * time.Minute),
				ObjectsUp: 10, BytesUp: 1024, MPUPartsMax: 3, Active: true,
			})
			mustRecord(t, st, ActivityDelta{
				Namespace: ns, Bucket: "bkt-a", HourBucket: hour.Add(20 * time.Minute),
				ObjectsUp: 5, ObjectsDown: 2, BytesUp: 512, BytesDown: 128, MPUPartsMax: 7, Active: true,
			})
			// A quiet observation in the same hour: counts as measured, not busy.
			mustRecord(t, st, ActivityDelta{Namespace: ns, Bucket: "bkt-a", HourBucket: hour.Add(40 * time.Minute)})
			// A different hour, and a different bucket.
			mustRecord(t, st, ActivityDelta{
				Namespace: ns, Bucket: "bkt-a", HourBucket: hour.Add(time.Hour),
				ObjectsUp: 1, Active: true,
			})
			mustRecord(t, st, ActivityDelta{
				Namespace: ns, Bucket: "bkt-b", HourBucket: hour,
				ObjectsDown: 4, BytesDown: 4096, Active: true,
			})

			rows, err := st.ActivityHours(ctx, ns, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 3 {
				t.Fatalf("hourly rows = %d, want 3: %+v", len(rows), rows)
			}
			first := rows[0]
			if first.Bucket != "bkt-a" || !first.HourBucket.Equal(hour) {
				t.Fatalf("unexpected first row (ordered by bucket, hour): %+v", first)
			}
			if first.HourBucket.Location() != time.UTC {
				t.Errorf("hour_bucket must come back UTC, got %v", first.HourBucket.Location())
			}
			if first.Samples != 3 || first.ActiveSamples != 2 {
				t.Errorf("fold = %d samples / %d active, want 3 / 2", first.Samples, first.ActiveSamples)
			}
			if first.ObjectsUp != 15 || first.ObjectsDown != 2 {
				t.Errorf("object deltas = +%d/-%d, want +15/-2", first.ObjectsUp, first.ObjectsDown)
			}
			if first.BytesUp != 1536 || first.BytesDown != 128 {
				t.Errorf("byte deltas = +%d/-%d, want +1536/-128", first.BytesUp, first.BytesDown)
			}
			if first.MPUPartsMax != 7 {
				t.Errorf("mpu_parts_max = %d, want the highest observed (7)", first.MPUPartsMax)
			}

			// Window filter: only the later hour.
			late, err := st.ActivityHours(ctx, ns, hour.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if len(late) != 1 || !late[0].HourBucket.Equal(hour.Add(time.Hour)) {
				t.Fatalf("since filter returned %+v", late)
			}
			// Namespace isolation.
			other, err := st.ActivityHours(ctx, "activity-rollup-other", time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if len(other) != 0 {
				t.Fatalf("another namespace leaked rows: %+v", other)
			}

			sum := SummarizeHours(rows)
			a := sum["bkt-a"]
			if a.ObservedHours != 2 || a.BusyHours != 2 {
				t.Errorf("bkt-a window = %d busy / %d observed, want 2 / 2", a.BusyHours, a.ObservedHours)
			}
			if a.ObjectsUp != 16 || a.ObjectsDown != 2 || a.MPUPartsMax != 7 {
				t.Errorf("bkt-a totals = %+v", a)
			}
			if b := sum["bkt-b"]; b.ObservedHours != 1 || b.BusyHours != 1 || b.ObjectsDown != 4 {
				t.Errorf("bkt-b window = %+v", b)
			}
			if _, ok := sum["missing"]; ok {
				t.Error("summary invented a bucket with no rows")
			}
		})
	}
}

func TestActivityWindowCountsQuietHoursAsObserved(t *testing.T) {
	for _, eng := range activityEngines(t) {
		t.Run(eng.name, func(t *testing.T) {
			st, done := eng.open(t)
			t.Cleanup(done)
			ctx := context.Background()
			ns := uniqueNS("activity-quiet")
			base := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)

			// Four measured hours, movement in only one: a genuinely quiet
			// bucket must be distinguishable from an unmeasured one.
			for i := 0; i < 4; i++ {
				d := ActivityDelta{Namespace: ns, Bucket: "bkt", HourBucket: base.Add(time.Duration(i) * time.Hour)}
				if i == 2 {
					d.ObjectsUp, d.BytesUp, d.Active = 100, 2048, true
				}
				mustRecord(t, st, d)
			}
			rows, err := st.ActivityHours(ctx, ns, base)
			if err != nil {
				t.Fatal(err)
			}
			sum := SummarizeHours(rows)["bkt"]
			if sum.ObservedHours != 4 || sum.BusyHours != 1 {
				t.Fatalf("window = %d busy / %d observed, want 1 / 4", sum.BusyHours, sum.ObservedHours)
			}
		})
	}
}

func TestActivityPruneRetention(t *testing.T) {
	for _, eng := range activityEngines(t) {
		t.Run(eng.name, func(t *testing.T) {
			st, done := eng.open(t)
			t.Cleanup(done)
			ctx := context.Background()
			ns := uniqueNS("activity-prune")
			old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
			keep := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)

			mustRecord(t, st, ActivityDelta{Namespace: ns, Bucket: "bkt", HourBucket: old, ObjectsUp: 1, Active: true})
			mustRecord(t, st, ActivityDelta{Namespace: ns, Bucket: "bkt", HourBucket: keep, ObjectsUp: 1, Active: true})

			// Retention prunes by age across namespaces; the caller owns the
			// cutoff, so a shared database loses every row older than it.
			n, err := st.PruneActivity(ctx, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			if n < 1 {
				t.Fatalf("prune removed %d rows, want at least the 2020 row", n)
			}
			rows, err := st.ActivityHours(ctx, ns, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || !rows[0].HourBucket.Equal(keep) {
				t.Fatalf("after prune = %+v, want only the retained hour", rows)
			}
			// Idempotent: nothing left to prune in that range.
			n, err = st.PruneActivity(ctx, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Fatalf("second prune removed %d rows, want 0", n)
			}
		})
	}
}

func TestActivityRejectsIncompleteDeltas(t *testing.T) {
	st, done := openReconcileSQLite(t)
	defer done()
	ctx := context.Background()
	hour := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)

	cases := map[string]ActivityDelta{
		"no namespace": {Bucket: "bkt", HourBucket: hour},
		"no bucket":    {Namespace: "ns", HourBucket: hour},
		"no hour":      {Namespace: "ns", Bucket: "bkt"},
	}
	for name, d := range cases {
		if err := st.RecordActivity(ctx, []ActivityDelta{d}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	// An empty batch is a no-op, not an error: a poll where no bucket had
	// fresh metering records nothing.
	if err := st.RecordActivity(ctx, nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	rows, err := st.ActivityHours(ctx, "ns", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rejected deltas wrote rows: %+v", rows)
	}
}

func TestHourBucketTruncatesToUTC(t *testing.T) {
	loc := time.FixedZone("ICT", 7*3600)
	// 2026-10-05 21:37 +07:00 == 2026-10-05 14:37 UTC.
	got := HourBucket(time.Date(2026, 10, 5, 21, 37, 12, 456, loc))
	want := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)
	if !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("HourBucket = %v (%v), want %v UTC", got, got.Location(), want)
	}
}

func TestStateRowRoundTripsMPUParts(t *testing.T) {
	for _, eng := range activityEngines(t) {
		t.Run(eng.name, func(t *testing.T) {
			st, done := eng.open(t)
			t.Cleanup(done)
			ctx := context.Background()
			ns := uniqueNS("mpu-parts")
			now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			t.Cleanup(func() {
				_ = st.ReplaceObservation(context.Background(), Observation{
					Namespace:      ns,
					NamespaceState: NamespaceState{Namespace: ns, PolledAt: now},
				})
			})

			if err := st.ReplaceObservation(ctx, Observation{
				Namespace:      ns,
				NamespaceState: NamespaceState{Namespace: ns, UsedBytes: 10, Objects: 2, PolledAt: now},
				Buckets: []StateRow{{
					Namespace: ns, Bucket: "bkt", UsedBytes: 10, Objects: 2,
					MPUBytes: 4096, MPUParts: 7, PolledAt: now,
				}},
			}); err != nil {
				t.Fatal(err)
			}
			rows, err := st.States(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var found *StateRow
			for i := range rows {
				if rows[i].Namespace == ns && rows[i].Bucket == "bkt" {
					found = &rows[i]
				}
			}
			if found == nil {
				t.Fatalf("state row missing: %+v", rows)
			}
			if found.MPUParts != 7 || found.MPUBytes != 4096 {
				t.Fatalf("mpu columns = %d parts / %d bytes, want 7 / 4096", found.MPUParts, found.MPUBytes)
			}
		})
	}
}

func mustRecord(t *testing.T, st Store, d ActivityDelta) {
	t.Helper()
	if err := st.RecordActivity(context.Background(), []ActivityDelta{d}); err != nil {
		t.Fatal(err)
	}
}
