// Package quota implements the over-quota state machine: two consecutive
// successful samples above quota confirm an alarm. One noisy sample never
// pages.
package quota

// Apply advances the hysteresis for one successful sample.
//
//	used >= quota → streak++  (ECS blocks writes once the quota is reached)
//	used <  quota → streak = 0
//	confirmed     = streak >= 2
//
// A nil quota means the bucket cannot be over; the streak resets.
func Apply(streak int, quotaBytes *int64, usedBytes int64) (newStreak int, confirmed bool) {
	if quotaBytes == nil {
		return 0, false
	}
	if usedBytes >= *quotaBytes {
		streak++
	} else {
		streak = 0
	}
	return streak, streak >= 2
}

// Advance applies one sample without treating an unknown threshold as unlimited.
//
// known false means the threshold was not observed and no cached observation
// exists. The previous streak and confirmation are retained.
//
// authoritative means this sample's threshold came from a successful inventory
// response. An explicit quota-off is authoritative and known with a nil quota,
// and clears the alarm. A non-authoritative sample may advance a streak against
// a cached threshold, but it must not clear an already-confirmed alarm.
func Advance(prevStreak int, prevConfirmed, authoritative, known bool, quotaBytes *int64, usedBytes int64) (int, bool) {
	if !known {
		return prevStreak, prevConfirmed
	}
	streak, confirmed := Apply(prevStreak, quotaBytes, usedBytes)
	if !authoritative && prevConfirmed && !confirmed {
		return prevStreak, true
	}
	return streak, confirmed
}
