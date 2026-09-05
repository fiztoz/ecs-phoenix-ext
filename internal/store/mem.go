package store

import (
	"context"
	"sync"
	"time"
)

// Mem is an in-memory Store used by tests. Not for production use.
type Mem struct {
	mu      sync.Mutex
	states  map[string]StateRow
	nsQuota *NamespaceQuotaRow
}

// NewMem builds an empty in-memory store.
func NewMem() *Mem {
	return &Mem{
		states: map[string]StateRow{},
	}
}

func key(ns, b string) string { return ns + "\x00" + b }

func (m *Mem) UpsertStates(_ context.Context, rows []StateRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range rows {
		m.states[key(r.Namespace, r.Bucket)] = r
	}
	return nil
}

func (m *Mem) States(_ context.Context) ([]StateRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]StateRow, 0, len(m.states))
	for _, r := range m.states {
		out = append(out, r)
	}
	return out, nil
}

func (m *Mem) SetNamespaceQuota(_ context.Context, namespace string, quotaBytes int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nsQuota = &NamespaceQuotaRow{
		Namespace: namespace, QuotaBytes: quotaBytes,
		UpdatedAt: time.Now().UTC(),
	}
	return nil
}

func (m *Mem) DeleteNamespaceQuota(_ context.Context, namespace string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nsQuota != nil && m.nsQuota.Namespace == namespace {
		m.nsQuota = nil
	}
	return nil
}

func (m *Mem) NamespaceQuota(_ context.Context, namespace string) (*NamespaceQuotaRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nsQuota != nil && m.nsQuota.Namespace == namespace {
		cp := *m.nsQuota
		return &cp, nil
	}
	return nil, nil
}

func (m *Mem) Migrate(context.Context) error { return nil }
func (m *Mem) Close() error                  { return nil }
