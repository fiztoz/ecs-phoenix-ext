// Package store persists durable observation state (hysteresis and the last
// ECS-reported thresholds) plus derived activity history in MariaDB or
// SQLite. All timestamps are normalised to UTC in Go before binding.
//
// Operator-set quotas are not stored. Threshold columns are a cache of the
// last ECS inventory observation so a restart can tell an explicit quota-off
// from inventory that has not been seen yet.
//
// Activity history is derived, not measured: ECS namespace billing reports no
// request counters, so the hourly rows record the change in objects/bytes/MPU
// parts between two metering observations. Read traffic never appears in it.
package store

import (
	"context"
	"time"
)

// ObservedLimit is a cached ECS threshold.
//
// Known false means inventory has not reported the field (missing, not
// unlimited). Known true and Bytes nil means ECS explicitly reported the
// threshold unset (quota-off).
type ObservedLimit struct {
	Known bool
	Bytes *int64
}

// Observed copies a threshold into an ObservedLimit. The pointer is copied
// so later mutation of the caller's value cannot change the stored one.
func Observed(known bool, bytes *int64) ObservedLimit {
	if !known {
		return ObservedLimit{}
	}
	if bytes == nil {
		return ObservedLimit{Known: true}
	}
	v := *bytes
	return ObservedLimit{Known: true, Bytes: &v}
}

// StateRow is one durable bucket observation plus hysteresis counters.
type StateRow struct {
	Namespace     string
	Bucket        string
	UsedBytes     int64
	Objects       int64
	MPUBytes      int64
	MPUParts      int64      // in-flight incomplete multipart parts at observation time
	UptodateTill  *time.Time // nil when ECS omitted it
	PolledAt      time.Time
	OverStreak    int
	ConfirmedOver bool
	LastError     string
	Block         ObservedLimit
	Notify        ObservedLimit
}

// NamespaceState is the durable namespace-level observation.
type NamespaceState struct {
	Namespace     string
	UsedBytes     int64
	Objects       int64
	PolledAt      time.Time
	OverStreak    int
	ConfirmedOver bool
	DefaultBlock  ObservedLimit
	Block         ObservedLimit
	Notify        ObservedLimit
}

// Observation is one complete successful billing snapshot for a namespace.
// Buckets may be empty. Callers must not build this from a failed or
// incomplete poll.
type Observation struct {
	Namespace      string
	NamespaceState NamespaceState
	Buckets        []StateRow
}

// ActivityDelta is one bucket's movement observed in one poll, folded into
// the hour containing HourBucket. Ups and downs are kept separate so a
// bucket that writes and deletes at the same rate is visibly churning
// instead of looking idle.
type ActivityDelta struct {
	Namespace   string
	Bucket      string
	HourBucket  time.Time // observation time truncated to the hour, UTC
	ObjectsUp   int64
	ObjectsDown int64
	BytesUp     int64
	BytesDown   int64
	MPUPartsMax int64
	// Active reports movement in this observation. Only deltas taken between
	// two metering observations should be recorded at all; callers must not
	// fold a repeated, unrefreshed metering sample in as inactive.
	Active bool
}

// ActivityHour is one rolled-up hour of derived activity for one bucket.
type ActivityHour struct {
	Namespace     string
	Bucket        string
	HourBucket    time.Time
	Samples       int
	ActiveSamples int
	ObjectsUp     int64
	ObjectsDown   int64
	BytesUp       int64
	BytesDown     int64
	MPUPartsMax   int64
}

// Store is the persistence surface used by poller and HTTP layers.
type Store interface {
	// ReplaceObservation atomically replaces one namespace's bucket rows with
	// obs.Buckets (deleting buckets absent from that complete snapshot) and
	// upserts the namespace observation. Other namespaces are untouched.
	// An empty bucket slice is a valid complete snapshot. Do not call this
	// for a failed or incomplete poll.
	ReplaceObservation(ctx context.Context, obs Observation) error
	// States returns all durable bucket rows (used to seed hysteresis after
	// a restart).
	States(ctx context.Context) ([]StateRow, error)
	// NamespaceStates returns durable namespace observations.
	NamespaceStates(ctx context.Context) ([]NamespaceState, error)
	// RecordActivity folds one poll's per-bucket deltas into hourly rollup
	// rows. It is additive and independent of ReplaceObservation: recording
	// never touches bucket state, and an empty slice is a no-op.
	RecordActivity(ctx context.Context, deltas []ActivityDelta) error
	// ActivityHours returns hourly rollups for one namespace with an hour
	// bucket at or after since, oldest first.
	ActivityHours(ctx context.Context, namespace string, since time.Time) ([]ActivityHour, error)
	// PruneActivity deletes hourly rollups whose hour bucket is strictly
	// before cutoff and reports how many rows went away.
	PruneActivity(ctx context.Context, cutoff time.Time) (int64, error)
	// Migrate applies this repo's SQL on start.
	Migrate(ctx context.Context) error
	Close() error
}
