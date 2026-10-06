// Package store persists durable observation state (hysteresis and the last
// ECS-reported thresholds) in MariaDB or SQLite. All timestamps are normalised
// to UTC in Go before binding.
//
// Operator-set quotas are not stored. Threshold columns are a cache of the
// last ECS inventory observation so a restart can tell an explicit quota-off
// from inventory that has not been seen yet.
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
	// Migrate applies this repo's SQL on start.
	Migrate(ctx context.Context) error
	Close() error
}
