package poller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/ecs-phoenix-ext/internal/ecs"
	"github.com/fiztoz/ecs-phoenix-ext/internal/store"
)

// scriptedSource replays one payload per poll and clamps at the last.
type scriptedSource struct {
	payloads []*ecs.BillingPayload
	errs     []error
	calls    int
}

func (s *scriptedSource) NamespaceBilling(context.Context, string) (*ecs.BillingPayload, error) {
	i := s.calls
	s.calls++
	if i >= len(s.payloads) {
		i = len(s.payloads) - 1
	}
	if i < len(s.errs) && s.errs[i] != nil {
		return nil, s.errs[i]
	}
	return s.payloads[i], nil
}

// clock is a mutable test clock; activity rates depend on real elapsed time.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func bkt(name string, used, objects, mpuParts int64, uptodate time.Time) ecs.BucketBilling {
	return ecs.BucketBilling{
		Name: name, Namespace: "prod-ns",
		UsedBytes: used, Objects: objects, MPUParts: mpuParts,
		UptodateTill: uptodate,
	}
}

func billingAt(uptodate time.Time, buckets ...ecs.BucketBilling) *ecs.BillingPayload {
	var bytes, objects int64
	for _, b := range buckets {
		bytes += b.UsedBytes
		objects += b.Objects
	}
	return &ecs.BillingPayload{
		Namespace:        "prod-ns",
		NamespaceBytes:   bytes,
		NamespaceObjects: objects,
		UptodateTill:     uptodate,
		Buckets:          buckets,
	}
}

func newActivityPoller(t *testing.T, src BillingSource, st store.Store, c *clock) *Poller {
	t.Helper()
	p := New(context.Background(), src, st, "prod-ns", 15*time.Minute, quietLog())
	p.now = c.now
	return p
}

func activityRows(t *testing.T, st store.Store) []store.ActivityHour {
	t.Helper()
	rows, err := st.ActivityHours(context.Background(), "prod-ns", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func onlyBucket(t *testing.T, p *Poller) BucketState {
	t.Helper()
	snap := p.Snapshot()
	if len(snap.Buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(snap.Buckets))
	}
	return snap.Buckets[0]
}

func TestFirstPollHasNoActivity(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	mem := store.NewMem()
	src := &scriptedSource{payloads: []*ecs.BillingPayload{
		billingAt(c.t.Add(-time.Minute), bkt("bkt", 1024, 100, 0, c.t.Add(-time.Minute))),
	}}
	p := newActivityPoller(t, src, mem, c)
	p.PollOnce(context.Background())

	b := onlyBucket(t, p)
	if b.Activity.Known {
		t.Fatalf("one observation cannot produce a rate: %+v", b.Activity)
	}
	if rows := activityRows(t, mem); len(rows) != 0 {
		t.Fatalf("first poll must not record history: %+v", rows)
	}
}

func TestSecondPollComputesRates(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	mem := store.NewMem()
	src := &scriptedSource{payloads: []*ecs.BillingPayload{
		billingAt(c.t.Add(-time.Minute), bkt("bkt", 1024, 100, 0, c.t.Add(-time.Minute))),
		billingAt(c.t.Add(14*time.Minute), bkt("bkt", 1024+2048, 160, 0, c.t.Add(14*time.Minute))),
	}}
	p := newActivityPoller(t, src, mem, c)
	p.PollOnce(context.Background())

	c.advance(15 * time.Minute)
	p.PollOnce(context.Background())

	a := onlyBucket(t, p).Activity
	if !a.Known || !a.Changed || !a.MeteringRefreshed {
		t.Fatalf("expected a fresh, comparable observation: %+v", a)
	}
	if a.WindowSeconds != 900 {
		t.Errorf("window = %vs, want 900 (one 15m interval)", a.WindowSeconds)
	}
	if a.ObjectsDelta != 60 || a.BytesDelta != 2048 {
		t.Errorf("deltas = %+d obj / %+d B, want +60 / +2048", a.ObjectsDelta, a.BytesDelta)
	}
	// 60 objects in a quarter hour is 240/h.
	if a.ObjectsPerHour != 240 {
		t.Errorf("objects/h = %v, want 240", a.ObjectsPerHour)
	}
	if a.BytesPerHour != float64(2048)*4 {
		t.Errorf("bytes/h = %v, want %v", a.BytesPerHour, float64(2048)*4)
	}
	rows := activityRows(t, mem)
	if len(rows) != 1 {
		t.Fatalf("history rows = %d, want 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Samples != 1 || r.ActiveSamples != 1 {
		t.Errorf("row = %d samples / %d active, want 1 / 1", r.Samples, r.ActiveSamples)
	}
	if r.ObjectsUp != 60 || r.ObjectsDown != 0 || r.BytesUp != 2048 {
		t.Errorf("row deltas = %+v", r)
	}
	if !r.HourBucket.Equal(store.HourBucket(c.t)) {
		t.Errorf("row folded into %v, want the current hour %v", r.HourBucket, store.HourBucket(c.t))
	}
	// One measured hour is not enough evidence to call a bucket always busy.
	if a.BusyHours != 1 || a.ObservedHours != 1 || a.Hot {
		t.Errorf("window = %d busy / %d observed, hot=%v; want 1 / 1 and not hot",
			a.BusyHours, a.ObservedHours, a.Hot)
	}
}

func TestUnrefreshedMeteringIsNotQuietAndHoldsTheBaseline(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	stale := c.t.Add(-time.Minute)
	mem := store.NewMem()
	src := &scriptedSource{payloads: []*ecs.BillingPayload{
		billingAt(stale, bkt("bkt", 1024, 100, 0, stale)),
		// ECS has not republished metering: identical uptodate_till and values.
		billingAt(stale, bkt("bkt", 1024, 100, 0, stale)),
		// Now metering advances, carrying 30 minutes of ingest with it.
		billingAt(c.t.Add(29*time.Minute), bkt("bkt", 1024, 160, 0, c.t.Add(29*time.Minute))),
	}}
	p := newActivityPoller(t, src, mem, c)
	p.PollOnce(context.Background())

	c.advance(15 * time.Minute)
	p.PollOnce(context.Background())
	a := onlyBucket(t, p).Activity
	if !a.Known {
		t.Fatalf("two observations must be comparable: %+v", a)
	}
	if a.Changed || a.MeteringRefreshed {
		t.Fatalf("uptodate_till did not advance, so this is not new metering: %+v", a)
	}
	if rows := activityRows(t, mem); len(rows) != 0 {
		t.Fatalf("an unrefreshed sample must not be folded in as a quiet hour: %+v", rows)
	}

	c.advance(15 * time.Minute)
	p.PollOnce(context.Background())
	a = onlyBucket(t, p).Activity
	if !a.MeteringRefreshed || !a.Changed {
		t.Fatalf("metering advanced: %+v", a)
	}
	// The baseline was held at the first observation, so the 60 objects are
	// rated over the whole 30 minutes rather than charged to the last 15.
	if a.WindowSeconds != 1800 {
		t.Errorf("window = %vs, want 1800", a.WindowSeconds)
	}
	if a.ObjectsPerHour != 120 {
		t.Errorf("objects/h = %v, want 120 (60 objects over 30m)", a.ObjectsPerHour)
	}
	if rows := activityRows(t, mem); len(rows) != 1 || rows[0].ActiveSamples != 1 {
		t.Fatalf("history = %+v, want exactly one active row", rows)
	}
}

func TestFreshButUnchangedMeteringCountsAsObservedNotBusy(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	mem := store.NewMem()
	first := c.t.Add(-time.Minute)
	second := c.t.Add(14 * time.Minute)
	src := &scriptedSource{payloads: []*ecs.BillingPayload{
		billingAt(first, bkt("bkt", 1024, 100, 0, first)),
		billingAt(second, bkt("bkt", 1024, 100, 0, second)), // same values, newer metering
	}}
	p := newActivityPoller(t, src, mem, c)
	p.PollOnce(context.Background())
	c.advance(15 * time.Minute)
	p.PollOnce(context.Background())

	a := onlyBucket(t, p).Activity
	if !a.Known || !a.MeteringRefreshed || a.Changed {
		t.Fatalf("expected fresh metering and no movement: %+v", a)
	}
	if a.ObjectsDelta != 0 || a.BytesDelta != 0 {
		t.Fatalf("deltas = %+d/%+d, want zero", a.ObjectsDelta, a.BytesDelta)
	}
	rows := activityRows(t, mem)
	if len(rows) != 1 || rows[0].Samples != 1 || rows[0].ActiveSamples != 0 {
		t.Fatalf("a measured quiet hour must be recorded as inactive: %+v", rows)
	}
	if a.BusyHours != 0 || a.ObservedHours != 1 {
		t.Fatalf("window = %d busy / %d observed, want 0 / 1", a.BusyHours, a.ObservedHours)
	}
}

func TestInFlightMultipartCountsAsBusy(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	mem := store.NewMem()
	first := c.t.Add(-time.Minute)
	second := c.t.Add(14 * time.Minute)
	src := &scriptedSource{payloads: []*ecs.BillingPayload{
		billingAt(first, bkt("bkt", 1024, 100, 0, first)),
		// A big upload in flight: parts exist, completed counters unchanged.
		billingAt(second, bkt("bkt", 1024, 100, 7, second)),
	}}
	p := newActivityPoller(t, src, mem, c)
	p.PollOnce(context.Background())
	c.advance(15 * time.Minute)
	p.PollOnce(context.Background())

	b := onlyBucket(t, p)
	if b.MPUParts != 7 {
		t.Errorf("bucket state mpu_parts = %d, want 7 (billing reports it, the poller used to drop it)", b.MPUParts)
	}
	a := b.Activity
	if a.MPUParts != 7 || a.MPUPartsDelta != 7 {
		t.Errorf("activity mpu = %d (delta %+d), want 7 (+7)", a.MPUParts, a.MPUPartsDelta)
	}
	rows := activityRows(t, mem)
	if len(rows) != 1 || rows[0].ActiveSamples != 1 || rows[0].MPUPartsMax != 7 {
		t.Fatalf("an in-flight upload must count as movement: %+v", rows)
	}
	if a.BusyHours != 1 {
		t.Errorf("busy hours = %d, want 1", a.BusyHours)
	}
}

func TestDeletionShowsAsNegativeRate(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	mem := store.NewMem()
	first := c.t.Add(-time.Minute)
	second := c.t.Add(14 * time.Minute)
	src := &scriptedSource{payloads: []*ecs.BillingPayload{
		billingAt(first, bkt("bkt", 4096, 100, 0, first)),
		billingAt(second, bkt("bkt", 1024, 40, 0, second)),
	}}
	p := newActivityPoller(t, src, mem, c)
	p.PollOnce(context.Background())
	c.advance(15 * time.Minute)
	p.PollOnce(context.Background())

	a := onlyBucket(t, p).Activity
	if a.ObjectsDelta != -60 || a.BytesDelta != -3072 {
		t.Fatalf("deltas = %+d obj / %+d B, want -60 / -3072", a.ObjectsDelta, a.BytesDelta)
	}
	if a.ObjectsPerHour != -240 {
		t.Errorf("objects/h = %v, want -240 (lifecycle expiry or deletes)", a.ObjectsPerHour)
	}
	rows := activityRows(t, mem)
	if len(rows) != 1 || rows[0].ObjectsDown != 60 || rows[0].ObjectsUp != 0 ||
		rows[0].BytesDown != 3072 || rows[0].BytesUp != 0 {
		t.Fatalf("deletion must land in the down counters, not cancel out: %+v", rows)
	}
}

func TestHotNeedsThreeBusyHours(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	mem := store.NewMem()

	// Nine polls half an hour apart: 10:00 .. 14:00, growth every time.
	var payloads []*ecs.BillingPayload
	for i := 0; i < 9; i++ {
		at := c.t.Add(time.Duration(i) * 30 * time.Minute)
		uptodate := at.Add(-time.Minute)
		payloads = append(payloads, billingAt(uptodate,
			bkt("bkt", 1024*int64(i+1), 100+int64(i)*10, 0, uptodate)))
	}
	src := &scriptedSource{payloads: payloads}
	p := newActivityPoller(t, src, mem, c)

	for i := 0; i < 9; i++ {
		if i > 0 {
			c.advance(30 * time.Minute)
		}
		p.PollOnce(context.Background())
		a := onlyBucket(t, p).Activity
		if a.Hot && a.ObservedHours < 3 {
			t.Fatalf("hot after only %d observed hours: %+v", a.ObservedHours, a)
		}
	}
	a := onlyBucket(t, p).Activity
	// Polls run 10:00..14:00; the first has no baseline, so hours 10-14 each
	// hold at least one measured movement.
	if a.ObservedHours != 5 || a.BusyHours != 5 {
		t.Fatalf("window = %d busy / %d observed, want 5 / 5", a.BusyHours, a.ObservedHours)
	}
	if !a.Hot {
		t.Fatalf("movement in every measured hour must be flagged always busy: %+v", a)
	}
}

func TestQuietBucketNeverBecomesHot(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	mem := store.NewMem()

	var payloads []*ecs.BillingPayload
	for i := 0; i < 9; i++ {
		at := c.t.Add(time.Duration(i) * 30 * time.Minute)
		uptodate := at.Add(-time.Minute)
		payloads = append(payloads, billingAt(uptodate, bkt("bkt", 1024, 100, 0, uptodate)))
	}
	p := newActivityPoller(t, &scriptedSource{payloads: payloads}, mem, c)
	for i := 0; i < 9; i++ {
		if i > 0 {
			c.advance(30 * time.Minute)
		}
		p.PollOnce(context.Background())
	}
	a := onlyBucket(t, p).Activity
	if a.ObservedHours < 3 {
		t.Fatalf("observed hours = %d, want at least 3 measured quiet hours", a.ObservedHours)
	}
	if a.BusyHours != 0 || a.Hot {
		t.Fatalf("a quiet bucket must not be flagged busy: %+v", a)
	}
}

// TestMissingUptodateTillStillMeasuresQuietHours covers the ECS builds that
// omit uptodate_till per bucket, which makes staleness unprovable. Every poll
// must then count as a measured hour, otherwise BusyHours equals
// ObservedHours by construction and any bucket that moves at all gets flagged
// "always busy" while a genuinely quiet one shows no evidence.
func TestMissingUptodateTillStillMeasuresQuietHours(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	mem := store.NewMem()

	// Nine polls half an hour apart, 10:00 .. 14:00. "grower" moves every
	// poll; "sleeper" never moves. Neither carries uptodate_till.
	var payloads []*ecs.BillingPayload
	for i := 0; i < 9; i++ {
		payloads = append(payloads, billingAt(time.Time{},
			bkt("grower", 1024*int64(i+1), 100+int64(i)*10, 0, time.Time{}),
			bkt("sleeper", 4096, 7, 0, time.Time{}),
		))
	}
	p := newActivityPoller(t, &scriptedSource{payloads: payloads}, mem, c)

	var snap Snapshot
	for i := 0; i < 9; i++ {
		if i > 0 {
			c.advance(30 * time.Minute)
		}
		p.PollOnce(context.Background())
		snap = p.Snapshot()
	}

	byName := map[string]BucketState{}
	for _, b := range snap.Buckets {
		byName[b.Name] = b
	}

	grower := byName["grower"].Activity
	if !grower.Known || grower.FreshnessKnown || grower.MeteringRefreshed {
		t.Fatalf("grower must report unknown freshness, not a proven refresh: %+v", grower)
	}
	if !grower.Changed {
		t.Fatalf("grower moved every poll: %+v", grower)
	}
	if grower.ObservedHours != 5 || grower.BusyHours != 5 || !grower.Hot {
		t.Errorf("grower window = %d busy / %d observed, hot=%v; want 5 / 5 and hot",
			grower.BusyHours, grower.ObservedHours, grower.Hot)
	}

	sleeper := byName["sleeper"].Activity
	if !sleeper.Known || sleeper.Changed || sleeper.MeteringRefreshed || sleeper.FreshnessKnown {
		t.Fatalf("sleeper = %+v, want known, unchanged, freshness unknown", sleeper)
	}
	// The whole point: a quiet bucket still accrues measured hours.
	if sleeper.ObservedHours != 5 {
		t.Errorf("sleeper observed hours = %d, want 5 measured hours", sleeper.ObservedHours)
	}
	if sleeper.BusyHours != 0 || sleeper.Hot {
		t.Errorf("sleeper = %d busy, hot=%v; want 0 and not hot", sleeper.BusyHours, sleeper.Hot)
	}

	rows, err := mem.ActivityHours(context.Background(), "prod-ns", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var sleeperSamples, sleeperActive int
	for _, r := range rows {
		if r.Bucket == "sleeper" {
			sleeperSamples += r.Samples
			sleeperActive += r.ActiveSamples
		}
	}
	// Polls 2..9 each produced an observation; none of them saw movement.
	if sleeperSamples != 8 || sleeperActive != 0 {
		t.Errorf("sleeper history = %d samples / %d active, want 8 / 0", sleeperSamples, sleeperActive)
	}
}

func TestActivityBaselineSurvivesRestart(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	mem := store.NewMem()
	first := c.t.Add(-time.Minute)
	src := &scriptedSource{payloads: []*ecs.BillingPayload{
		billingAt(first, bkt("bkt", 1024, 100, 2, first)),
	}}
	p := newActivityPoller(t, src, mem, c)
	p.PollOnce(context.Background())

	// Pod restart: a new Poller seeds the baseline from durable state, so the
	// next poll still diffs against the pre-restart observation.
	c.advance(15 * time.Minute)
	second := c.t.Add(-time.Minute)
	restarted := &scriptedSource{payloads: []*ecs.BillingPayload{
		billingAt(second, bkt("bkt", 4096, 180, 0, second)),
	}}
	p2 := newActivityPoller(t, restarted, mem, c)
	if got := p2.Snapshot().Buckets[0].MPUParts; got != 2 {
		t.Fatalf("restart lost in-flight mpu parts: %d, want 2", got)
	}
	p2.PollOnce(context.Background())

	a := onlyBucket(t, p2).Activity
	if !a.Known || !a.Changed {
		t.Fatalf("restart lost the activity baseline: %+v", a)
	}
	if a.ObjectsDelta != 80 || a.BytesDelta != 3072 {
		t.Fatalf("deltas across restart = %+d obj / %+d B, want +80 / +3072", a.ObjectsDelta, a.BytesDelta)
	}
	if a.WindowSeconds != 900 {
		t.Errorf("window across restart = %vs, want 900", a.WindowSeconds)
	}
	if a.MPUPartsDelta != -2 {
		t.Errorf("mpu delta = %+d, want -2 (the upload completed)", a.MPUPartsDelta)
	}
}

func TestActivityWindowConfigBoundsTheLookback(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	mem := store.NewMem()

	var payloads []*ecs.BillingPayload
	for i := 0; i < 9; i++ {
		at := c.t.Add(time.Duration(i) * 30 * time.Minute)
		uptodate := at.Add(-time.Minute)
		payloads = append(payloads, billingAt(uptodate,
			bkt("bkt", 1024*int64(i+1), 100+int64(i)*10, 0, uptodate)))
	}
	p := newActivityPoller(t, &scriptedSource{payloads: payloads}, mem, c)
	// A one-hour lookback with six hours of retention.
	p.SetActivityWindow(time.Hour, 6*time.Hour)
	if got := p.ActivityWindow(); got != time.Hour {
		t.Fatalf("window = %s, want 1h", got)
	}
	for i := 0; i < 9; i++ {
		if i > 0 {
			c.advance(30 * time.Minute)
		}
		p.PollOnce(context.Background())
	}
	a := onlyBucket(t, p).Activity
	if a.ObservedHours > 2 {
		t.Fatalf("observed hours = %d inside a 1h window, want at most 2 (hour boundaries)", a.ObservedHours)
	}
	if p.Snapshot().ActivityWindow != time.Hour {
		t.Fatalf("snapshot window = %s, want the configured 1h", p.Snapshot().ActivityWindow)
	}
	// Retention, not the window, bounds the table: hours 10-14 are all still
	// stored even though the window only counts two of them.
	if rows := activityRows(t, mem); len(rows) != 5 {
		t.Fatalf("history rows = %d, want the 5 hours polled (10:00-14:00): %+v", len(rows), rows)
	}
}

func TestSubHourActivityWindowFallsBackToDefault(t *testing.T) {
	mem := store.NewMem()
	p := New(context.Background(), &scriptedSource{}, mem, "prod-ns", 15*time.Minute, quietLog())
	p.SetActivityWindow(15*time.Minute, time.Hour)
	if got := p.ActivityWindow(); got != DefaultActivityWindow {
		t.Fatalf("window = %s, want the %s default for a sub-hour request", got, DefaultActivityWindow)
	}
	// Retention below the window is floored to it, so history always covers
	// the lookback the UI claims.
	p.SetActivityWindow(48*time.Hour, time.Hour)
	if got := p.ActivityWindow(); got != 48*time.Hour {
		t.Fatalf("window = %s, want 48h", got)
	}
}

// TestStaleNamespaceMeteringMarkerDoesNotGateActivity pins the behaviour on
// clusters that publish a namespace uptodate_till far behind wall clock while
// per-bucket counters keep moving — observed in production as a marker ~2 days
// old with counters changing between polls 30 minutes apart. The marker is
// display only. If it gated recording, no activity would ever be stored on such
// a cluster, and every bucket would show "no new metering" forever.
func TestStaleNamespaceMeteringMarkerDoesNotGateActivity(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)}
	marker := time.Date(2026, 10, 14, 4, 13, 30, 0, time.UTC) // ~2.3 days behind the clock
	mem := store.NewMem()
	src := &scriptedSource{payloads: []*ecs.BillingPayload{
		billingAt(marker, bkt("bkt", 1024, 100, 0, time.Time{})),
		billingAt(marker, bkt("bkt", 2048, 160, 0, time.Time{})),
	}}
	p := newActivityPoller(t, src, mem, c)
	p.PollOnce(context.Background())
	if got := p.Snapshot().NamespaceUptodateTill; !got.Equal(marker) {
		t.Fatalf("namespace marker = %v, want %v carried into the snapshot", got, marker)
	}

	c.advance(15 * time.Minute)
	p.PollOnce(context.Background())

	snap := p.Snapshot()
	if !snap.NamespaceUptodateTill.Equal(marker) {
		t.Fatalf("marker not carried on the second poll: %v", snap.NamespaceUptodateTill)
	}
	a := snap.Buckets[0].Activity
	if !a.Known || !a.Changed || a.FreshnessKnown || a.MeteringRefreshed {
		t.Fatalf("activity = %+v, want a changed observation with unknown per-bucket freshness", a)
	}
	rows := activityRows(t, mem)
	if len(rows) != 1 || rows[0].ActiveSamples != 1 {
		t.Fatalf("a stale namespace marker must not suppress recording: %+v", rows)
	}
	if a.ObservedHours != 1 || a.BusyHours != 1 {
		t.Errorf("window = %d busy / %d observed, want 1 / 1", a.BusyHours, a.ObservedHours)
	}
	if snap.Buckets[0].Stale {
		t.Error("per-bucket staleness must not be inferred from the namespace marker")
	}
}

func TestFailedPollRecordsNoActivity(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	mem := store.NewMem()
	first := c.t.Add(-time.Minute)
	src := &scriptedSource{
		payloads: []*ecs.BillingPayload{
			billingAt(first, bkt("bkt", 1024, 100, 0, first)),
			nil,
		},
		errs: []error{nil, errors.New("ecs: billing returned HTTP 503")},
	}
	p := newActivityPoller(t, src, mem, c)
	p.PollOnce(context.Background())
	c.advance(15 * time.Minute)
	p.PollOnce(context.Background())

	snap := p.Snapshot()
	if snap.PollOK {
		t.Fatal("setup: poll should have failed")
	}
	if rows := activityRows(t, mem); len(rows) != 0 {
		t.Fatalf("a failed poll must not write history: %+v", rows)
	}
}

// brokenActivityStore fails only the derived-activity calls. Activity is
// best-effort: a store outage there must never flip PollOK or lose buckets.
type brokenActivityStore struct {
	store.Store
	err error
}

func (b brokenActivityStore) RecordActivity(context.Context, []store.ActivityDelta) error {
	return b.err
}

func (b brokenActivityStore) ActivityHours(context.Context, string, time.Time) ([]store.ActivityHour, error) {
	return nil, b.err
}

func (b brokenActivityStore) PruneActivity(context.Context, time.Time) (int64, error) {
	return 0, b.err
}

func TestActivityStoreFailureKeepsBillingTruth(t *testing.T) {
	c := &clock{t: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	st := brokenActivityStore{Store: store.NewMem(), err: errors.New("store: activity table is locked")}
	first := c.t.Add(-time.Minute)
	second := c.t.Add(14 * time.Minute)
	src := &scriptedSource{payloads: []*ecs.BillingPayload{
		billingAt(first, bkt("bkt", 1024, 100, 0, first)),
		billingAt(second, bkt("bkt", 4096, 160, 0, second)),
	}}
	p := newActivityPoller(t, src, st, c)
	p.PollOnce(context.Background())
	c.advance(15 * time.Minute)
	p.PollOnce(context.Background())

	snap := p.Snapshot()
	if !snap.PollOK || len(snap.Buckets) != 1 {
		t.Fatalf("activity store failure broke billing truth: %+v", snap)
	}
	a := snap.Buckets[0].Activity
	if !a.Known || a.ObjectsPerHour != 240 {
		t.Fatalf("the in-poll rate must still be computed: %+v", a)
	}
	if a.BusyHours != 0 || a.ObservedHours != 0 || a.Hot {
		t.Fatalf("window counters must stay empty, not invent evidence: %+v", a)
	}
}
