package http

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fiztoz/ecs-phoenix-ext/internal/ecs"
	"github.com/fiztoz/ecs-phoenix-ext/internal/poller"
)

// bucketView is the pre-formatted per-bucket row for templates.
// Only necessary columns are shown: name, usage, objects, block/notify
// sizing knobs, quota % and status. MPU is folded into Used (billing
// already sums total+mpu) and deliberately has no column.
type bucketView struct {
	Name          string
	Used          string
	UsedBytes     int64
	Objects       int64
	MPUBytes      int64 // kept for the JSON API; no table column
	Block         string
	BlockBytes    int64 // -1 when inventory has not reported it
	Notify        string
	NotifyBytes   int64 // -1 when inventory has not reported it
	HasBlock      bool
	HasNotify     bool
	QuotaMode     string // off | notify-only | block-only | block-notify (ECS-native)
	HasQuota      bool
	Quota         string
	QuotaBytes    int64
	Percent       string
	BarWidth      int
	Stale         bool
	AtQuota       bool // used >= quota: ECS rejects further writes
	OverStreak    int
	ConfirmedOver bool
	NotifyReached bool // used >= ECS notification threshold
}

type dashboardData struct {
	BasePath               string
	Namespace              string
	Now                    time.Time
	PolledAt               time.Time
	PollOK                 bool
	LastError              string
	InventoryError         string
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
	Flash                  string
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	data := s.buildDashboard(r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "dashboard.html", data); err != nil {
		s.deps.Log.Error("dashboard render", "err", err)
	}
}

func (s *Server) buildDashboard(r *http.Request) dashboardData {
	snap := s.deps.Snapshots.Snapshot()
	now := time.Now().UTC()
	data := dashboardData{
		BasePath:           strings.TrimRight(s.deps.BasePath, "/"),
		Namespace:          snap.Namespace,
		Now:                now,
		PolledAt:           snap.PolledAt,
		PollOK:             snap.PollOK,
		LastError:          snap.LastError,
		NamespaceUsed:      HumanBytes(snap.NamespaceBytes),
		NamespaceUsedBytes: snap.NamespaceBytes,
		NamespaceObjects:   snap.NamespaceObjects,
		Flash:              r.URL.Query().Get("msg"),
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
	data.InventoryError = snap.InventoryError

	for _, b := range snap.Buckets {
		data.Buckets = append(data.Buckets, toBucketView(b))
	}
	return data
}

func toBucketView(b poller.BucketState) bucketView {
	v := bucketView{
		Name:          b.Name,
		Used:          HumanBytes(b.UsedBytes),
		UsedBytes:     b.UsedBytes,
		Objects:       b.Objects,
		MPUBytes:      b.MPUBytes,
		BlockBytes:    -1,
		NotifyBytes:   -1,
		Block:         "—",
		Notify:        "—",
		Stale:         b.Stale,
		AtQuota:       b.AtQuota,
		OverStreak:    b.OverStreak,
		ConfirmedOver: b.ConfirmedOver,
		QuotaBytes:    quotaBytesOrZero(b.QuotaBytes),
		HasQuota:      b.QuotaBytes != nil,
		Quota:         quotaOrUnlimited(b.QuotaBytes),
		Percent:       percentOrDash(b.UsedPercent),
		BarWidth:      barWidth(b.UsedPercent),
		NotifyReached: b.NotificationSize != nil && b.UsedBytes >= *b.NotificationSize,
	}
	if b.BlockSize != nil {
		v.HasBlock = true
		v.BlockBytes = *b.BlockSize
		v.Block = HumanBytes(*b.BlockSize)
	}
	if b.NotificationSize != nil {
		v.HasNotify = true
		v.NotifyBytes = *b.NotificationSize
		v.Notify = HumanBytes(*b.NotificationSize)
	}
	v.QuotaMode = ecs.BucketMeta{HasBlock: v.HasBlock, HasNotify: v.HasNotify}.QuotaMode()
	return v
}

func quotaBytesOrZero(q *int64) int64 {
	if q == nil {
		return 0
	}
	return *q
}

// quotaOrUnlimited renders the ECS Block Access threshold; quota-off
// buckets (no threshold) are unlimited, never a dash.
func quotaOrUnlimited(q *int64) string {
	if q == nil {
		return "Unlimited"
	}
	return HumanBytes(*q)
}

func percentOrDash(p *float64) string {
	if p == nil {
		return "—"
	}
	return pctStr(*p)
}

func barWidth(p *float64) int {
	if p == nil {
		return 0
	}
	w := int(*p)
	if w > 100 {
		w = 100
	}
	if w < 0 {
		w = 0
	}
	return w
}

func pctStr(p float64) string {
	return strconv.FormatFloat(p, 'f', 1, 64) + "%"
}

// HumanBytes renders bytes with binary units (GiB, never "GB").
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	suffix := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	v := float64(n)
	i := -1
	for v >= unit {
		v /= unit
		i++
		if i == len(suffix)-1 {
			break
		}
	}
	return strconv.FormatFloat(v, 'f', 2, 64) + " " + suffix[i]
}
