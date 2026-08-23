package http

import (
	"net/http"
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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "wallboard.html", data); err != nil {
		s.deps.Log.Error("wallboard render", "err", err)
	}
}
