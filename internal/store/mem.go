package store

import (
	"context"
	"sync"
)

// Mem is an in-memory Store used by tests. Not for production use.
type Mem struct {
	mu     sync.Mutex
	states map[string]StateRow
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

func (m *Mem) Migrate(context.Context) error { return nil }
func (m *Mem) Close() error                  { return nil }
