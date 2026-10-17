package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// HourBucket truncates t to the top of its UTC hour. Activity rows are keyed
// by this, so every poll inside one hour folds into a single row per bucket
// and the lookback query stays bounded by hours, not by poll frequency.
func HourBucket(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), 0, 0, 0, time.UTC)
}

func validateDeltas(deltas []ActivityDelta) error {
	for _, d := range deltas {
		if d.Namespace == "" {
			return fmt.Errorf("store: activity delta for bucket %q has no namespace", d.Bucket)
		}
		if d.Bucket == "" {
			return fmt.Errorf("store: activity delta in namespace %q has no bucket", d.Namespace)
		}
		if d.HourBucket.IsZero() {
			return fmt.Errorf("store: activity delta %s/%s has no hour bucket", d.Namespace, d.Bucket)
		}
	}
	return nil
}

// recordActivity folds deltas into hourly rows inside one transaction. The
// upsert is additive (counts summed, mpu_parts_max raised), so re-recording
// the same poll after a restart-free retry inflates counters rather than
// duplicating rows; callers record at most once per successful poll.
func recordActivity(ctx context.Context, db *sql.DB, upsertSQL string, deltas []ActivityDelta) error {
	if len(deltas) == 0 {
		return nil
	}
	if err := validateDeltas(deltas); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin activity record: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, d := range deltas {
		active := 0
		if d.Active {
			active = 1
		}
		if _, err := tx.ExecContext(ctx, upsertSQL,
			d.Namespace, d.Bucket, bindUTC(HourBucket(d.HourBucket)),
			1, active,
			d.ObjectsUp, d.ObjectsDown, d.BytesUp, d.BytesDown, d.MPUPartsMax,
		); err != nil {
			return fmt.Errorf("store: upsert activity %s/%s: %w", d.Namespace, d.Bucket, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit activity record: %w", err)
	}
	return nil
}

const selectActivityHours = `
SELECT namespace, bucket, hour_bucket, samples, active_samples,
       objects_up, objects_down, bytes_up, bytes_down, mpu_parts_max
FROM ext_ecs_usage_activity_hourly
WHERE namespace = ? AND hour_bucket >= ?
ORDER BY bucket, hour_bucket`

func queryActivityHours(ctx context.Context, db *sql.DB, namespace string, since time.Time) ([]ActivityHour, error) {
	rows, err := db.QueryContext(ctx, selectActivityHours, namespace, bindUTC(since))
	if err != nil {
		return nil, fmt.Errorf("store: query activity hours: %w", err)
	}
	defer rows.Close()
	var out []ActivityHour
	for rows.Next() {
		var r ActivityHour
		var hour any
		if err := rows.Scan(&r.Namespace, &r.Bucket, &hour, &r.Samples, &r.ActiveSamples,
			&r.ObjectsUp, &r.ObjectsDown, &r.BytesUp, &r.BytesDown, &r.MPUPartsMax); err != nil {
			return nil, fmt.Errorf("store: scan activity hour: %w", err)
		}
		hb, err := scanTime(hour)
		if err != nil {
			return nil, err
		}
		if hb == nil {
			return nil, fmt.Errorf("store: activity row %s/%s has no hour bucket", r.Namespace, r.Bucket)
		}
		r.HourBucket = *hb
		out = append(out, r)
	}
	return out, rows.Err()
}

const deleteActivityHours = `DELETE FROM ext_ecs_usage_activity_hourly WHERE hour_bucket < ?`

func pruneActivity(ctx context.Context, db *sql.DB, cutoff time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, deleteActivityHours, bindUTC(cutoff))
	if err != nil {
		return 0, fmt.Errorf("store: prune activity: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: prune activity count: %w", err)
	}
	return n, nil
}

// SummarizeHours folds hourly rows into per-bucket window counters.
// BusyHours counts hours with at least one observation that saw movement;
// ObservedHours counts hours with at least one usable metering observation, so
// a bucket that was genuinely quiet is distinguishable from a bucket nobody
// measured.
func SummarizeHours(rows []ActivityHour) map[string]ActivityWindowSummary {
	out := make(map[string]ActivityWindowSummary, len(rows))
	for _, r := range rows {
		s := out[r.Bucket]
		s.ObservedHours++
		if r.ActiveSamples > 0 {
			s.BusyHours++
		}
		s.ObjectsUp += r.ObjectsUp
		s.ObjectsDown += r.ObjectsDown
		s.BytesUp += r.BytesUp
		s.BytesDown += r.BytesDown
		if r.MPUPartsMax > s.MPUPartsMax {
			s.MPUPartsMax = r.MPUPartsMax
		}
		out[r.Bucket] = s
	}
	return out
}

// ActivityWindowSummary is one bucket's derived activity over a lookback
// window, folded from hourly rows.
type ActivityWindowSummary struct {
	BusyHours     int
	ObservedHours int
	ObjectsUp     int64
	ObjectsDown   int64
	BytesUp       int64
	BytesDown     int64
	MPUPartsMax   int64
}
