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
	BlockSize        *int64 // nil when unset or not yet reported
	BlockKnown       bool   // false until inventory (or a restored observation) reports the field
	NotificationSize *int64 // nil when unset or not yet reported
	NotifyKnown      bool
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
	NamespaceDefaultKnown bool
	// NamespaceBlockSize/NamespaceNotificationSize are the namespace-level
	// blockSize/notificationSize thresholds when ECS reports them
	// (positive only); nil when unset. The Known flags distinguish that
	// explicit quota-off from inventory that has not reported the field.
	NamespaceBlockSize        *int64
	NamespaceBlockKnown       bool
	NamespaceNotificationSize *int64
	NamespaceNotifyKnown      bool
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
	rows, err := st.States(ctx)
	if err != nil {
		log.Warn("poller: could not seed bucket state from store", "err", err)
	}
	ns, err := st.NamespaceStates(ctx)
	if err != nil {
		log.Warn("poller: could not seed namespace state from store", "err", err)
	}
	p.seedFromState(rows, ns)
	return p
}

func applySeededLimit(dst **int64, known *bool, src store.ObservedLimit) {
	*known = src.Known
	if !src.Known || src.Bytes == nil {
		*dst = nil
		return
	}
	v := *src.Bytes
	*dst = &v
}

func (p *Poller) seedFromState(rows []store.StateRow, ns []store.NamespaceState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	restored := false
	for _, r := range rows {
		if r.Namespace != p.namespace {
			continue
		}
		restored = true
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
		applySeededLimit(&b.BlockSize, &b.BlockKnown, r.Block)
		if b.BlockKnown && b.BlockSize != nil {
			v := *b.BlockSize
			b.QuotaBytes = &v
		}
		applySeededLimit(&b.NotificationSize, &b.NotifyKnown, r.Notify)
		p.snap.Buckets = append(p.snap.Buckets, b)
	}
	for _, n := range ns {
		if n.Namespace != p.namespace {
			continue
		}
		restored = true
		p.snap.NamespaceBytes = n.UsedBytes
		p.snap.NamespaceObjects = n.Objects
		p.snap.NamespaceOverStreak = n.OverStreak
		p.snap.NamespaceConfirmedOver = n.ConfirmedOver
		applySeededLimit(&p.snap.NamespaceDefaultBlock, &p.snap.NamespaceDefaultKnown, n.DefaultBlock)
		applySeededLimit(&p.snap.NamespaceBlockSize, &p.snap.NamespaceBlockKnown, n.Block)
		applySeededLimit(&p.snap.NamespaceNotificationSize, &p.snap.NamespaceNotifyKnown, n.Notify)
		if n.Block.Known && n.Block.Bytes != nil {
			v := *n.Block.Bytes
			p.snap.NamespaceQuotaBytes = &v
			pct := float64(n.UsedBytes) / float64(v) * 100
			p.snap.NamespaceUsedPercent = &pct
			p.snap.NamespaceAtQuota = n.UsedBytes >= v
		}
	}
	if restored {
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

	prev, nsPrevStreak, nsPrevConfirmed, prevDefault, prevBlock, prevNotify := p.carried()
	th := p.StaleThreshold()

	// Bucket and namespace quotas are ECS-native. Each inventory endpoint is
	// independent: a successful response is authoritative, including unset
	// fields (explicit quota-off). Fallback applies only to the endpoint that
	// failed, and an unknown threshold is not treated as unlimited.
	metas, bucketsOK, nsmeta, nsOK, invErr := p.fetchInventory(ctx)
	invOK := bucketsOK && nsOK

	buckets := make([]BucketState, 0, len(payload.Buckets))
	for _, b := range payload.Buckets {
		var qp, block, notify *int64
		blockKnown, notifyKnown := false, false
		if bucketsOK {
			blockKnown, notifyKnown = true, true
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
		} else if c, ok := prev[b.Name]; ok {
			blockKnown = c.blockKnown
			notifyKnown = c.notifyKnown
			if c.blockKnown && c.block != nil {
				v := *c.block
				block, qp = &v, &v
			}
			if c.notifyKnown && c.notify != nil {
				v := *c.notify
				notify = &v
			}
		}
		c := prev[b.Name]
		streak, confirmed := quota.Advance(c.streak, c.confirmed, bucketsOK, blockKnown, qp, b.UsedBytes)

		uptodate := b.UptodateTill.UTC()
		bs := BucketState{
			Name:             b.Name,
			Namespace:        p.namespace,
			UsedBytes:        b.UsedBytes,
			Objects:          b.Objects,
			MPUBytes:         b.MPUBytes,
			QuotaBytes:       qp,
			BlockSize:        block,
			BlockKnown:       blockKnown,
			NotificationSize: notify,
			NotifyKnown:      notifyKnown,
			UptodateTill:     uptodate,
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

	var nsDefault, nsBlock, nsNotify *int64
	nsDefaultKnown, nsBlockKnown, nsNotifyKnown := false, false, false
	if nsOK {
		// A successful namespace response is authoritative even when fields
		// are unset. Do not reinstate the previous limit because the bucket
		// endpoint failed.
		nsDefaultKnown, nsBlockKnown, nsNotifyKnown = true, true, true
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
	} else {
		nsDefault, nsDefaultKnown = prevDefault.bytes, prevDefault.known
		nsBlock, nsBlockKnown = prevBlock.bytes, prevBlock.known
		nsNotify, nsNotifyKnown = prevNotify.bytes, prevNotify.known
	}

	var nsQuotaBytes *int64
	var nsPct *float64
	var nsAtQuota bool
	if nsBlockKnown && nsBlock != nil {
		v := *nsBlock
		nsQuotaBytes = &v
		nsAtQuota = payload.NamespaceBytes >= v
		pct := float64(payload.NamespaceBytes) / float64(v) * 100
		nsPct = &pct
	}
	nsStreak, nsConfirmed := quota.Advance(nsPrevStreak, nsPrevConfirmed, nsOK, nsBlockKnown, nsQuotaBytes, payload.NamespaceBytes)

	rows := make([]store.StateRow, 0, len(buckets))
	for _, bs := range buckets {
		uptodate := bs.UptodateTill.UTC()
		rows = append(rows, store.StateRow{
			Namespace:     p.namespace,
			Bucket:        bs.Name,
			UsedBytes:     bs.UsedBytes,
			Objects:       bs.Objects,
			MPUBytes:      bs.MPUBytes,
			UptodateTill:  &uptodate,
			PolledAt:      now,
			OverStreak:    bs.OverStreak,
			ConfirmedOver: bs.ConfirmedOver,
			Block:         store.Observed(bs.BlockKnown, bs.BlockSize),
			Notify:        store.Observed(bs.NotifyKnown, bs.NotificationSize),
		})
	}
	if err := p.store.ReplaceObservation(ctx, store.Observation{
		Namespace: p.namespace,
		NamespaceState: store.NamespaceState{
			Namespace:     p.namespace,
			UsedBytes:     payload.NamespaceBytes,
			Objects:       payload.NamespaceObjects,
			PolledAt:      now,
			OverStreak:    nsStreak,
			ConfirmedOver: nsConfirmed,
			DefaultBlock:  store.Observed(nsDefaultKnown, nsDefault),
			Block:         store.Observed(nsBlockKnown, nsBlock),
			Notify:        store.Observed(nsNotifyKnown, nsNotify),
		},
		Buckets: rows,
	}); err != nil {
		p.log.Error("poller: persist state failed", "err", err)
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
		NamespaceDefaultKnown:     nsDefaultKnown,
		NamespaceBlockSize:        nsBlock,
		NamespaceBlockKnown:       nsBlockKnown,
		NamespaceNotificationSize: nsNotify,
		NamespaceNotifyKnown:      nsNotifyKnown,
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

// fetchInventory loads bucket and namespace info independently. Billing
// stays the source of truth: either failure leaves PollOK alone and the
// caller falls back only for the endpoint that failed.
func (p *Poller) fetchInventory(ctx context.Context) (map[string]ecs.BucketMeta, bool, *ecs.NamespaceMeta, bool, string) {
	src, ok := p.client.(inventorySource)
	if !ok {
		return map[string]ecs.BucketMeta{}, false, nil, false, ""
	}
	metas, berr := src.BucketMetas(ctx, p.namespace)
	nsmeta, nerr := src.NamespaceMetaInfo(ctx, p.namespace)
	msg := ""
	if berr != nil {
		msg = "buckets: " + berr.Error()
		metas = map[string]ecs.BucketMeta{}
	}
	if nerr != nil {
		if msg != "" {
			msg += "; "
		}
		msg += "namespace: " + nerr.Error()
		nsmeta = nil
	}
	if metas == nil {
		metas = map[string]ecs.BucketMeta{}
	}
	return metas, berr == nil, nsmeta, nerr == nil, msg
}

type carriedBucket struct {
	streak      int
	confirmed   bool
	blockKnown  bool
	block       *int64
	notifyKnown bool
	notify      *int64
}

type carriedLimit struct {
	known bool
	bytes *int64
}

func cloneI64(v *int64) *int64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

func (p *Poller) carried() (buckets map[string]carriedBucket, nsStreak int, nsConfirmed bool, nsDefault, nsBlock, nsNotify carriedLimit) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	buckets = make(map[string]carriedBucket, len(p.snap.Buckets))
	for _, b := range p.snap.Buckets {
		buckets[b.Name] = carriedBucket{
			streak:      b.OverStreak,
			confirmed:   b.ConfirmedOver,
			blockKnown:  b.BlockKnown,
			block:       cloneI64(b.BlockSize),
			notifyKnown: b.NotifyKnown,
			notify:      cloneI64(b.NotificationSize),
		}
	}
	return buckets, p.snap.NamespaceOverStreak, p.snap.NamespaceConfirmedOver,
		carriedLimit{known: p.snap.NamespaceDefaultKnown, bytes: cloneI64(p.snap.NamespaceDefaultBlock)},
		carriedLimit{known: p.snap.NamespaceBlockKnown, bytes: cloneI64(p.snap.NamespaceBlockSize)},
		carriedLimit{known: p.snap.NamespaceNotifyKnown, bytes: cloneI64(p.snap.NamespaceNotificationSize)}
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
