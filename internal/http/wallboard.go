package http

import (
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// busiestEntry is one row of the wallboard's busiest strip. Like every
// activity number here it is derived from billing deltas: write-side movement
// only, never a measured request rate.
type busiestEntry struct {
	Name        string
	ObjectsRate string
	BytesRate   string
	Busy        string
	Hot         bool
	HasMPU      bool
	MPUParts    int64
}

type wallboardData struct {
	BasePath               string
	Namespace              string
	Now                    time.Time
	PolledAt               time.Time
	PollOK                 bool
	LastError              string
	NamespaceUsed          string
	NamespaceUsedBytes     int64
	NamespaceObjects       int64
	NamespaceHasQuota      bool
	NamespaceQuota         string
	NamespaceQuotaBytes    int64
	NamespacePercent       string
	NamespaceBarWidth      int
	NamespaceAtQuota       bool
	NamespaceConfirmedOver bool
	NamespaceDefaultBlock  string
	HasDefaultBlock        bool
	NamespaceBlock         string
	HasNamespaceBlock      bool
	NamespaceNotify        string
	HasNamespaceNotify     bool
	Buckets                []bucketView
	// ActivityWindow labels the busy counters, e.g. "24h".
	ActivityWindow string
	// HotBuckets counts buckets with movement in most measured hours.
	HotBuckets int
	// Busiest is the top-N ranking for the wallboard strip.
	Busiest []busiestEntry
	// NamespaceMeteringAsOf is ECS's own uptodate_till for the namespace. It is
	// shown because it can lag the poll time by days, which changes how the
	// usage numbers should be read.
	NamespaceMeteringAsOf time.Time
	HasNamespaceMetering  bool
}

func (s *Server) handleWallboard(w http.ResponseWriter, r *http.Request) {
	snap := s.deps.Snapshots.Snapshot()
	now := time.Now().UTC()
	data := wallboardData{
		BasePath:           strings.TrimRight(s.deps.BasePath, "/"),
		Namespace:          snap.Namespace,
		Now:                now,
		PolledAt:           snap.PolledAt,
		PollOK:             snap.PollOK,
		LastError:          snap.LastError,
		NamespaceUsed:      HumanBytes(snap.NamespaceBytes),
		NamespaceUsedBytes: snap.NamespaceBytes,
		NamespaceObjects:   snap.NamespaceObjects,
		ActivityWindow:     shortDuration(snap.ActivityWindow),
	}
	if !snap.NamespaceUptodateTill.IsZero() {
		data.HasNamespaceMetering = true
		data.NamespaceMeteringAsOf = snap.NamespaceUptodateTill.UTC()
	}

	// Namespace-level quota.
	if snap.NamespaceQuotaBytes != nil {
		data.NamespaceHasQuota = true
		data.NamespaceQuotaBytes = *snap.NamespaceQuotaBytes
		data.NamespaceQuota = HumanBytes(*snap.NamespaceQuotaBytes)
		data.NamespaceAtQuota = snap.NamespaceAtQuota
		data.NamespaceConfirmedOver = snap.NamespaceConfirmedOver
		if snap.NamespaceUsedPercent != nil {
			data.NamespacePercent = pctStr(*snap.NamespaceUsedPercent)
			data.NamespaceBarWidth = barWidth(snap.NamespaceUsedPercent)
		}
	}
	if snap.NamespaceDefaultBlock != nil {
		data.HasDefaultBlock = true
		data.NamespaceDefaultBlock = HumanBytes(*snap.NamespaceDefaultBlock)
	}
	if snap.NamespaceBlockSize != nil {
		data.HasNamespaceBlock = true
		data.NamespaceBlock = HumanBytes(*snap.NamespaceBlockSize)
	}
	if snap.NamespaceNotificationSize != nil {
		data.HasNamespaceNotify = true
		data.NamespaceNotify = HumanBytes(*snap.NamespaceNotificationSize)
	}

	for _, b := range snap.Buckets {
		data.Buckets = append(data.Buckets, toBucketView(b, snap.ActivityWindow))
		if b.Activity.Hot {
			data.HotBuckets++
		}
	}
	data.Busiest = busiestBuckets(data.Buckets, 5)

	// Severity first, like the uptime-phoenix wallboard: over-quota cards
	// before yellow-80% before at-quota before stale before ok; name order within each tier.
	sort.SliceStable(data.Buckets, func(i, j int) bool {
		si, sj := severityRank(data.Buckets[i]), severityRank(data.Buckets[j])
		if si != sj {
			return si < sj
		}
		return data.Buckets[i].Name < data.Buckets[j].Name
	})

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "wallboard.html", data); err != nil {
		s.deps.Log.Error("wallboard render", "err", err)
	}
}

func severityRank(b bucketView) int {
	switch {
	case b.ConfirmedOver:
		return 0 // over quota - highest priority
	case b.BarWidth >= 80 || b.NotifyReached:
		return 1 // yellow warning: 80% of block or ECS notify threshold
	case b.AtQuota:
		return 2 // at quota
	case b.Stale:
		return 3 // stale data
	default:
		return 4 // ok/green UP
	}
}

// busiestBuckets ranks buckets by "always used" first — measured hours with
// movement — then by ingest rate, so a bucket that is steadily busy outranks
// one that spiked once. Buckets with no measured activity at all are left out
// rather than shown as zero.
func busiestBuckets(buckets []bucketView, limit int) []busiestEntry {
	ranked := make([]bucketView, 0, len(buckets))
	for _, b := range buckets {
		if b.Activity.BusyScore >= 0 || b.Activity.Score >= 0 {
			ranked = append(ranked, b)
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		a, c := ranked[i].Activity, ranked[j].Activity
		if a.BusyHours != c.BusyHours {
			return a.BusyHours > c.BusyHours
		}
		if a.Score != c.Score {
			return a.Score > c.Score
		}
		if math.Abs(a.BytesPerHour) != math.Abs(c.BytesPerHour) {
			return math.Abs(a.BytesPerHour) > math.Abs(c.BytesPerHour)
		}
		return ranked[i].Name < ranked[j].Name
	})
	out := make([]busiestEntry, 0, limit)
	for _, b := range ranked[:min(limit, len(ranked))] {
		out = append(out, busiestEntry{
			Name:        b.Name,
			ObjectsRate: b.Activity.ObjectsRate,
			BytesRate:   b.Activity.BytesRate,
			Busy:        b.Activity.Busy,
			Hot:         b.Activity.Hot,
			HasMPU:      b.Activity.HasMPU,
			MPUParts:    b.Activity.MPUParts,
		})
	}
	return out
}
