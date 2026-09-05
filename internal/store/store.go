// Package store persists durable per-bucket observation state (for hysteresis
// across restarts) in MariaDB or SQLite. All timestamps are normalised to UTC
// in Go before binding. Quotas are ECS-native and never persisted here.
package store

import (
	"context"
	"time"
)

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
}

// Store is the persistence surface used by poller and HTTP layers.
type Store interface {
	// UpsertStates replaces the durable observation rows for the polled
	// namespace. It must not touch quota rows.
	UpsertStates(ctx context.Context, rows []StateRow) error
	// States returns all durable state rows (used to seed hysteresis after
	// a restart).
	States(ctx context.Context) ([]StateRow, error)
	// Migrate applies this repo's SQL on start.
	Migrate(ctx context.Context) error
	Close() error
}
