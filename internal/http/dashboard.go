package http

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fiztoz/ecs-phoenix-ext/internal/ecs"
	"github.com/fiztoz/ecs-phoenix-ext/internal/poller"
)

// bucketView is the pre-formatted per-bucket row for templates.
type bucketView struct {
	Name          string
	Used          string
	UsedBytes     int64
	Objects       int64
	MPU           string
	MPUBytes      int64
	HasQuota      bool
	Quota         string
	QuotaBytes    int64
	Percent       string
	BarWidth      int
	SampleTime    string
	SampleUnix    int64 // -1 when no sample (quota-only)
	UptodateTill  string
	Stale         bool
	OverStreak    int
	ConfirmedOver bool
	QuotaOnly     bool // quota set but not in last poll
}

type dashboardData struct {
	BasePath         string
	Namespace        string
	Now              time.Time
	PolledAt         time.Time
	PollOK           bool
	LastError        string
	NamespaceUsed    string
	NamespaceObjects int64
	Buckets          []bucketView
	Flash            string
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
		BasePath:         strings.TrimRight(s.deps.BasePath, "/"),
		Namespace:        snap.Namespace,
		Now:              now,
		PolledAt:         snap.PolledAt,
		PollOK:           snap.PollOK,
		LastError:        snap.LastError,
		NamespaceUsed:    HumanBytes(snap.NamespaceBytes),
		NamespaceObjects: snap.NamespaceObjects,
		Flash:            r.URL.Query().Get("msg"),
	}

	seen := make(map[string]bool, len(snap.Buckets))
	for _, b := range snap.Buckets {
		seen[b.Name] = true
		data.Buckets = append(data.Buckets, toBucketView(b))
	}

	// Quotas set for buckets not present in the last poll.
	if quotas, err := s.deps.Store.Quotas(r.Context(), s.deps.Namespace); err == nil {
		for name, q := range quotas {
			if seen[name] {
				continue
			}
			v := bucketView{
				Name:       name,
				HasQuota:   true,
				Quota:      HumanBytes(q.QuotaBytes),
				QuotaBytes: q.QuotaBytes,
				SampleUnix: -1,
				QuotaOnly:  true,
			}
			data.Buckets = append(data.Buckets, v)
		}
	}
	return data
}

func toBucketView(b poller.BucketState) bucketView {
	v := bucketView{
		Name:          b.Name,
		Used:          HumanBytes(b.UsedBytes),
		UsedBytes:     b.UsedBytes,
		Objects:       b.Objects,
		MPU:           HumanBytes(b.MPUBytes),
		MPUBytes:      b.MPUBytes,
		SampleTime:    b.SampleTime.UTC().Format("2006-01-02 15:04 UTC"),
		SampleUnix:    sampleUnix(b.SampleTime),
		Stale:         b.Stale,
		OverStreak:    b.OverStreak,
		ConfirmedOver: b.ConfirmedOver,
	}
	if b.UptodateTill.IsZero() {
		v.UptodateTill = "—"
	} else {
		v.UptodateTill = b.UptodateTill.UTC().Format("2006-01-02 15:04 UTC")
	}
	if b.QuotaBytes != nil {
		v.HasQuota = true
		v.QuotaBytes = *b.QuotaBytes
		v.Quota = HumanBytes(*b.QuotaBytes)
	}
	if b.UsedPercent != nil {
		v.Percent = pctStr(*b.UsedPercent)
		w := int(*b.UsedPercent)
		if w > 100 {
			w = 100
		}
		if w < 0 {
			w = 0
		}
		v.BarWidth = w
	}
	return v
}

func sampleUnix(t time.Time) int64 {
	if t.IsZero() {
		return -1
	}
	return t.Unix()
}

func pctStr(p float64) string {
	return strconv.FormatFloat(p, 'f', 1, 64) + "%"
}

// handleQuotaForm implements the HTML quota forms (PRG): set or delete a
// quota, then redirect back to the dashboard.
func (s *Server) handleQuotaForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	base := strings.TrimRight(s.deps.BasePath, "/")
	redirect := func(msg string) {
		http.Redirect(w, r, base+"/?msg="+url.QueryEscape(msg), http.StatusSeeOther)
	}

	bucket := r.Form.Get("bucket")
	if err := validateBucket(bucket); err != nil {
		redirect("error: " + err.Error())
		return
	}

	if r.Form.Get("action") == "delete" {
		if err := s.deps.Store.DeleteQuota(r.Context(), s.deps.Namespace, bucket); err != nil {
			s.deps.Log.Error("form delete quota", "err", err)
			redirect("error: could not delete quota")
			return
		}
		redirect("quota removed for " + bucket)
		return
	}

	q, err := strconv.ParseFloat(r.Form.Get("quota"), 64)
	if err != nil || q <= 0 {
		redirect("error: quota must be a positive number")
		return
	}
	unit := r.Form.Get("unit")
	if unit == "" {
		unit = "GiB"
	}
	bytes, err := ecs.ToBytes(q, unit)
	if err != nil {
		redirect("error: " + err.Error())
		return
	}
	if err := s.deps.Store.SetQuota(r.Context(), s.deps.Namespace, bucket, bytes); err != nil {
		s.deps.Log.Error("form set quota", "err", err)
		redirect("error: could not save quota")
		return
	}
	redirect("quota set for " + bucket + ": " + HumanBytes(bytes))
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
