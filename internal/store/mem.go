package store

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Mem is an in-memory Store used by tests. Not for production use.
type Mem struct {
	mu     sync.Mutex
	states map[string]StateRow
	ns     map[string]NamespaceState
	hours  map[string]ActivityHour
}

// NewMem builds an empty in-memory store.
func NewMem() *Mem {
	return &Mem{
		states: map[string]StateRow{},
		ns:     map[string]NamespaceState{},
		hours:  map[string]ActivityHour{},
	}
}

func key(ns, b string) string { return ns + "\x00" + b }

func hourKey(ns, b string, hour time.Time) string {
	return ns + "\x00" + b + "\x00" + hour.UTC().Format(time.RFC3339)
}

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

func (m *Mem) RecordActivity(_ context.Context, deltas []ActivityDelta) error {
	if len(deltas) == 0 {
		return nil
	}
	if err := validateDeltas(deltas); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range deltas {
		hour := HourBucket(d.HourBucket)
		k := hourKey(d.Namespace, d.Bucket, hour)
		row := m.hours[k]
		if row.HourBucket.IsZero() {
			row = ActivityHour{Namespace: d.Namespace, Bucket: d.Bucket, HourBucket: hour}
		}
		row.Samples++
		if d.Active {
			row.ActiveSamples++
		}
		row.ObjectsUp += d.ObjectsUp
		row.ObjectsDown += d.ObjectsDown
		row.BytesUp += d.BytesUp
		row.BytesDown += d.BytesDown
		if d.MPUPartsMax > row.MPUPartsMax {
			row.MPUPartsMax = d.MPUPartsMax
		}
		m.hours[k] = row
	}
	return nil
}

func (m *Mem) ActivityHours(_ context.Context, namespace string, since time.Time) ([]ActivityHour, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	since = since.UTC()
	var out []ActivityHour
	for _, r := range m.hours {
		if r.Namespace != namespace || r.HourBucket.Before(since) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bucket != out[j].Bucket {
			return out[i].Bucket < out[j].Bucket
		}
		return out[i].HourBucket.Before(out[j].HourBucket)
	})
	return out, nil
}

func (m *Mem) PruneActivity(_ context.Context, cutoff time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff = cutoff.UTC()
	var n int64
	for k, r := range m.hours {
		if r.HourBucket.Before(cutoff) {
			delete(m.hours, k)
			n++
		}
	}
	return n, nil
}

func (m *Mem) Migrate(context.Context) error { return nil }
func (m *Mem) Close() error                  { return nil }
