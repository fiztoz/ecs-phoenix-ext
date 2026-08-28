package store

import (
	"context"
	"sync"
	"time"
)

// Mem is an in-memory Store used by tests. Not for production use.
type Mem struct {
	mu     sync.Mutex
	states map[string]StateRow
	quotas map[string]QuotaRow
	nsQuota *NamespaceQuotaRow
}

// NewMem builds an empty in-memory store.
func NewMem() *Mem {
	return &Mem{
		states: map[string]StateRow{},
		quotas: map[string]QuotaRow{},
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

func (m *Mem) SetQuota(_ context.Context, namespace, bucket string, quotaBytes int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quotas[key(namespace, bucket)] = QuotaRow{
		Namespace: namespace, Bucket: bucket, QuotaBytes: quotaBytes,
		UpdatedAt: time.Now().UTC(),
	}
	return nil
}

func (m *Mem) DeleteQuota(_ context.Context, namespace, bucket string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.quotas, key(namespace, bucket))
	return nil
}

func (m *Mem) Quotas(_ context.Context, namespace string) (map[string]QuotaRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]QuotaRow{}
	for _, q := range m.quotas {
		if q.Namespace == namespace {
			out[q.Bucket] = q
		}
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
