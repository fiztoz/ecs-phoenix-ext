package poller

import (
	"time"
)

// Activity is one bucket's derived write-side movement.
//
// ECS namespace billing reports no request counters, so nothing here is a
// measured read or write: it is the change in objects, bytes and in-flight
// multipart parts between two metering observations. A bucket serving heavy
// read traffic with no writes looks idle here, and a bucket that overwrites
// objects in place or deletes as fast as it writes can look idle too. Deltas
// are net, not gross.
type Activity struct {
	// Known reports that two comparable observations exist. It is false for
	// the first observation of a bucket and when no baseline could be
	// restored.
	Known bool
	// Changed reports that at least one counter moved since the baseline.
	Changed bool
	// MeteringRefreshed reports that ECS proved it republished metering: both
	// observations carried uptodate_till and it advanced.
	MeteringRefreshed bool
	// FreshnessKnown reports that uptodate_till was present on both sides, so
	// staleness is provable at all. Many ECS builds omit it per bucket; then
	// this is false and "nothing moved" cannot be told apart from "ECS has
	// not republished yet".
	FreshnessKnown bool
	// WindowSeconds is the elapsed time between the two observations.
	WindowSeconds float64

	ObjectsDelta   int64 // signed: positive ingest, negative delete/expire
	BytesDelta     int64 // signed
	ObjectsPerHour float64
	BytesPerHour   float64

	// MPUParts is the in-flight incomplete multipart part count at the latest
	// observation: non-zero means an upload was running, which billing size
	// alone does not show.
	MPUParts      int64
	MPUPartsDelta int64

	// BusyHours/ObservedHours cover the rolling lookback window: hours that
	// saw movement, over hours that saw at least one usable observation.
	// Equal counts mean every measured hour was busy; a low ObservedHours
	// means the window holds little evidence either way.
	BusyHours     int
	ObservedHours int
	// Hot is the "always busy" flag: movement in at least half of the
	// observed hours, with enough observed hours to mean something.
	Hot bool
}

// observation is the activity-relevant part of one billing row.
type observation struct {
	objects   int64
	usedBytes int64
	mpuParts  int64
	uptodate  time.Time
}

// baseline is the observation current rates are measured against. It only
// moves when the values are proven current as of that poll, so a run of polls
// that all return the same unrefreshed sample widens the window instead of
// charging several hours of ingest to the last few minutes.
type baseline struct {
	at        time.Time // when the baseline observation was made
	objects   int64
	usedBytes int64
	mpuParts  int64
	uptodate  time.Time
}

func (b baseline) usable() bool { return !b.at.IsZero() }

func (o observation) asBaseline(at time.Time) baseline {
	return baseline{at: at, objects: o.objects, usedBytes: o.usedBytes, mpuParts: o.mpuParts, uptodate: o.uptodate}
}

// computeActivity diffs one observation against its baseline. It never
// mutates anything and is safe to call when there is no baseline yet.
func computeActivity(base baseline, obs observation, now time.Time) Activity {
	act := Activity{MPUParts: obs.mpuParts}
	if !base.usable() {
		return act // nothing to compare against yet
	}
	elapsed := now.Sub(base.at)
	if elapsed <= 0 {
		return act // clock moved backwards or same instant: no rate is honest
	}
	act.Known = true
	act.WindowSeconds = elapsed.Seconds()
	act.ObjectsDelta = obs.objects - base.objects
	act.BytesDelta = obs.usedBytes - base.usedBytes
	act.MPUPartsDelta = obs.mpuParts - base.mpuParts
	act.FreshnessKnown = !base.uptodate.IsZero() && !obs.uptodate.IsZero()
	act.MeteringRefreshed = act.FreshnessKnown && obs.uptodate.After(base.uptodate)
	act.Changed = act.ObjectsDelta != 0 || act.BytesDelta != 0 || act.MPUPartsDelta != 0
	hours := elapsed.Hours()
	act.ObjectsPerHour = float64(act.ObjectsDelta) / hours
	act.BytesPerHour = float64(act.BytesDelta) / hours
	return act
}

// moving reports whether this observation saw any movement. An in-flight
// multipart upload counts as movement even when the completed-object counters
// have not changed yet.
func (a Activity) moving() bool {
	return a.Changed || a.MPUParts > 0
}

// proven reports whether the observed values are known to be current as of
// this poll, which is what allows the diff baseline to move: either ECS
// advanced uptodate_till, or a counter moved (which is itself proof of a
// republication). Without either, the baseline is held so the next real
// change is rated over the whole quiet stretch.
func (a Activity) proven() bool { return a.MeteringRefreshed || a.Changed }

// recordable reports whether this observation may be folded into the hourly
// history, i.e. whether it counts as a measured hour.
//
// When ECS publishes uptodate_till, a poll that provably returns the same
// metering sample carries no information and is skipped: recording it would
// inflate the denominator with hours nobody measured. When ECS omits it — the
// common case per bucket — staleness is unprovable, so every poll is an
// observation and moving() decides whether that hour was busy. Skipping those
// instead would make BusyHours equal ObservedHours by construction and flag
// every moving bucket as "always busy".
func (a Activity) recordable() bool {
	if !a.Known {
		return false
	}
	return a.MeteringRefreshed || a.Changed || !a.FreshnessKnown
}

const (
	// DefaultActivityWindow is the rolling lookback for BusyHours/ObservedHours.
	DefaultActivityWindow = 24 * time.Hour
	// DefaultActivityRetention is how long hourly rows are kept. 14 days of
	// hourly rows is 336 rows per bucket, so retention bounds the table
	// without needing per-poll rollups.
	DefaultActivityRetention = 14 * 24 * time.Hour
	// hotMinObservedHours is the evidence floor for the "always busy" flag:
	// three measured hours stops a fresh install from labelling every bucket
	// hot on its first samples.
	hotMinObservedHours = 3
)

// isHot reports the "always busy" flag: movement in at least half of the
// observed hours, once enough hours have been observed.
func isHot(busy, observed int) bool {
	return observed >= hotMinObservedHours && busy*2 >= observed
}
