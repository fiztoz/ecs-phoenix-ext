package store

import (
	"context"
	"sync"
)

// Mem is an in-memory Store used by tests. Not for production use.
type Mem struct {
	mu     sync.Mutex
	states map[string]StateRow
	ns     map[string]NamespaceState
}

// NewMem builds an empty in-memory store.
func NewMem() *Mem {
	return &Mem{
		states: map[string]StateRow{},
		ns:     map[string]NamespaceState{},
	}
}

func key(ns, b string) string { return ns + "\x00" + b }

func (m *Mem) ReplaceObservation(_ context.Context, obs Observation) error {
	if err := validateObservation(obs); err != nil {
		return err
	}
	ns := obs.NamespaceState
	if ns.Namespace == "" {
		ns.Namespace = obs.Namespace
	}
	if ns.Namespace != obs.Namespace {
		return errNamespaceMismatch(ns.Namespace, obs.Namespace)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, row := range m.states {
		if row.Namespace == obs.Namespace {
			delete(m.states, k)
		}
	}
	for _, r := range obs.Buckets {
		m.states[key(r.Namespace, r.Bucket)] = r
	}
	m.ns[obs.Namespace] = ns
	return nil
}

func errNamespaceMismatch(got, want string) error {
	return &namespaceMismatch{got: got, want: want}
}

type namespaceMismatch struct{ got, want string }

func (e *namespaceMismatch) Error() string {
	return "store: namespace state " + e.got + " does not match " + e.want
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

func (m *Mem) NamespaceStates(_ context.Context) ([]NamespaceState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]NamespaceState, 0, len(m.ns))
	for _, r := range m.ns {
		out = append(out, r)
	}
	return out, nil
}

func (m *Mem) Migrate(context.Context) error { return nil }
func (m *Mem) Close() error                  { return nil }
