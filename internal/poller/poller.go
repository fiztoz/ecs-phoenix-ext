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

// inventorySource is optionally implemented by the ECS client. The poller
// type-asserts for it so fixture fakes that only do billing keep working.
type inventorySource interface {
	BucketMetas(ctx context.Context, namespace string) (map[string]ecs.BucketMeta, error)
	NamespaceMetaInfo(ctx context.Context, namespace string) (*ecs.NamespaceMeta, error)
}

// BucketState is one bucket as shown to the HTTP layer.
type BucketState struct {
	Name             string
	Namespace        string
	UsedBytes        int64
	Objects          int64
	MPUBytes         int64
	QuotaBytes       *int64 // nil when no quota is set
	UsedPercent      *float64
	BlockSize        *int64 // nil when inventory has not reported it yet
	NotificationSize *int64 // nil when inventory has not reported it yet
	UptodateTill     time.Time
	Stale            bool
	AtQuota          bool // used >= quota: ECS rejects further writes
	OverStreak       int
	ConfirmedOver    bool
}

// Snapshot is the poller's last observation.
type Snapshot struct {
	Namespace              string
	PolledAt               time.Time
	PollOK                 bool
	LastError              string
	NamespaceBytes         int64
	NamespaceObjects       int64
	NamespaceQuotaBytes    *int64 // nil when no namespace quota is set
	NamespaceUsedPercent   *float64
	NamespaceAtQuota       bool
	NamespaceOverStreak    int
	NamespaceConfirmedOver bool
	// NamespaceDefaultBlock is the ECS default_bucket_block_size from
	// GET /object/namespaces/namespace/{ns}; nil until inventory succeeds.
	NamespaceDefaultBlock *int64
	// NamespaceBlockSize/NamespaceNotificationSize are the namespace-level
	// blockSize/notificationSize thresholds when ECS reports them
	// (positive only); nil when unset or until inventory succeeds.
	NamespaceBlockSize        *int64
	NamespaceNotificationSize *int64
	// InventoryOK reports whether the last bucket/namespace info enrich
	// succeeded. Billing is the source of truth: a failed inventory never
	// flips PollOK, it only sets InventoryError.
	InventoryOK    bool
	InventoryError string
	Buckets        []BucketState
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

	prev := p.previousStreaks()
	th := p.StaleThreshold()

	// Bucket quotas are ECS-native (Block Access thresholds from the bucket
	// inventory poll), never operator-set. A failed inventory keeps billing
	// live; thresholds fall back to the previous poll so hysteresis
	// survives a transient enrich failure.
	metas, nsmeta, invOK, invErr := p.fetchInventory(ctx)
	if !invOK {
		for name, pm := range p.previousInventory() {
			if _, ok := metas[name]; !ok {
				metas[name] = pm
			}
		}
	}

	rows := make([]store.StateRow, 0, len(payload.Buckets))
	buckets := make([]BucketState, 0, len(payload.Buckets))
	for _, b := range payload.Buckets {
		// ECS Block Access threshold is the quota; quota-off buckets
		// (no block threshold) have no quota: unlimited.
		var qp, block, notify *int64
		if m, ok := metas[b.Name]; ok {
			if m.HasBlock {
				v := m.BlockSize
				block, qp = &v, &v
			}
			if m.HasNotify {
				v := m.NotificationSize
				notify = &v
			}
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
			Name:             b.Name,
			Namespace:        b.Namespace,
			UsedBytes:        b.UsedBytes,
			Objects:          b.Objects,
			MPUBytes:         b.MPUBytes,
			QuotaBytes:       qp,
			BlockSize:        block,
			NotificationSize: notify,
			UptodateTill:     uptodate.UTC(),
			Stale:            !uptodate.IsZero() && now.Sub(uptodate) > th,
			AtQuota:          qp != nil && b.UsedBytes >= *qp,
			OverStreak:       streak,
			ConfirmedOver:    confirmed,
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

	// Namespace quota is ECS-native now, like buckets: the namespace-level
	// blockSize threshold (GiB, normalized to bytes by the parser) is the
	// limit, with the same 2-sample hysteresis before /health/quota trips.
	// On a failed inventory poll, fall back to the prior poll's values so a
	// limiter does not blink off on one bad enrich.
	var nsDefault, nsBlock, nsNotify *int64
	if nsmeta != nil {
		if nsmeta.HasDefaultBucketBlock {
			v := nsmeta.DefaultBucketBlock
			nsDefault = &v
		}
		if nsmeta.HasBlock {
			v := nsmeta.BlockSize
			nsBlock = &v
		}
		if nsmeta.HasNotify {
			v := nsmeta.NotificationSize
			nsNotify = &v
		}
	}
	if !invOK {
		pd, pb, pn := p.previousNamespaceMeta()
		if nsDefault == nil {
			nsDefault = pd
		}
		if nsBlock == nil {
			nsBlock = pb
		}
		if nsNotify == nil {
			nsNotify = pn
		}
	}

	var nsQuotaBytes *int64
	var nsPct *float64
	var nsAtQuota bool
	var nsStreak int
	var nsConfirmed bool
	if nsBlock != nil {
		nsQuotaBytes = nsBlock
		nsStreak, nsConfirmed = quota.Apply(p.prevNamespaceStreak(), nsQuotaBytes, payload.NamespaceBytes)
		nsAtQuota = payload.NamespaceBytes >= *nsQuotaBytes
		pct := float64(payload.NamespaceBytes) / float64(*nsQuotaBytes) * 100
		nsPct = &pct
	}

	p.mu.Lock()
	p.snap = Snapshot{
		Namespace:                 p.namespace,
		PolledAt:                  now,
		PollOK:                    true,
		LastError:                 "",
		NamespaceBytes:            payload.NamespaceBytes,
		NamespaceObjects:          payload.NamespaceObjects,
		NamespaceQuotaBytes:       nsQuotaBytes,
		NamespaceUsedPercent:      nsPct,
		NamespaceAtQuota:          nsAtQuota,
		NamespaceOverStreak:       nsStreak,
		NamespaceConfirmedOver:    nsConfirmed,
		NamespaceDefaultBlock:     nsDefault,
		NamespaceBlockSize:        nsBlock,
		NamespaceNotificationSize: nsNotify,
		InventoryOK:               invOK,
		InventoryError:            invErr,
		Buckets:                   buckets,
	}
	p.mu.Unlock()
	if !invOK && invErr != "" {
		p.log.Warn("poller: inventory enrich failed", "err", invErr)
	}
	p.log.Info("poller: billing poll ok", "buckets", len(buckets), "namespace_bytes", payload.NamespaceBytes)
}

// fetchInventory best-effort loads bucket + namespace info. Billing stays
// the source of truth: failures return ok=false and the caller falls back
// to the previous poll's thresholds, never PollOK=false.
func (p *Poller) fetchInventory(ctx context.Context) (map[string]ecs.BucketMeta, *ecs.NamespaceMeta, bool, string) {
	src, ok := p.client.(inventorySource)
	if !ok {
		return map[string]ecs.BucketMeta{}, nil, false, ""
	}
	metas, berr := src.BucketMetas(ctx, p.namespace)
	nsmeta, nerr := src.NamespaceMetaInfo(ctx, p.namespace)
	if berr != nil || nerr != nil {
		msg := ""
		if berr != nil {
			msg += "buckets: " + berr.Error()
		}
		if nerr != nil {
			if msg != "" {
				msg += "; "
			}
			msg += "namespace: " + nerr.Error()
		}
		if metas == nil {
			metas = map[string]ecs.BucketMeta{}
		}
		return metas, nsmeta, false, msg
	}
	if metas == nil {
		metas = map[string]ecs.BucketMeta{}
	}
	return metas, nsmeta, true, ""
}

// previousInventory returns the last poll's block/notify thresholds keyed
// by bucket, for carry-over when a fresh enrich fails.
func (p *Poller) previousInventory() map[string]ecs.BucketMeta {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]ecs.BucketMeta, len(p.snap.Buckets))
	for _, b := range p.snap.Buckets {
		m := ecs.BucketMeta{Name: b.Name, Namespace: b.Namespace}
		if b.BlockSize != nil {
			m.BlockSize, m.HasBlock = *b.BlockSize, true
		}
		if b.NotificationSize != nil {
			m.NotificationSize, m.HasNotify = *b.NotificationSize, true
		}
		out[b.Name] = m
	}
	return out
}

// previousNamespaceMeta returns the prior poll's namespace-level
// default/block/notify values (value-copied) for carry-over when a fresh
// inventory poll fails, so a limiter does not blink off on one bad enrich.
func (p *Poller) previousNamespaceMeta() (defaultBlock, block, notify *int64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if v := p.snap.NamespaceDefaultBlock; v != nil {
		c := *v
		defaultBlock = &c
	}
	if v := p.snap.NamespaceBlockSize; v != nil {
		c := *v
		block = &c
	}
	if v := p.snap.NamespaceNotificationSize; v != nil {
		c := *v
		notify = &c
	}
	return defaultBlock, block, notify
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

// PollOnce runs a single poll synchronously (used by tests and startup).
func (p *Poller) PollOnce(ctx context.Context) { p.pollOnce(ctx) }
