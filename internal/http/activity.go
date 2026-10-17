package http

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/fiztoz/ecs-phoenix-ext/internal/poller"
)

// activityView is the pre-formatted derived activity for one bucket row.
//
// Everything here is computed from ECS billing deltas. ECS exposes no request
// counters to a namespace-scoped management user, so these numbers describe
// write-side movement only: a bucket serving heavy read traffic with no
// writes shows as idle, and so does a bucket that overwrites in place or
// deletes as fast as it writes. The UI says so rather than implying a
// measured request rate.
type activityView struct {
	// Known is false until two comparable metering observations exist, so a
	// fresh install or a brand-new bucket renders "—" instead of "idle".
	Known bool
	// State says what the numbers mean, because "nothing moved" has three
	// different causes and only one of them is idleness:
	//   moved       a counter changed since the baseline
	//   idle        ECS republished metering and nothing moved
	//   unrefreshed ECS provably has not republished since the baseline
	//   unchanged   ECS omits uptodate_till, so idle and unrefreshed cannot be
	//               told apart
	//   unknown     no comparable baseline yet
	State             string
	Changed           bool
	MeteringRefreshed bool
	FreshnessKnown    bool
	ObjectsRate       string // "+12.3k obj/h" / "-40 obj/h" / "0 obj/h"
	BytesRate         string // "+3.40 GiB/h"
	ObjectsPerHour    float64
	BytesPerHour      float64
	BusyHours         int
	ObservedHours     int
	Busy              string // "4/6h" — movement in N of M measured hours
	Hot               bool   // "always busy": movement in most measured hours
	MPUParts          int64  // in-flight incomplete multipart parts
	HasMPU            bool
	// Score sorts the Activity column: absolute objects per hour, or -1 when
	// unknown so unmeasured buckets sort last in both directions.
	Score float64
	// BusyScore sorts the Busy column, or -1 when the window holds no
	// measured hour yet.
	BusyScore int
	// Window is the rolling lookback label, e.g. "24h0m".
	Window string
}

// activityState maps the three-way freshness evidence onto one label, so
// templates never have to reason about boolean combinations.
func activityState(a poller.Activity) string {
	switch {
	case !a.Known:
		return "unknown"
	case a.Changed:
		return "moved"
	case a.MeteringRefreshed:
		return "idle" // ECS republished metering and nothing moved: provably quiet
	case a.FreshnessKnown:
		return "unrefreshed" // ECS has not republished since the baseline
	default:
		return "unchanged" // no uptodate_till: idle and unrefreshed look identical
	}
}

// toActivityView formats one bucket's derived activity for templates.
func toActivityView(a poller.Activity, window time.Duration) activityView {
	v := activityView{
		Known:             a.Known,
		State:             activityState(a),
		Changed:           a.Changed,
		MeteringRefreshed: a.MeteringRefreshed,
		FreshnessKnown:    a.FreshnessKnown,
		ObjectsPerHour:    a.ObjectsPerHour,
		BytesPerHour:      a.BytesPerHour,
		BusyHours:         a.BusyHours,
		ObservedHours:     a.ObservedHours,
		Hot:               a.Hot,
		MPUParts:          a.MPUParts,
		HasMPU:            a.MPUParts > 0,
		Score:             -1,
		BusyScore:         -1,
		Window:            shortDuration(window),
		ObjectsRate:       "—",
		BytesRate:         "—",
		Busy:              "—",
	}
	if a.Known {
		v.ObjectsRate = humanObjectsRate(a.ObjectsPerHour)
		v.BytesRate = humanBytesRate(a.BytesPerHour)
		v.Score = math.Abs(a.ObjectsPerHour)
	}
	if a.ObservedHours > 0 {
		v.Busy = fmt.Sprintf("%d/%dh", a.BusyHours, a.ObservedHours)
		v.BusyScore = a.BusyHours
	}
	return v
}

// shortDuration renders a window as "24h", "90m" or "45s" — no "24h0m0s".
func shortDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "—"
	case d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	default:
		return strconv.Itoa(int(d/time.Second)) + "s"
	}
}

// humanObjectsRate renders a signed object rate. Counts are dimensionless, so
// the suffixes are decimal (k/M/G), unlike bytes.
func humanObjectsRate(v float64) string {
	return sign(v) + compactCount(v) + " obj/h"
}

// humanBytesRate renders a signed byte rate with the binary units the rest of
// the UI uses.
func humanBytesRate(v float64) string {
	return sign(v) + HumanBytes(int64(math.Round(math.Abs(v)))) + "/h"
}

// sign returns the explicit sign for a rate. It must be applied separately
// because both formatters below render magnitudes: a shrinking bucket would
// otherwise lose its minus sign and read as ingest.
func sign(v float64) string {
	switch {
	case v > 0:
		return "+"
	case v < 0:
		return "-"
	default:
		return ""
	}
}

func compactCount(v float64) string {
	a := math.Abs(v)
	switch {
	case a >= 1e9:
		return strconv.FormatFloat(a/1e9, 'f', 2, 64) + "G"
	case a >= 1e6:
		return strconv.FormatFloat(a/1e6, 'f', 2, 64) + "M"
	case a >= 1e3:
		return strconv.FormatFloat(a/1e3, 'f', 1, 64) + "k"
	case a >= 10:
		return strconv.FormatFloat(a, 'f', 0, 64)
	default:
		return strconv.FormatFloat(a, 'f', 1, 64)
	}
}
