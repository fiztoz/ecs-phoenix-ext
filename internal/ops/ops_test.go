package ops

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/ecs-phoenix-ext/internal/poller"
	"github.com/fiztoz/ecs-phoenix-ext/internal/store"
)

type fakeSnap struct{ snap poller.Snapshot }

func (f fakeSnap) Snapshot() poller.Snapshot     { return f.snap }
func (f fakeSnap) StaleThreshold() time.Duration { return time.Hour }

var testNow = time.Date(2026, 11, 1, 12, 30, 0, 0, time.UTC)

func testService(t *testing.T, st store.Store) *Service {
	t.Helper()
	pct := func(v float64) *float64 { return &v }
	snap := poller.Snapshot{
		Namespace: "prod-ns", PollOK: true, PolledAt: testNow,
		Buckets: []poller.BucketState{
			{Name: "alpha", Namespace: "prod-ns", UsedBytes: 100, Objects: 9, UsedPercent: pct(10)},
			{Name: "beta", Namespace: "prod-ns", UsedBytes: 300, Objects: 1, AtQuota: true, UsedPercent: pct(100),
				Activity: poller.Activity{Known: true, Changed: true, ObjectsPerHour: -50, Hot: true}},
			{Name: "gamma", Namespace: "prod-ns", UsedBytes: 200, Objects: 5,
				Activity: poller.Activity{Known: true, Changed: true, ObjectsPerHour: 20}},
			{Name: "app-logs", Namespace: "prod-ns", UsedBytes: 200, Objects: 5},
		},
	}
	if st == nil {
		st = store.NewMem()
	}
	return &Service{DefaultNamespace: "prod-ns", Snapshots: fakeSnap{snap}, Store: st, Now: func() time.Time { return testNow }}
}

func names(l BucketList) []string {
	out := make([]string, 0, len(l.Buckets))
	for _, b := range l.Buckets {
		out = append(out, b.Name)
	}
	return out
}

func TestListBucketsSortAndFilter(t *testing.T) {
	s := testService(t, nil)
	cases := []struct {
		name string
		opts ListOptions
		want []string
	}{
		{"default name", ListOptions{}, []string{"alpha", "app-logs", "beta", "gamma"}},
		// app-logs and gamma tie on bytes: name breaks the tie.
		{"used desc", ListOptions{Sort: SortUsed}, []string{"beta", "app-logs", "gamma", "alpha"}},
		{"objects desc", ListOptions{Sort: SortObjects}, []string{"alpha", "app-logs", "gamma", "beta"}},
		// Unknown percent sorts last.
		{"percent", ListOptions{Sort: SortUsedPercent}, []string{"beta", "alpha", "app-logs", "gamma"}},
		// |rate| desc; no baseline last.
		{"activity", ListOptions{Sort: SortActivity}, []string{"beta", "gamma", "alpha", "app-logs"}},
		{"prefix", ListOptions{Prefix: "a"}, []string{"alpha", "app-logs"}},
		{"at quota", ListOptions{AtQuotaOnly: true}, []string{"beta"}},
		{"hot", ListOptions{HotOnly: true}, []string{"beta"}},
	}
	for _, tc := range cases {
		got, err := s.ListBuckets(tc.opts)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if g := names(got); len(g) != len(tc.want) || func() bool {
			for i := range g {
				if g[i] != tc.want[i] {
					return true
				}
			}
			return false
		}() {
			t.Errorf("%s: got %v, want %v", tc.name, g, tc.want)
		}
	}
}

func TestListBucketsLimitReportsTruncation(t *testing.T) {
	got, err := testService(t, nil).ListBuckets(ListOptions{Sort: SortUsed, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 4 || !got.Truncated || len(got.Buckets) != 2 {
		t.Fatalf("total=%d truncated=%v len=%d", got.Total, got.Truncated, len(got.Buckets))
	}
}

func TestListBucketsBadSort(t *testing.T) {
	if _, err := testService(t, nil).ListBuckets(ListOptions{Sort: "size"}); !errors.Is(err, ErrBadSort) {
		t.Fatalf("err = %v, want ErrBadSort", err)
	}
}

func TestNamespaceRollups(t *testing.T) {
	n := testService(t, nil).Namespace()
	if n.BucketCount != 4 || n.AtQuotaBuckets != 1 || n.HotBuckets != 1 {
		t.Fatalf("summary = %+v", n)
	}
}

func TestActivityHistory(t *testing.T) {
	st := store.NewMem()
	ctx := context.Background()
	hour := testNow.Truncate(time.Hour)
	err := st.RecordActivity(ctx, []store.ActivityDelta{
		{Namespace: "prod-ns", Bucket: "beta", HourBucket: hour.Add(-2 * time.Hour), ObjectsDown: 50, Active: true},
		{Namespace: "prod-ns", Bucket: "beta", HourBucket: hour.Add(-30 * time.Hour), ObjectsUp: 1, Active: true},
		{Namespace: "prod-ns", Bucket: "gamma", HourBucket: hour, ObjectsUp: 5, Active: true},
		// A bucket deleted since: history only.
		{Namespace: "prod-ns", Bucket: "gone", HourBucket: hour, BytesDown: 9, Active: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := testService(t, st)

	h, err := s.ActivityHistory(ctx, "beta", 24)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Hours) != 1 || h.Hours[0].ObjectsDown != 50 || h.Note == "" {
		t.Fatalf("beta history = %+v", h)
	}
	if h.Since != hour.Add(-23*time.Hour) {
		t.Errorf("since = %s", h.Since)
	}
	if h, err := s.ActivityHistory(ctx, "alpha", 24); err != nil || len(h.Hours) != 0 {
		t.Errorf("alpha: %+v, %v; want empty history for a known bucket", h, err)
	}
	if h, err := s.ActivityHistory(ctx, "gone", 24); err != nil || len(h.Hours) != 1 {
		t.Errorf("gone: %+v, %v; want its history", h, err)
	}
	if _, err := s.ActivityHistory(ctx, "nope", 24); !errors.Is(err, ErrNotFound) {
		t.Errorf("nope: err = %v, want ErrNotFound", err)
	}
}
