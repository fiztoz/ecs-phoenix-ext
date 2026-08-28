// Package store persists quotas and durable per-bucket state in MariaDB or
// SQLite. All timestamps are normalised to UTC in Go before binding.
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

// QuotaRow is one operator-set quota.
type QuotaRow struct {
	Namespace  string
	Bucket     string
	QuotaBytes int64
	UpdatedAt  time.Time
}

// NamespaceQuotaRow is one operator-set namespace-level total quota.
type NamespaceQuotaRow struct {
	Namespace  string
	QuotaBytes int64
	UpdatedAt  time.Time
}

// Store is the persistence surface used by poller and HTTP layers.
type Store interface {
	// UpsertStates replaces the durable observation rows for the polled
	// namespace. It must not touch quota rows.
	UpsertStates(ctx context.Context, rows []StateRow) error
	// States returns all durable state rows (used to seed hysteresis after
	// a restart).
	States(ctx context.Context) ([]StateRow, error)
	// SetQuota inserts or replaces a quota (bytes only).
	SetQuota(ctx context.Context, namespace, bucket string, quotaBytes int64) error
	// DeleteQuota removes a quota row. Missing rows are not an error.
	DeleteQuota(ctx context.Context, namespace, bucket string) error
	// Quotas returns quotas for one namespace keyed by bucket name.
	Quotas(ctx context.Context, namespace string) (map[string]QuotaRow, error)
	// SetNamespaceQuota inserts or replaces a namespace-level total quota.
	SetNamespaceQuota(ctx context.Context, namespace string, quotaBytes int64) error
	// DeleteNamespaceQuota removes a namespace-level quota. Missing rows are not an error.
	DeleteNamespaceQuota(ctx context.Context, namespace string) error
	// NamespaceQuota returns the namespace-level quota, if set.
	NamespaceQuota(ctx context.Context, namespace string) (*NamespaceQuotaRow, error)
	// Migrate applies this repo's SQL on start.
	Migrate(ctx context.Context) error
	Close() error
}
