package http

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

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
		data.Buckets = append(data.Buckets, toBucketView(b))
	}

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
