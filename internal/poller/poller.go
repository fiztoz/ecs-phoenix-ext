// Package poller owns the billing poll loop and the in-memory snapshot the
// HTTP layer reads. Failed polls never wipe buckets; they mark poll_ok=false
// and keep the last good sample.
package poller

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/fiztoz/ecs-phoenix-ext/internal/ecs"
	"github.com/fiztoz/ecs-phoenix-ext/internal/quota"
	"github.com/fiztoz/ecs-phoenix-ext/internal/store"
)

// BillingSource is the subset of the ECS client the poller needs.
type BillingSource interface {
	NamespaceBilling(ctx context.Context, namespace string) (*ecs.BillingPayload, error)
}

// BucketState is one bucket as shown to the HTTP layer.
type BucketState struct {
	Name          string
	Namespace     string
	UsedBytes     int64
	Objects       int64
	MPUBytes      int64
	QuotaBytes    *int64 // nil when no quota is set
	UsedPercent   *float64
	UptodateTill  time.Time
	Stale         bool
	AtQuota       bool // used >= quota: ECS rejects further writes
	OverStreak    int
	ConfirmedOver bool
}

// Snapshot is the poller's last observation.
type Snapshot struct {
	Namespace            string
	PolledAt             time.Time
	PollOK               bool
	LastError            string
	NamespaceBytes       int64
	NamespaceObjects     int64
	NamespaceQuotaBytes  *int64   // nil when no namespace quota is set
	NamespaceUsedPercent *float64
	NamespaceAtQuota     bool
	NamespaceOverStreak  int
	NamespaceConfirmedOver bool
	Buckets              []BucketState
}

// Poller polls ECS billing on an interval and maintains hysteresis state.
type Poller struct {
	client    BillingSource
	store     store.Store
	namespace string
	interval  time.Duration
	log       *slog.Logger

	// now is injectable for tests.
	now func() time.Time

	mu   sync.RWMutex
	snap Snapshot
}

// New builds a Poller. The snapshot is seeded from durable store state so a
// restart does not forget a confirmed over-quota alarm.
func New(ctx context.Context, client BillingSource, st store.Store, namespace string, interval time.Duration, log *slog.Logger) *Poller {
	p := &Poller{
		client:    client,
		store:     st,
		namespace: namespace,
		interval:  interval,
		log:       log,
		now:       time.Now,
		snap:      Snapshot{Namespace: namespace, PollOK: false},
	}
	if rows, err := st.States(ctx); err == nil {
		p.seedFromState(rows)
	} else {
		log.Warn("poller: could not seed state from store", "err", err)
	}
	return p
}

func (p *Poller) seedFromState(rows []store.StateRow) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range rows {
		if r.Namespace != p.namespace {
			continue
		}
		b := BucketState{
			Name:          r.Bucket,
			Namespace:     r.Namespace,
			UsedBytes:     r.UsedBytes,
			Objects:       r.Objects,
			MPUBytes:      r.MPUBytes,
			OverStreak:    r.OverStreak,
			ConfirmedOver: r.ConfirmedOver,
			Stale:         true, // until a fresh sample proves otherwise
		}
		if r.UptodateTill != nil {
			b.UptodateTill = *r.UptodateTill
		}
		p.snap.Buckets = append(p.snap.Buckets, b)
	}
	if len(p.snap.Buckets) > 0 {
		p.snap.LastError = "restored last observation; no poll has succeeded since start"
	}
}

// Run blocks polling until ctx is cancelled: once immediately, then on the
// interval.
func (p *Poller) Run(ctx context.Context) {
	p.pollOnce(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.pollOnce(ctx)
		}
	}
}

// StaleThreshold is max(3 * poll interval, 1h): the age after which
// uptodate_till marks a bucket stale.
func (p *Poller) StaleThreshold() time.Duration {
	th := 3 * p.interval
	if th < time.Hour {
		th = time.Hour
	}
	return th
}

func (p *Poller) pollOnce(ctx context.Context) {
	payload, err := p.client.NamespaceBilling(ctx, p.namespace)
	now := p.now().UTC()
	if err != nil {
		p.mu.Lock()
		// Keep buckets; mark the failure. /health/ready will 503.
		p.snap.PollOK = false
		p.snap.PolledAt = now
		p.snap.LastError = err.Error()
		p.mu.Unlock()
		p.log.Error("poller: billing poll failed", "err", err)
		return
	}

	quotas, qerr := p.store.Quotas(ctx, p.namespace)
	if qerr != nil {
		p.log.Warn("poller: quota lookup failed; hysteresis uses no quotas", "err", qerr)
		quotas = map[string]store.QuotaRow{}
	}

	nsQuota, nsqErr := p.store.NamespaceQuota(ctx, p.namespace)
	if nsqErr != nil {
		p.log.Warn("poller: namespace quota lookup failed", "err", nsqErr)
	}

	prev := p.previousStreaks()
	th := p.StaleThreshold()

	rows := make([]store.StateRow, 0, len(payload.Buckets))
	buckets := make([]BucketState, 0, len(payload.Buckets))
	for _, b := range payload.Buckets {
		var qp *int64
		if q, ok := quotas[b.Name]; ok {
			v := q.QuotaBytes
			qp = &v
		}
		streak, confirmed := quota.Apply(prev[b.Name], qp, b.UsedBytes)

		// Normalise to UTC before bind — same class of bug Phoenix shipped once.
		uptodate := b.UptodateTill.UTC()
		rows = append(rows, store.StateRow{
			Namespace:     b.Namespace,
			Bucket:        b.Name,
			UsedBytes:     b.UsedBytes,
			Objects:       b.Objects,
			MPUBytes:      b.MPUBytes,
			UptodateTill:  &uptodate,
			PolledAt:      now,
			OverStreak:    streak,
			ConfirmedOver: confirmed,
		})

		bs := BucketState{
			Name:          b.Name,
			Namespace:     b.Namespace,
			UsedBytes:     b.UsedBytes,
			Objects:       b.Objects,
			MPUBytes:      b.MPUBytes,
			QuotaBytes:    qp,
			UptodateTill:  uptodate.UTC(),
			Stale:         !uptodate.IsZero() && now.Sub(uptodate) > th,
			AtQuota:       qp != nil && b.UsedBytes >= *qp,
			OverStreak:    streak,
			ConfirmedOver: confirmed,
		}
		if qp != nil && *qp > 0 {
			pct := float64(b.UsedBytes) / float64(*qp) * 100
			bs.UsedPercent = &pct
		}
		buckets = append(buckets, bs)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Name < buckets[j].Name })

	if err := p.store.UpsertStates(ctx, rows); err != nil {
		p.log.Error("poller: persist state failed", "err", err)
	}

	// Namespace-level quota hysteresis.
	var nsQuotaBytes *int64
	var nsPct *float64
	var nsAtQuota bool
	var nsStreak int
	var nsConfirmed bool
	if nsQuota != nil {
		v := nsQuota.QuotaBytes
		nsQuotaBytes = &v
		nsStreak, nsConfirmed = quota.Apply(p.prevNamespaceStreak(), nsQuotaBytes, payload.NamespaceBytes)
		nsAtQuota = payload.NamespaceBytes >= *nsQuotaBytes
		pct := float64(payload.NamespaceBytes) / float64(*nsQuotaBytes) * 100
		nsPct = &pct
	}

	p.mu.Lock()
	p.snap = Snapshot{
		Namespace:              p.namespace,
		PolledAt:               now,
		PollOK:                 true,
		LastError:              "",
		NamespaceBytes:         payload.NamespaceBytes,
		NamespaceObjects:       payload.NamespaceObjects,
		NamespaceQuotaBytes:    nsQuotaBytes,
		NamespaceUsedPercent:   nsPct,
		NamespaceAtQuota:       nsAtQuota,
		NamespaceOverStreak:    nsStreak,
		NamespaceConfirmedOver: nsConfirmed,
		Buckets:                buckets,
	}
	p.mu.Unlock()
	p.log.Info("poller: billing poll ok", "buckets", len(buckets), "namespace_bytes", payload.NamespaceBytes)
}

func (p *Poller) previousStreaks() map[string]int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]int, len(p.snap.Buckets))
	for _, b := range p.snap.Buckets {
		out[b.Name] = b.OverStreak
	}
	return out
}

func (p *Poller) prevNamespaceStreak() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.snap.NamespaceOverStreak
}

// Snapshot returns a copy of the current snapshot.
func (p *Poller) Snapshot() Snapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	cp := p.snap
	cp.Buckets = make([]BucketState, len(p.snap.Buckets))
	copy(cp.Buckets, p.snap.Buckets)
	return cp
}

// RefreshQuotas re-reads quotas from the store and recomputes the quota,
// percent and hysteresis fields on the current snapshot without waiting for
// the next poll, so a quota set/delete is visible immediately.
func (p *Poller) RefreshQuotas(ctx context.Context) {
	quotas, err := p.store.Quotas(ctx, p.namespace)
	if err != nil {
		p.log.Warn("poller: refresh quotas lookup failed", "err", err)
		return
	}
	nsQuota, nsqErr := p.store.NamespaceQuota(ctx, p.namespace)
	if nsqErr != nil {
		p.log.Warn("poller: refresh namespace quota lookup failed", "err", nsqErr)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.snap.PollOK {
		return // nothing polled yet; next poll will apply quotas
	}
	prev := make(map[string]int, len(p.snap.Buckets))
	for _, b := range p.snap.Buckets {
		prev[b.Name] = b.OverStreak
	}
	for i, b := range p.snap.Buckets {
		var qp *int64
		if q, ok := quotas[b.Name]; ok {
			v := q.QuotaBytes
			qp = &v
		}
		streak, confirmed := quota.Apply(prev[b.Name], qp, b.UsedBytes)
		p.snap.Buckets[i].QuotaBytes = qp
		p.snap.Buckets[i].UsedPercent = nil
		p.snap.Buckets[i].AtQuota = qp != nil && b.UsedBytes >= *qp
		p.snap.Buckets[i].OverStreak = streak
		p.snap.Buckets[i].ConfirmedOver = confirmed
		if qp != nil && *qp > 0 {
			pct := float64(b.UsedBytes) / float64(*qp) * 100
			p.snap.Buckets[i].UsedPercent = &pct
		}
	}

	// Namespace-level quota refresh.
	if nsQuota != nil {
		v := nsQuota.QuotaBytes
		p.snap.NamespaceQuotaBytes = &v
		p.snap.NamespaceOverStreak, p.snap.NamespaceConfirmedOver = quota.Apply(
			p.snap.NamespaceOverStreak, &v, p.snap.NamespaceBytes)
		p.snap.NamespaceAtQuota = p.snap.NamespaceBytes >= v
		pct := float64(p.snap.NamespaceBytes) / float64(v) * 100
		p.snap.NamespaceUsedPercent = &pct
	} else {
		p.snap.NamespaceQuotaBytes = nil
		p.snap.NamespaceUsedPercent = nil
		p.snap.NamespaceAtQuota = false
		p.snap.NamespaceOverStreak = 0
		p.snap.NamespaceConfirmedOver = false
	}
}

// PollOnce runs a single poll synchronously (used by tests and startup).
func (p *Poller) PollOnce(ctx context.Context) { p.pollOnce(ctx) }
