// Package quota implements the over-quota state machine: two consecutive
// successful samples above quota confirm an alarm. One noisy sample never
// pages.
package quota

// Apply advances the hysteresis for one successful sample.
//
//	used > quota  → streak++
//	used <= quota → streak = 0
//	confirmed     = streak >= 2
//
// A nil quota means the bucket cannot be over; the streak resets.
func Apply(streak int, quotaBytes *int64, usedBytes int64) (newStreak int, confirmed bool) {
	if quotaBytes == nil {
		return 0, false
	}
	if usedBytes > *quotaBytes {
		streak++
	} else {
		streak = 0
	}
	return streak, streak >= 2
}
