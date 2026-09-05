package poller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/fiztoz/ecs-phoenix-ext/internal/ecs"
	"github.com/fiztoz/ecs-phoenix-ext/internal/store"
)

// fakeSource is a scripted BillingSource.
type fakeSource struct {
	calls   int
	results []pollResult
}

type pollResult struct {
	payload *ecs.BillingPayload
	err     error
}

func (f *fakeSource) NamespaceBilling(context.Context, string) (*ecs.BillingPayload, error) {
	i := f.calls
	f.calls++
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	return f.results[i].payload, f.results[i].err
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func payload(usedBytes int64, uptodateAgo time.Duration, now time.Time) *ecs.BillingPayload {
	ut := now.Add(-uptodateAgo).UTC()
	return &ecs.BillingPayload{
		Namespace:        "prod-ns",
		NamespaceBytes:   usedBytes,
		NamespaceObjects: 1,
		UptodateTill:     ut,
		Buckets: []ecs.BucketBilling{{
			Name:         "bkt-one",
			Namespace:    "prod-ns",
			UsedBytes:    usedBytes,
			Objects:      1,
			UptodateTill: ut,
		}},
	}
}
func newTestPoller(t *testing.T, src BillingSource, mem *store.Mem, now time.Time) *Poller {
	t.Helper()
	p := New(context.Background(), src, mem, "prod-ns", 15*time.Minute, quietLog())
	p.now = func() time.Time { return now }
	return p
}

func TestFailedPollKeepsBuckets(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	src := &fakeSource{results: []pollResult{
		{payload: payload(100, 0, now)},
		{err: errors.New("ecs: login returned HTTP 503")},
	}}
	mem := store.NewMem()
	p := newTestPoller(t, src, mem, now)

	p.PollOnce(context.Background())
	if snap := p.Snapshot(); !snap.PollOK || len(snap.Buckets) != 1 {
		t.Fatalf("after good poll: %+v", snap)
	}

	p.PollOnce(context.Background())
	snap := p.Snapshot()
	if snap.PollOK {
		t.Fatal("poll_ok must be false after failure")
	}
	if len(snap.Buckets) != 1 || snap.Buckets[0].UsedBytes != 100 {
		t.Fatalf("failed poll wiped buckets: %+v", snap.Buckets)
	}
	if snap.LastError == "" {
		t.Fatal("last_error must be set")
	}
}

func TestStaleFlag(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	src := &fakeSource{results: []pollResult{
		{payload: payload(100, 2*time.Hour, now)}, // older than max(3*15m,1h)=1h
	}}
	mem := store.NewMem()
	p := newTestPoller(t, src, mem, now)
	p.PollOnce(context.Background())
	if b := p.Snapshot().Buckets[0]; !b.Stale {
		t.Fatalf("uptodate_till 2h old must be stale: %+v", b)
	}

	src2 := &fakeSource{results: []pollResult{
		{payload: payload(100, 5*time.Minute, now)},
	}}
	p2 := newTestPoller(t, src2, mem, now)
	p2.PollOnce(context.Background())
	if b := p2.Snapshot().Buckets[0]; b.Stale {
		t.Fatalf("fresh uptodate_till must not be stale: %+v", b)
	}
}

func TestStaleThresholdFloor(t *testing.T) {
	mem := store.NewMem()
	p := New(context.Background(), &fakeSource{}, mem, "ns", 10*time.Minute, quietLog())
	if got := p.StaleThreshold(); got != time.Hour {
		t.Fatalf("threshold = %s, want floor of 1h", got)
	}
	p2 := New(context.Background(), &fakeSource{}, mem, "ns", time.Hour, quietLog())
	if got := p2.StaleThreshold(); got != 3*time.Hour {
		t.Fatalf("threshold = %s, want 3h", got)
	}
}

func TestHysteresisTwoSamples(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	mem := store.NewMem()
	// ECS Block Access threshold of 100 acts as the quota.
	src := &fullSource{
		fakeSource: fakeSource{results: []pollResult{
			{payload: payload(200, 0, now)}, // over, sample 1
			{payload: payload(200, 0, now)}, // over, sample 2 → confirmed
			{payload: payload(50, 0, now)},  // under → reset
		}},
		metas: map[string]ecs.BucketMeta{
			"bkt-one": {Name: "bkt-one", BlockSize: 100, HasBlock: true},
		},
		nsmeta: &ecs.NamespaceMeta{Name: "prod-ns"},
	}
	p := newTestPoller(t, src, mem, now)

	p.PollOnce(context.Background())
	b := p.Snapshot().Buckets[0]
	if b.OverStreak != 1 || b.ConfirmedOver {
		t.Fatalf("one over sample must not confirm: %+v", b)
	}

	p.PollOnce(context.Background())
	b = p.Snapshot().Buckets[0]
	if b.OverStreak != 2 || !b.ConfirmedOver {
		t.Fatalf("two over samples must confirm: %+v", b)
	}
	rows, _ := mem.States(context.Background())
	if len(rows) != 1 || !rows[0].ConfirmedOver {
		t.Fatalf("confirmed state must be persisted: %+v", rows)
	}

	p.PollOnce(context.Background())
	b = p.Snapshot().Buckets[0]
	if b.OverStreak != 0 || b.ConfirmedOver {
		t.Fatalf("under-quota sample must reset streak: %+v", b)
	}
}

func TestConfirmedAlarmSurvivesRestart(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	mem := store.NewMem()
	src := &fullSource{
		fakeSource: fakeSource{results: []pollResult{
			{payload: payload(200, 0, now)},
			{payload: payload(200, 0, now)},
		}},
		metas: map[string]ecs.BucketMeta{
			"bkt-one": {Name: "bkt-one", BlockSize: 100, HasBlock: true},
		},
		nsmeta: &ecs.NamespaceMeta{Name: "prod-ns"},
	}
	p := newTestPoller(t, src, mem, now)
	p.PollOnce(context.Background())
	p.PollOnce(context.Background())
	if !p.Snapshot().Buckets[0].ConfirmedOver {
		t.Fatal("setup: expected confirmed over")
	}

	// Simulated pod restart: a brand new Poller seeds from durable state.
	restarted := New(context.Background(), &fakeSource{}, mem, "prod-ns", 15*time.Minute, quietLog())
	snap := restarted.Snapshot()
	if len(snap.Buckets) != 1 {
		t.Fatalf("restart lost buckets: %+v", snap)
	}
	if !snap.Buckets[0].ConfirmedOver || snap.Buckets[0].OverStreak != 2 {
		t.Fatalf("restart must not reset a confirmed alarm: %+v", snap.Buckets[0])
	}
}

func TestNoQuotaNeverConfirmed(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	src := &fakeSource{results: []pollResult{
		{payload: payload(999999999, 0, now)},
		{payload: payload(999999999, 0, now)},
		{payload: payload(999999999, 0, now)},
	}}
	p := newTestPoller(t, src, store.NewMem(), now)
	for i := 0; i < 3; i++ {
		p.PollOnce(context.Background())
		if b := p.Snapshot().Buckets[0]; b.ConfirmedOver || b.OverStreak != 0 {
			t.Fatalf("bucket without quota can never be over: %+v", b)
		}
	}
}

func TestRefreshQuotasLeavesECSBucketQuotasAlone(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	mem := store.NewMem()
	src := &fullSource{
		fakeSource: fakeSource{results: []pollResult{
			{payload: payload(100, 0, now)},
		}},
		metas: map[string]ecs.BucketMeta{
			"bkt-one": {Name: "bkt-one", BlockSize: 50, HasBlock: true},
		},
		nsmeta: &ecs.NamespaceMeta{Name: "prod-ns"},
	}
	p := newTestPoller(t, src, mem, now)
	p.PollOnce(context.Background())
	b := p.Snapshot().Buckets[0]
	if b.QuotaBytes == nil || *b.QuotaBytes != 50 {
		t.Fatalf("ECS block threshold must be the quota: %+v", b)
	}

	// A namespace quota set becomes visible without a new poll, while the
	// ECS bucket quota fields are untouched.
	if err := mem.SetNamespaceQuota(context.Background(), "prod-ns", 1000); err != nil {
		t.Fatal(err)
	}
	p.RefreshQuotas(context.Background())
	snap := p.Snapshot()
	if snap.NamespaceQuotaBytes == nil || *snap.NamespaceQuotaBytes != 1000 {
		t.Fatalf("namespace quota not visible after refresh: %+v", snap)
	}
	b = snap.Buckets[0]
	if b.QuotaBytes == nil || *b.QuotaBytes != 50 || b.OverStreak != 1 || b.ConfirmedOver {
		t.Fatalf("bucket ECS quota must be untouched by refresh: %+v", b)
	}
}

func TestRefreshQuotasBeforeFirstPollIsNoop(t *testing.T) {
	mem := store.NewMem()
	if err := mem.SetNamespaceQuota(context.Background(), "prod-ns", 50); err != nil {
		t.Fatal(err)
	}
	p := New(context.Background(), &fakeSource{}, mem, "prod-ns", 15*time.Minute, quietLog())
	p.RefreshQuotas(context.Background()) // must not panic or seed buckets
	if snap := p.Snapshot(); len(snap.Buckets) != 0 {
		t.Fatalf("refresh before poll must not add buckets: %+v", snap)
	}
}

func TestTimestampsAreUTC(t *testing.T) {
	// Feed a +07:00 zoned timestamp; bound timestamps must come out UTC.
	loc := time.FixedZone("ICT", 7*3600)
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	ut := time.Date(2026, 8, 18, 16, 50, 0, 0, loc)
	src := &fakeSource{results: []pollResult{{payload: &ecs.BillingPayload{
		Namespace:    "prod-ns",
		UptodateTill: ut,
		Buckets: []ecs.BucketBilling{{
			Name: "bkt-one", Namespace: "prod-ns", UsedBytes: 1,
			UptodateTill: ut,
		}},
	}}}}
	p := newTestPoller(t, src, store.NewMem(), now)
	p.PollOnce(context.Background())

	b := p.Snapshot().Buckets[0]
	if b.UptodateTill.Location() != time.UTC {
		t.Fatalf("bound timestamp must be UTC: %v", b.UptodateTill.Location())
	}
	if !b.UptodateTill.Equal(ut) {
		t.Fatalf("uptodate instant shifted: %v vs %v", b.UptodateTill, ut)
	}
	rows, _ := p.store.States(context.Background())
	if len(rows) != 1 {
		t.Fatalf("states = %d", len(rows))
	}
	if rows[0].UptodateTill == nil || rows[0].UptodateTill.Location() != time.UTC || rows[0].PolledAt.Location() != time.UTC {
		t.Fatalf("persisted timestamps must be UTC")
	}
}

func TestNamespaceQuotaHysteresis(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	mem := store.NewMem()
	if err := mem.SetNamespaceQuota(context.Background(), "prod-ns", 500); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{results: []pollResult{
		{payload: payload(600, 0, now)}, // over, sample 1
		{payload: payload(600, 0, now)}, // over, sample 2 → confirmed
		{payload: payload(200, 0, now)}, // under → reset
	}}
	p := newTestPoller(t, src, mem, now)

	p.PollOnce(context.Background())
	snap := p.Snapshot()
	if snap.NamespaceQuotaBytes == nil || *snap.NamespaceQuotaBytes != 500 {
		t.Fatalf("namespace quota not set: %+v", snap)
	}
	if snap.NamespaceOverStreak != 1 || snap.NamespaceConfirmedOver {
		t.Fatalf("one over sample must not confirm: %+v", snap)
	}
	if !snap.NamespaceAtQuota {
		t.Fatal("namespace at quota must be true")
	}

	p.PollOnce(context.Background())
	snap = p.Snapshot()
	if snap.NamespaceOverStreak != 2 || !snap.NamespaceConfirmedOver {
		t.Fatalf("two over samples must confirm: %+v", snap)
	}

	p.PollOnce(context.Background())
	snap = p.Snapshot()
	if snap.NamespaceOverStreak != 0 || snap.NamespaceConfirmedOver {
		t.Fatalf("under-quota sample must reset streak: %+v", snap)
	}
}

func TestNamespaceQuotaRefresh(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	mem := store.NewMem()
	src := &fakeSource{results: []pollResult{
		{payload: payload(100, 0, now)},
	}}
	p := newTestPoller(t, src, mem, now)
	p.PollOnce(context.Background())

	// No namespace quota initially.
	if snap := p.Snapshot(); snap.NamespaceQuotaBytes != nil {
		t.Fatalf("expected no namespace quota: %+v", snap)
	}

	// Set namespace quota below used: visible immediately.
	if err := mem.SetNamespaceQuota(context.Background(), "prod-ns", 50); err != nil {
		t.Fatal(err)
	}
	p.RefreshQuotas(context.Background())
	snap := p.Snapshot()
	if snap.NamespaceQuotaBytes == nil || *snap.NamespaceQuotaBytes != 50 {
		t.Fatalf("namespace quota not visible after refresh: %+v", snap)
	}
	if snap.NamespaceUsedPercent == nil || *snap.NamespaceUsedPercent != 200 {
		t.Fatalf("namespace percent = %v, want 200%%", snap.NamespaceUsedPercent)
	}
	if snap.NamespaceOverStreak != 1 {
		t.Fatalf("namespace over streak = %d, want 1", snap.NamespaceOverStreak)
	}

	// Delete namespace quota: clears immediately.
	if err := mem.DeleteNamespaceQuota(context.Background(), "prod-ns"); err != nil {
		t.Fatal(err)
	}
	p.RefreshQuotas(context.Background())
	snap = p.Snapshot()
	if snap.NamespaceQuotaBytes != nil || snap.NamespaceUsedPercent != nil || snap.NamespaceConfirmedOver {
		t.Fatalf("namespace quota delete not reflected: %+v", snap)
	}
}

// fullSource is a BillingSource plus the optional inventory overlay.
type fullSource struct {
	fakeSource
	metas  map[string]ecs.BucketMeta
	nsmeta *ecs.NamespaceMeta
	berr   error
	nerr   error
}

func (f *fullSource) BucketMetas(context.Context, string) (map[string]ecs.BucketMeta, error) {
	return f.metas, f.berr
}

func (f *fullSource) NamespaceMetaInfo(context.Context, string) (*ecs.NamespaceMeta, error) {
	return f.nsmeta, f.nerr
}

func TestInventoryEnrichMergesBlockNotify(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	src := &fullSource{
		fakeSource: fakeSource{results: []pollResult{{payload: payload(100, 0, now)}}},
		metas: map[string]ecs.BucketMeta{
			"bkt-one": {Name: "bkt-one", BlockSize: 134217728, NotificationSize: 1024, HasBlock: true, HasNotify: true},
		},
		nsmeta: &ecs.NamespaceMeta{Name: "prod-ns", DefaultBucketBlock: 134217728, HasDefaultBucketBlock: true},
	}
	mem := store.NewMem()
	p := New(context.Background(), src, mem, "prod-ns", 15*time.Minute, quietLog())
	p.now = func() time.Time { return now }
	p.PollOnce(context.Background())

	snap := p.Snapshot()
	if !snap.InventoryOK || snap.InventoryError != "" {
		t.Fatalf("inventory should succeed: %+v", snap)
	}
	if snap.NamespaceDefaultBlock == nil || *snap.NamespaceDefaultBlock != 134217728 {
		t.Fatalf("namespace default block missing: %+v", snap)
	}
	b := snap.Buckets[0]
	if b.BlockSize == nil || *b.BlockSize != 134217728 {
		t.Fatalf("block not merged: %+v", b)
	}
	if b.NotificationSize == nil || *b.NotificationSize != 1024 {
		t.Fatalf("notify not merged: %+v", b)
	}
}

func TestInventoryFailureKeepsBilling(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	src := &fullSource{
		fakeSource: fakeSource{results: []pollResult{{payload: payload(100, 0, now)}}},
		berr:       errors.New("ecs: bucket list returned HTTP 503"),
		nerr:       errors.New("ecs: namespace info returned HTTP 503"),
	}
	mem := store.NewMem()
	p := New(context.Background(), src, mem, "prod-ns", 15*time.Minute, quietLog())
	p.now = func() time.Time { return now }
	p.PollOnce(context.Background())

	snap := p.Snapshot()
	if !snap.PollOK || len(snap.Buckets) != 1 {
		t.Fatalf("billing must survive inventory failure: %+v", snap)
	}
	if snap.InventoryOK || snap.InventoryError == "" {
		t.Fatalf("inventory error must be recorded: %+v", snap)
	}
}

func TestInventoryNamespaceThresholds(t *testing.T) {
	now := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	src := &fullSource{
		fakeSource: fakeSource{results: []pollResult{{payload: payload(100, 0, now)}}},
		metas:      map[string]ecs.BucketMeta{},
		nsmeta: &ecs.NamespaceMeta{
			Name:                  "prod-ns",
			DefaultBucketBlock:    134217728,
			HasDefaultBucketBlock: true,
			BlockSize:             10737418240,
			HasBlock:              true,
			NotificationSize:      8589934592,
			HasNotify:             true,
		},
	}
	mem := store.NewMem()
	p := New(context.Background(), src, mem, "prod-ns", 15*time.Minute, quietLog())
	p.now = func() time.Time { return now }
	p.PollOnce(context.Background())

	snap := p.Snapshot()
	if !snap.InventoryOK {
		t.Fatalf("inventory should succeed: %+v", snap)
	}
	if snap.NamespaceDefaultBlock == nil || *snap.NamespaceDefaultBlock != 134217728 {
		t.Fatalf("namespace default block missing: %+v", snap)
	}
	if snap.NamespaceBlockSize == nil || *snap.NamespaceBlockSize != 10737418240 {
		t.Fatalf("namespace block missing: %+v", snap)
	}
	if snap.NamespaceNotificationSize == nil || *snap.NamespaceNotificationSize != 8589934592 {
		t.Fatalf("namespace notify missing: %+v", snap)
	}
}
