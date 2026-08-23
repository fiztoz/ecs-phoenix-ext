package http

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

type wallboardData struct {
	BasePath         string
	Namespace        string
	Now              time.Time
	PolledAt         time.Time
	PollOK           bool
	LastError        string
	NamespaceUsed    string
	NamespaceObjects int64
	Buckets          []bucketView
}

func (s *Server) handleWallboard(w http.ResponseWriter, r *http.Request) {
	snap := s.deps.Snapshots.Snapshot()
	now := time.Now().UTC()
	data := wallboardData{
		BasePath:         strings.TrimRight(s.deps.BasePath, "/"),
		Namespace:        snap.Namespace,
		Now:              now,
		PolledAt:         snap.PolledAt,
		PollOK:           snap.PollOK,
		LastError:        snap.LastError,
		NamespaceUsed:    HumanBytes(snap.NamespaceBytes),
		NamespaceObjects: snap.NamespaceObjects,
	}
	for _, b := range snap.Buckets {
		data.Buckets = append(data.Buckets, toBucketView(b))
	}

	// Severity first, like the uptime-phoenix wallboard: over-quota cards
	// before at-quota before stale before ok; name order within each tier.
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
		return 0
	case b.AtQuota:
		return 1
	case b.Stale:
		return 2
	default:
		return 3
	}
}
