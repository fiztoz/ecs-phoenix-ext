package store

import (
	"context"
	"database/sql"
	"fmt"
)

func validateObservation(obs Observation) error {
	if obs.Namespace == "" {
		return fmt.Errorf("store: observation namespace is required")
	}
	for _, r := range obs.Buckets {
		if r.Namespace != obs.Namespace {
			return fmt.Errorf("store: bucket %q namespace %q does not match %q", r.Bucket, r.Namespace, obs.Namespace)
		}
	}
	return nil
}

func replaceObservation(ctx context.Context, db *sql.DB, bucketSQL, namespaceSQL string, obs Observation) error {
	if err := validateObservation(obs); err != nil {
		return err
	}
	ns := obs.NamespaceState
	if ns.Namespace == "" {
		ns.Namespace = obs.Namespace
	}
	if ns.Namespace != obs.Namespace {
		return fmt.Errorf("store: namespace state %q does not match %q", ns.Namespace, obs.Namespace)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin observation replace: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM ext_ecs_usage_state WHERE namespace = ?`, obs.Namespace); err != nil {
		return fmt.Errorf("store: delete stale buckets for %s: %w", obs.Namespace, err)
	}
	for _, r := range obs.Buckets {
		blockBytes, blockKnown := bindLimit(r.Block)
		notifyBytes, notifyKnown := bindLimit(r.Notify)
		if _, err := tx.ExecContext(ctx, bucketSQL,
			r.Namespace, r.Bucket, r.UsedBytes, r.Objects, r.MPUBytes, r.MPUParts,
			bindNullUTC(r.UptodateTill),
			bindUTC(r.PolledAt), r.OverStreak, boolInt(r.ConfirmedOver), nullString(r.LastError),
			blockBytes, blockKnown, notifyBytes, notifyKnown,
		); err != nil {
			return fmt.Errorf("store: upsert state %s/%s: %w", r.Namespace, r.Bucket, err)
		}
	}
	defBytes, defKnown := bindLimit(ns.DefaultBlock)
	blockBytes, blockKnown := bindLimit(ns.Block)
	notifyBytes, notifyKnown := bindLimit(ns.Notify)
	if _, err := tx.ExecContext(ctx, namespaceSQL,
		ns.Namespace, ns.UsedBytes, ns.Objects, bindUTC(ns.PolledAt),
		ns.OverStreak, boolInt(ns.ConfirmedOver),
		defBytes, defKnown, blockBytes, blockKnown, notifyBytes, notifyKnown,
	); err != nil {
		return fmt.Errorf("store: upsert namespace %s: %w", ns.Namespace, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit observation replace: %w", err)
	}
	return nil
}

func bindLimit(l ObservedLimit) (any, int) {
	if !l.Known {
		return nil, 0
	}
	if l.Bytes == nil {
		return nil, 1
	}
	return *l.Bytes, 1
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func scanLimit(rawBytes any, knownRaw any) (ObservedLimit, error) {
	known, err := scanBool(knownRaw)
	if err != nil {
		return ObservedLimit{}, err
	}
	if !known {
		return ObservedLimit{}, nil
	}
	b, err := scanInt64Ptr(rawBytes)
	if err != nil {
		return ObservedLimit{}, err
	}
	return ObservedLimit{Known: true, Bytes: b}, nil
}

func scanBool(v any) (bool, error) {
	switch x := v.(type) {
	case int64:
		return x != 0, nil
	case int32:
		return x != 0, nil
	case int:
		return x != 0, nil
	case bool:
		return x, nil
	case []byte:
		return len(x) > 0 && string(x) != "0", nil
	case string:
		return x != "" && x != "0", nil
	default:
		return false, fmt.Errorf("store: unexpected bool column type %T", v)
	}
}

func scanInt64Ptr(v any) (*int64, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case int64:
		return &x, nil
	case int32:
		n := int64(x)
		return &n, nil
	case int:
		n := int64(x)
		return &n, nil
	case []byte:
		return parseInt64Ptr(string(x))
	case string:
		return parseInt64Ptr(x)
	default:
		return nil, fmt.Errorf("store: unexpected int column type %T", v)
	}
}

func parseInt64Ptr(s string) (*int64, error) {
	if s == "" {
		return nil, nil
	}
	var n int64
	if _, err := fmt.Sscan(s, &n); err != nil {
		return nil, fmt.Errorf("store: unparseable int column %q: %w", s, err)
	}
	return &n, nil
}
