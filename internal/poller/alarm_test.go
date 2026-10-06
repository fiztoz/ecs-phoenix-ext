package poller

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	ecsphoenixext "github.com/fiztoz/ecs-phoenix-ext"
	"github.com/fiztoz/ecs-phoenix-ext/internal/ecs"
	"github.com/fiztoz/ecs-phoenix-ext/internal/store"
)

func openPollerSQLite(t *testing.T) *store.SQLite {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "alarm.db")
	s, err := store.OpenSQLite(dsn, ecsphoenixext.Migrations)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConfirmedAlarmsSurviveRestartAndPartialPoll(t *testing.T) {
	now := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	st := openPollerSQLite(t)
	src := &fullSource{
		fakeSource: fakeSource{results: []pollResult{{payload: payload(600, 0, now)}}},
		metas: map[string]ecs.BucketMeta{
			"bkt-one": {Name: "bkt-one", BlockSize: 100, HasBlock: true},
		},
		nsmeta: &ecs.NamespaceMeta{Name: "prod-ns", BlockSize: 500, HasBlock: true, NotificationSize: 400, HasNotify: true, DefaultBucketBlock: 10, HasDefaultBucketBlock: true},
	}
	p := newTestPoller(t, src, st, now)
	p.PollOnce(context.Background())
	p.PollOnce(context.Background())
	if !p.Snapshot().NamespaceConfirmedOver || !p.Snapshot().Buckets[0].ConfirmedOver {
		t.Fatal("setup: confirmed namespace and bucket must trip quota health")
	}

	restarted := New(context.Background(), &fakeSource{results: []pollResult{{payload: payload(1, 0, now)}}}, st, "prod-ns", 15*time.Minute, quietLog())
	restarted.now = func() time.Time { return now }
	if !restarted.Snapshot().NamespaceConfirmedOver || !restarted.Snapshot().Buckets[0].ConfirmedOver {
		t.Fatal("restart cleared a confirmed alarm before the next poll")
	}
	snap := restarted.Snapshot()
	if !snap.NamespaceConfirmedOver || snap.NamespaceBlockSize == nil || *snap.NamespaceBlockSize != 500 {
		t.Fatalf("namespace alarm/threshold not restored: %+v", snap)
	}
	if len(snap.Buckets) != 1 || !snap.Buckets[0].ConfirmedOver || snap.Buckets[0].BlockSize == nil {
		t.Fatalf("bucket alarm/threshold not restored: %+v", snap.Buckets)
	}

	// Billing succeeds under the cached limit while both inventory endpoints
	// fail. That is not an authoritative recovery, so the alarm must stay.
	restarted.PollOnce(context.Background())
	if !restarted.Snapshot().NamespaceConfirmedOver || !restarted.Snapshot().Buckets[0].ConfirmedOver {
		t.Fatal("billing-only success cleared a restored alarm")
	}
	if !restarted.Snapshot().Buckets[0].ConfirmedOver || !restarted.Snapshot().NamespaceConfirmedOver {
		t.Fatalf("partial poll persisted a false recovery: %+v", restarted.Snapshot())
	}

	// Authoritative under-limit clears both.
	src.metas["bkt-one"] = ecs.BucketMeta{Name: "bkt-one", BlockSize: 100, HasBlock: true}
	src.nsmeta = &ecs.NamespaceMeta{Name: "prod-ns", BlockSize: 500, HasBlock: true}
	src.results = []pollResult{{payload: payload(50, 0, now)}}
	src.calls = 0
	src.berr, src.nerr = nil, nil
	under := newTestPoller(t, src, st, now)
	under.PollOnce(context.Background())
	if under.Snapshot().NamespaceConfirmedOver || under.Snapshot().Buckets[0].ConfirmedOver {
		t.Fatalf("authoritative under-limit must clear: %+v", under.Snapshot())
	}
	if under.Snapshot().NamespaceConfirmedOver || under.Snapshot().Buckets[0].ConfirmedOver {
		t.Fatal("quota health stayed down after authoritative recovery")
	}
}

func TestBillingFailureDoesNotDeleteBuckets(t *testing.T) {
	now := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	mem := store.NewMem()
	src := &fullSource{
		fakeSource: fakeSource{results: []pollResult{
			{payload: payload(200, 0, now)},
			{err: errors.New("ecs down")},
		}},
		metas:  map[string]ecs.BucketMeta{"bkt-one": {Name: "bkt-one", HasBlock: true, BlockSize: 100}},
		nsmeta: &ecs.NamespaceMeta{Name: "prod-ns"},
	}
	p := newTestPoller(t, src, mem, now)
	p.PollOnce(context.Background())
	p.PollOnce(context.Background())
	rows, err := mem.States(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("failed poll changed durable buckets: %+v", rows)
	}
}

func TestRemovedBucketsStayGoneAfterRestart(t *testing.T) {
	now := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "gone.db")
	ctx := context.Background()
	open := func() *store.SQLite {
		t.Helper()
		s, err := store.OpenSQLite(dsn, ecsphoenixext.Migrations)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		return s
	}
	st := open()
	two := payload(200, 0, now)
	two.Buckets = append(two.Buckets, ecs.BucketBilling{
		Name: "gone", Namespace: "prod-ns", UsedBytes: 200, UptodateTill: now,
	})
	src := &fullSource{
		fakeSource: fakeSource{results: []pollResult{{payload: two}, {payload: payload(10, 0, now)}}},
		metas: map[string]ecs.BucketMeta{
			"bkt-one": {Name: "bkt-one", HasBlock: true, BlockSize: 100},
			"gone":    {Name: "gone", HasBlock: true, BlockSize: 100},
		},
		nsmeta: &ecs.NamespaceMeta{Name: "prod-ns"},
	}
	p := newTestPoller(t, src, st, now)
	p.PollOnce(context.Background())
	p.PollOnce(context.Background())
	p.PollOnce(context.Background())
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st = open()
	t.Cleanup(func() { _ = st.Close() })
	restarted := New(context.Background(), &fakeSource{}, st, "prod-ns", 15*time.Minute, quietLog())
	snap := restarted.Snapshot()
	if len(snap.Buckets) != 1 || snap.Buckets[0].Name != "bkt-one" {
		t.Fatalf("removed bucket resurrected: %+v", snap.Buckets)
	}
	if len(restarted.Snapshot().Buckets) != 1 {
		t.Fatal("resurrected alarm tripped quota health")
	}
}

func TestInventoryEndpointsAreIndependent(t *testing.T) {
	now := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	mem := store.NewMem()
	src := &fullSource{
		fakeSource: fakeSource{results: []pollResult{{payload: payload(600, 0, now)}}},
		metas:      map[string]ecs.BucketMeta{"bkt-one": {Name: "bkt-one", HasBlock: true, BlockSize: 100, HasNotify: true, NotificationSize: 50}},
		nsmeta: &ecs.NamespaceMeta{
			Name: "prod-ns", HasBlock: true, BlockSize: 500,
			HasNotify: true, NotificationSize: 400, HasDefaultBucketBlock: true, DefaultBucketBlock: 10,
		},
	}
	p := newTestPoller(t, src, mem, now)
	p.PollOnce(context.Background())
	p.PollOnce(context.Background())
	if !p.Snapshot().NamespaceConfirmedOver {
		t.Fatal("setup: namespace alarm not confirmed")
	}

	// Bucket failure must not hide a successful namespace quota-off.
	src.berr = errors.New("buckets down")
	src.nsmeta = &ecs.NamespaceMeta{Name: "prod-ns"}
	p.PollOnce(context.Background())
	snap := p.Snapshot()
	if snap.NamespaceConfirmedOver || snap.NamespaceBlockKnown && snap.NamespaceBlockSize != nil {
		t.Fatalf("successful namespace quota-off was ignored: block=%v known=%v confirmed=%v err=%s",
			snap.NamespaceBlockSize, snap.NamespaceBlockKnown, snap.NamespaceConfirmedOver, snap.InventoryError)
	}
	if !snap.NamespaceDefaultKnown || snap.NamespaceDefaultBlock != nil || snap.NamespaceNotifyKnown && snap.NamespaceNotificationSize != nil {
		t.Fatalf("successful namespace response retained removed fields: %+v", snap)
	}
	if !snap.Buckets[0].ConfirmedOver || snap.Buckets[0].BlockSize == nil {
		t.Fatalf("bucket failure must keep the cached bucket alarm: %+v", snap.Buckets[0])
	}

	// Namespace failure must not hide a successful bucket quota-off, and must
	// keep the last namespace fields. Re-confirm namespace first.
	src.berr = nil
	src.nsmeta = &ecs.NamespaceMeta{Name: "prod-ns", HasBlock: true, BlockSize: 500, HasNotify: true, NotificationSize: 400, HasDefaultBucketBlock: true, DefaultBucketBlock: 10}
	src.metas["bkt-one"] = ecs.BucketMeta{Name: "bkt-one", HasBlock: true, BlockSize: 100}
	p.PollOnce(context.Background())
	p.PollOnce(context.Background())
	src.nerr = errors.New("namespace down")
	src.metas["bkt-one"] = ecs.BucketMeta{Name: "bkt-one"}
	p.PollOnce(context.Background())
	snap = p.Snapshot()
	if snap.Buckets[0].ConfirmedOver || !snap.Buckets[0].BlockKnown || snap.Buckets[0].BlockSize != nil {
		t.Fatalf("successful bucket quota-off was ignored: %+v", snap.Buckets[0])
	}
	if !snap.NamespaceConfirmedOver || snap.NamespaceBlockSize == nil || *snap.NamespaceBlockSize != 500 {
		t.Fatalf("namespace failure dropped the cached namespace alarm: %+v", snap)
	}
	if snap.NamespaceNotificationSize == nil || snap.NamespaceDefaultBlock == nil {
		t.Fatalf("namespace failure dropped cached notify/default: %+v", snap)
	}

	// Both fail: retain. Both succeed with a new limit: take the new limit.
	src.berr = errors.New("buckets down")
	p.PollOnce(context.Background())
	if !p.Snapshot().NamespaceConfirmedOver || p.Snapshot().Buckets[0].ConfirmedOver {
		t.Fatalf("both-fail poll changed retained state: ns=%v bucket=%+v", p.Snapshot().NamespaceConfirmedOver, p.Snapshot().Buckets[0])
	}
	src.berr, src.nerr = nil, nil
	src.nsmeta = &ecs.NamespaceMeta{Name: "prod-ns", HasBlock: true, BlockSize: 50}
	src.metas["bkt-one"] = ecs.BucketMeta{Name: "bkt-one", HasBlock: true, BlockSize: 10}
	p.PollOnce(context.Background())
	snap = p.Snapshot()
	if snap.NamespaceBlockSize == nil || *snap.NamespaceBlockSize != 50 || snap.NamespaceNotificationSize != nil {
		t.Fatalf("both-success did not take the new namespace observation: %+v", snap)
	}
	if snap.Buckets[0].BlockSize == nil || *snap.Buckets[0].BlockSize != 10 {
		t.Fatalf("both-success did not take the new bucket observation: %+v", snap.Buckets[0])
	}
}
