// Package ops is the single read surface behind every programmatic entry
// point: the legacy /api/buckets JSON, the versioned /api/v1 REST API and the
// MCP tools all build their answers here, so the wire shapes cannot drift
// apart.
//
// Every operation is read-only and served from the poller snapshot or the
// activity history table. Nothing here calls ECS: the management user is
// NAMESPACE_ADMIN, and a caller-driven path to it would hand that role to
// whoever can reach the API. A future write operation must be added here as
// its own named method with its own scope, never as a passthrough.
package ops

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/fiztoz/ecs-phoenix-ext/internal/poller"
	"github.com/fiztoz/ecs-phoenix-ext/internal/store"
)

// ErrNotFound reports a bucket that is neither in the latest snapshot nor in
// the activity history.
var ErrNotFound = errors.New("unknown bucket")

// SnapshotSource is what ops reads from the poller.
type SnapshotSource interface {
	Snapshot() poller.Snapshot
	StaleThreshold() time.Duration
}

// Service answers read operations for one namespace.
type Service struct {
	DefaultNamespace string // used before the first poll names the namespace
	Snapshots        SnapshotSource
	Store            store.Store
	// Now is injectable for tests; nil means time.Now.
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Bucket is one bucket on the wire.
type Bucket struct {
	Name             string     `json:"name"`
	Namespace        string     `json:"namespace"`
	UsedBytes        int64      `json:"used_bytes"`
	Objects          int64      `json:"objects"`
	MPUBytes         int64      `json:"mpu_bytes"`
	MPUParts         int64      `json:"mpu_parts"`         // in-flight incomplete multipart parts
	BlockSize        *int64     `json:"block_size"`        // null until inventory succeeds / when ECS quota off
	NotificationSize *int64     `json:"notification_size"` // null until inventory succeeds / when ECS quota off
	QuotaMode        string     `json:"quota_mode"`        // off | notify-only | block-only | block-notify (ECS-native)
	QuotaBytes       *int64     `json:"quota_bytes"`       // null when unset
	UsedPercent      *float64   `json:"used_percent"`
	UptodateTill     *time.Time `json:"uptodate_till"` // null when ECS omitted metering freshness
	Stale            bool       `json:"stale"`
	AtQuota          bool       `json:"at_quota"`
	OverStreak       int        `json:"over_streak"`
	ConfirmedOver    bool       `json:"confirmed_over"`
	// Activity is null until two comparable metering observations exist.
	Activity *Activity `json:"activity"`
}

// Activity is derived write-side movement. ECS billing exposes no request
// counters to a namespace-scoped management user, so these are deltas between
// two metering observations: read traffic is invisible, and the deltas are net
// (overwrite-in-place and write-then-delete both look like zero).
type Activity struct {
	Known bool `json:"known"`
	// State is one of moved | idle | unrefreshed | unchanged. "unchanged"
	// means ECS reports no uptodate_till for this bucket, so an idle bucket
	// and one ECS has not republished yet cannot be told apart.
	State             string  `json:"state"`
	Changed           bool    `json:"changed"`
	MeteringRefreshed bool    `json:"metering_refreshed"`       // ECS advanced uptodate_till
	FreshnessKnown    bool    `json:"metering_freshness_known"` // uptodate_till present on both sides
	WindowSeconds     float64 `json:"window_seconds"`
	ObjectsDelta      int64   `json:"objects_delta"`
	BytesDelta        int64   `json:"bytes_delta"`
	ObjectsPerHour    float64 `json:"objects_per_hour"`
	BytesPerHour      float64 `json:"bytes_per_hour"`
	MPUParts          int64   `json:"mpu_parts"`
	MPUPartsDelta     int64   `json:"mpu_parts_delta"`
	BusyHours         int     `json:"busy_hours"`     // hours with movement inside the lookback
	ObservedHours     int     `json:"observed_hours"` // hours with a usable metering observation
	Hot               bool    `json:"hot"`            // movement in most observed hours
}

// Summary is the namespace-level part of a report.
type Summary struct {
	Namespace          string    `json:"namespace"`
	PolledAt           time.Time `json:"polled_at"`
	PollOK             bool      `json:"poll_ok"`
	LastError          string    `json:"last_error"`
	InventoryOK        bool      `json:"inventory_ok"`
	InventoryError     string    `json:"inventory_error"`
	NamespaceUsedBytes int64     `json:"namespace_used_bytes"`
	NamespaceObjects   int64     `json:"namespace_objects"`
	// NamespaceUptodateTill is ECS's own claim about how current this metering
	// is; null when ECS omits it. It can lag wall clock by days while
	// per-bucket counters keep moving, so the age is exposed next to it
	// rather than left for consumers to derive.
	NamespaceUptodateTill       *time.Time `json:"namespace_uptodate_till"`
	NamespaceMeteringAgeSeconds *int64     `json:"namespace_metering_age_seconds"`
	NamespaceQuotaBytes         *int64     `json:"namespace_quota_bytes"`
	NamespaceUsedPercent        *float64   `json:"namespace_used_percent"`
	NamespaceDefaultBlock       *int64     `json:"namespace_default_block_size"`
	NamespaceBlockSize          *int64     `json:"namespace_block_size"`
	NamespaceNotificationSize   *int64     `json:"namespace_notification_size"`
	NamespaceAtQuota            bool       `json:"namespace_at_quota"`
	NamespaceConfirmedOver      bool       `json:"namespace_confirmed_over"`
	StaleAfterSeconds           int64      `json:"stale_after_seconds"`
	ActivityWindowSeconds       int64      `json:"activity_window_seconds"`
	HotBuckets                  int        `json:"hot_buckets"`
}

// Report is the full namespace view: the /api/buckets wire shape.
type Report struct {
	Summary
	Buckets []Bucket `json:"buckets"`
}

// NamespaceSummary adds bucket roll-ups to Summary for callers that do not
// want every bucket row.
type NamespaceSummary struct {
	Summary
	BucketCount          int `json:"bucket_count"`
	AtQuotaBuckets       int `json:"at_quota_buckets"`
	ConfirmedOverBuckets int `json:"confirmed_over_buckets"`
	StaleBuckets         int `json:"stale_buckets"`
}

// Report builds the full namespace report from the latest snapshot.
func (s *Service) Report() Report {
	snap := s.Snapshots.Snapshot()
	r := Report{Summary: s.summary(snap), Buckets: make([]Bucket, 0, len(snap.Buckets))}
	for _, b := range snap.Buckets {
		r.Buckets = append(r.Buckets, toBucket(b))
	}
	return r
}

// Namespace returns the namespace summary with bucket roll-up counts.
func (s *Service) Namespace() NamespaceSummary {
	r := s.Report()
	out := NamespaceSummary{Summary: r.Summary, BucketCount: len(r.Buckets)}
	for _, b := range r.Buckets {
		if b.AtQuota {
			out.AtQuotaBuckets++
		}
		if b.ConfirmedOver {
			out.ConfirmedOverBuckets++
		}
		if b.Stale {
			out.StaleBuckets++
		}
	}
	return out
}

// Sort keys accepted by ListBuckets.
const (
	SortName        = "name"
	SortUsed        = "used"
	SortObjects     = "objects"
	SortUsedPercent = "used_percent"
	SortActivity    = "activity"
)

// SortKeys lists every accepted sort key, for validation and schemas.
var SortKeys = []string{SortName, SortUsed, SortObjects, SortUsedPercent, SortActivity}

// MaxListLimit caps one ListBuckets page.
const MaxListLimit = 1000

// ListOptions filters and orders ListBuckets. The zero value lists every
// bucket by name.
type ListOptions struct {
	Sort        string // one of SortKeys; empty means name
	Prefix      string // bucket name prefix
	AtQuotaOnly bool
	HotOnly     bool
	Limit       int // 0 means MaxListLimit
}

// BucketList is one ListBuckets answer.
type BucketList struct {
	Namespace         string    `json:"namespace"`
	PolledAt          time.Time `json:"polled_at"`
	PollOK            bool      `json:"poll_ok"`
	StaleAfterSeconds int64     `json:"stale_after_seconds"`
	Sort              string    `json:"sort"`
	// Total counts matches before Limit, so a caller can tell a short page
	// from a truncated one.
	Total     int      `json:"total"`
	Truncated bool     `json:"truncated"`
	Buckets   []Bucket `json:"buckets"`
}

// ErrBadSort reports an unknown sort key.
var ErrBadSort = errors.New("sort must be one of " + strings.Join(SortKeys, ", "))

// ListBuckets returns the buckets matching opts. Numeric sorts are
// descending (biggest first) and break ties by name; buckets with an unknown
// value (no quota for used_percent, no baseline for activity) sort last.
func (s *Service) ListBuckets(opts ListOptions) (BucketList, error) {
	key := opts.Sort
	if key == "" {
		key = SortName
	}
	less, ok := sorters[key]
	if !ok {
		return BucketList{}, ErrBadSort
	}
	limit := opts.Limit
	if limit <= 0 || limit > MaxListLimit {
		limit = MaxListLimit
	}

	r := s.Report()
	out := BucketList{
		Namespace:         r.Namespace,
		PolledAt:          r.PolledAt,
		PollOK:            r.PollOK,
		StaleAfterSeconds: r.StaleAfterSeconds,
		Sort:              key,
		Buckets:           make([]Bucket, 0, len(r.Buckets)),
	}
	for _, b := range r.Buckets {
		if opts.Prefix != "" && !strings.HasPrefix(b.Name, opts.Prefix) {
			continue
		}
		if opts.AtQuotaOnly && !b.AtQuota {
			continue
		}
		if opts.HotOnly && (b.Activity == nil || !b.Activity.Hot) {
			continue
		}
		out.Buckets = append(out.Buckets, b)
	}
	sort.SliceStable(out.Buckets, func(i, j int) bool {
		a, b := out.Buckets[i], out.Buckets[j]
		if c := less(a, b); c != 0 {
			return c < 0
		}
		return a.Name < b.Name
	})
	out.Total = len(out.Buckets)
	if len(out.Buckets) > limit {
		out.Buckets = out.Buckets[:limit]
		out.Truncated = true
	}
	return out, nil
}

// sorters compare two buckets: negative puts a first, 0 falls back to name.
var sorters = map[string]func(a, b Bucket) int{
	SortName:    func(a, b Bucket) int { return 0 },
	SortUsed:    func(a, b Bucket) int { return desc(float64(a.UsedBytes), float64(b.UsedBytes)) },
	SortObjects: func(a, b Bucket) int { return desc(float64(a.Objects), float64(b.Objects)) },
	SortUsedPercent: func(a, b Bucket) int {
		return descKnown(a.UsedPercent != nil, b.UsedPercent != nil, deref(a.UsedPercent), deref(b.UsedPercent))
	},
	SortActivity: func(a, b Bucket) int {
		return descKnown(a.Activity != nil, b.Activity != nil, absRate(a.Activity), absRate(b.Activity))
	},
}

func desc(a, b float64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return 0
}

func descKnown(aKnown, bKnown bool, a, b float64) int {
	switch {
	case aKnown && !bKnown:
		return -1
	case !aKnown && bKnown:
		return 1
	case !aKnown:
		return 0
	}
	return desc(a, b)
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func absRate(a *Activity) float64 {
	if a == nil {
		return 0
	}
	if a.ObjectsPerHour < 0 {
		return -a.ObjectsPerHour
	}
	return a.ObjectsPerHour
}

// Bucket returns one bucket from the latest snapshot.
func (s *Service) Bucket(name string) (Bucket, error) {
	for _, b := range s.Snapshots.Snapshot().Buckets {
		if b.Name == name {
			return toBucket(b), nil
		}
	}
	return Bucket{}, ErrNotFound
}

// MaxHistoryHours caps one ActivityHistory lookback (the store's own
// retention ceiling is 90 days).
const MaxHistoryHours = 2160

// ActivityHour is one rolled-up hour of derived activity on the wire.
type ActivityHour struct {
	Hour          time.Time `json:"hour"`
	Samples       int       `json:"samples"`
	ActiveSamples int       `json:"active_samples"`
	ObjectsUp     int64     `json:"objects_up"`
	ObjectsDown   int64     `json:"objects_down"`
	BytesUp       int64     `json:"bytes_up"`
	BytesDown     int64     `json:"bytes_down"`
	MPUPartsMax   int64     `json:"mpu_parts_max"`
}

// ActivityHistory is one bucket's hourly derived activity, oldest first.
type ActivityHistory struct {
	Namespace string    `json:"namespace"`
	Bucket    string    `json:"bucket"`
	Since     time.Time `json:"since"`
	// Hours lists only hours with at least one usable metering observation;
	// a missing hour means nothing was measured, not that nothing moved.
	Hours []ActivityHour `json:"hours"`
	// Note restates the honest limit so a caller that only reads this object
	// cannot mistake it for request traffic.
	Note string `json:"note"`
}

// HistoryNote is the limit every activity answer carries.
const HistoryNote = "derived from ECS billing deltas: write-side net movement only; reads are invisible"

// ActivityHistory returns the last hours of rolled-up activity for bucket.
// A bucket absent from both the snapshot and the history is ErrNotFound; a
// bucket deleted since still returns the history it has.
func (s *Service) ActivityHistory(ctx context.Context, bucket string, hours int) (ActivityHistory, error) {
	if hours <= 0 {
		hours = 24
	}
	if hours > MaxHistoryHours {
		hours = MaxHistoryHours
	}
	ns := s.namespace(s.Snapshots.Snapshot())
	since := s.now().UTC().Truncate(time.Hour).Add(-time.Duration(hours-1) * time.Hour)
	rows, err := s.Store.ActivityHours(ctx, ns, since)
	if err != nil {
		return ActivityHistory{}, err
	}
	out := ActivityHistory{Namespace: ns, Bucket: bucket, Since: since, Hours: []ActivityHour{}, Note: HistoryNote}
	for _, h := range rows {
		if h.Bucket != bucket {
			continue
		}
		out.Hours = append(out.Hours, ActivityHour{
			Hour:          h.HourBucket.UTC(),
			Samples:       h.Samples,
			ActiveSamples: h.ActiveSamples,
			ObjectsUp:     h.ObjectsUp,
			ObjectsDown:   h.ObjectsDown,
			BytesUp:       h.BytesUp,
			BytesDown:     h.BytesDown,
			MPUPartsMax:   h.MPUPartsMax,
		})
	}
	if len(out.Hours) == 0 {
		if _, err := s.Bucket(bucket); err != nil {
			return ActivityHistory{}, err
		}
	}
	return out, nil
}

func (s *Service) namespace(snap poller.Snapshot) string {
	if snap.Namespace != "" {
		return snap.Namespace
	}
	return s.DefaultNamespace
}

func (s *Service) summary(snap poller.Snapshot) Summary {
	sum := Summary{
		Namespace:                 snap.Namespace,
		PolledAt:                  snap.PolledAt.UTC(),
		PollOK:                    snap.PollOK,
		LastError:                 snap.LastError,
		InventoryOK:               snap.InventoryOK,
		InventoryError:            snap.InventoryError,
		NamespaceUsedBytes:        snap.NamespaceBytes,
		NamespaceObjects:          snap.NamespaceObjects,
		NamespaceQuotaBytes:       snap.NamespaceQuotaBytes,
		NamespaceUsedPercent:      snap.NamespaceUsedPercent,
		NamespaceDefaultBlock:     snap.NamespaceDefaultBlock,
		NamespaceBlockSize:        snap.NamespaceBlockSize,
		NamespaceNotificationSize: snap.NamespaceNotificationSize,
		NamespaceAtQuota:          snap.NamespaceAtQuota,
		NamespaceConfirmedOver:    snap.NamespaceConfirmedOver,
		StaleAfterSeconds:         int64(s.Snapshots.StaleThreshold().Seconds()),
		ActivityWindowSeconds:     int64(snap.ActivityWindow.Seconds()),
	}
	if !snap.NamespaceUptodateTill.IsZero() {
		ut := snap.NamespaceUptodateTill.UTC()
		sum.NamespaceUptodateTill = &ut
		age := int64(s.now().UTC().Sub(ut).Seconds())
		sum.NamespaceMeteringAgeSeconds = &age
	}
	for _, b := range snap.Buckets {
		if b.Activity.Hot {
			sum.HotBuckets++
		}
	}
	return sum
}

func toBucket(b poller.BucketState) Bucket {
	var uptodate *time.Time
	if !b.UptodateTill.IsZero() {
		ut := b.UptodateTill.UTC()
		uptodate = &ut
	}
	var act *Activity
	if b.Activity.Known {
		act = &Activity{
			Known:             true,
			State:             ActivityState(b.Activity),
			Changed:           b.Activity.Changed,
			MeteringRefreshed: b.Activity.MeteringRefreshed,
			FreshnessKnown:    b.Activity.FreshnessKnown,
			WindowSeconds:     b.Activity.WindowSeconds,
			ObjectsDelta:      b.Activity.ObjectsDelta,
			BytesDelta:        b.Activity.BytesDelta,
			ObjectsPerHour:    b.Activity.ObjectsPerHour,
			BytesPerHour:      b.Activity.BytesPerHour,
			MPUParts:          b.Activity.MPUParts,
			MPUPartsDelta:     b.Activity.MPUPartsDelta,
			BusyHours:         b.Activity.BusyHours,
			ObservedHours:     b.Activity.ObservedHours,
			Hot:               b.Activity.Hot,
		}
	}
	return Bucket{
		Name:             b.Name,
		Namespace:        b.Namespace,
		UsedBytes:        b.UsedBytes,
		Objects:          b.Objects,
		MPUBytes:         b.MPUBytes,
		MPUParts:         b.MPUParts,
		BlockSize:        b.BlockSize,
		NotificationSize: b.NotificationSize,
		QuotaMode:        QuotaMode(b.BlockSize != nil, b.NotificationSize != nil),
		QuotaBytes:       b.QuotaBytes,
		UsedPercent:      b.UsedPercent,
		UptodateTill:     uptodate,
		Stale:            b.Stale,
		AtQuota:          b.AtQuota,
		OverStreak:       b.OverStreak,
		ConfirmedOver:    b.ConfirmedOver,
		Activity:         act,
	}
}

// ActivityState maps the three-way freshness evidence onto one label, so
// consumers never have to reason about boolean combinations.
func ActivityState(a poller.Activity) string {
	switch {
	case !a.Known:
		return "unknown"
	case a.Changed:
		return "moved"
	case a.MeteringRefreshed:
		return "idle" // ECS republished metering and nothing moved: provably quiet
	case a.FreshnessKnown:
		return "unrefreshed" // ECS has not republished since the baseline
	default:
		return "unchanged" // no uptodate_till: idle and unrefreshed look identical
	}
}

// QuotaMode derives the ECS-native quota mode from the two thresholds.
func QuotaMode(hasBlock, hasNotify bool) string {
	switch {
	case hasBlock && hasNotify:
		return "block-notify"
	case hasBlock:
		return "block-only"
	case hasNotify:
		return "notify-only"
	default:
		return "off"
	}
}
