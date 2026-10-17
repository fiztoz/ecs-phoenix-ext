package deploycheck

import (
	"regexp"
	"strings"
	"testing"
)

// TestActivityHourKeyIsNotATimestamp guards a MariaDB-only failure mode.
//
// With MariaDB's default explicit_defaults_for_timestamp=OFF, the first
// TIMESTAMP column of a table silently gains DEFAULT CURRENT_TIMESTAMP
// ON UPDATE CURRENT_TIMESTAMP unless it is declared NULL or given an explicit
// DEFAULT/ON UPDATE. ext_ecs_usage_activity_hourly.hour_bucket is a
// primary-key component that the additive upsert never re-assigns, so that
// implicit annotation would rewrite the key on every fold: counters would move
// into the current hour or fail on a duplicate key. SQLite does not care about
// the declared type, so only the MariaDB leg of the gate would ever show it.
func TestActivityHourKeyIsNotATimestamp(t *testing.T) {
	body := repoFile(t, "migrations/005_activity.up.sql")

	create := body[strings.Index(body, "CREATE TABLE IF NOT EXISTS ext_ecs_usage_activity_hourly"):]
	if end := strings.Index(create, ");"); end >= 0 {
		create = create[:end]
	}
	line := findColumn(t, create, "hour_bucket")
	if strings.Contains(strings.ToUpper(line), "TIMESTAMP") {
		t.Errorf("hour_bucket is declared %q; a first TIMESTAMP column gains an implicit "+
			"ON UPDATE CURRENT_TIMESTAMP on MariaDB and would rewrite the primary key", line)
	}
	if !strings.Contains(strings.ToUpper(line), "DATETIME") {
		t.Errorf("hour_bucket = %q, want an explicit DATETIME declaration", line)
	}
	// hour_bucket must stay the leading timestamp-ish column of the table; if a
	// TIMESTAMP column is ever added above it, the same trap reopens.
	firstTimeish := regexp.MustCompile(`(?im)^\s*\w+\s+(TIMESTAMP|DATETIME)\b`).FindString(create)
	if !strings.Contains(firstTimeish, "hour_bucket") {
		t.Errorf("hour_bucket is no longer the first time column (%q); re-check the MariaDB "+
			"explicit_defaults_for_timestamp behaviour for the new leading column", firstTimeish)
	}
}

func findColumn(t *testing.T, create, column string) string {
	t.Helper()
	for _, line := range strings.Split(create, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, column+" ") {
			return strings.TrimSuffix(trimmed, ",")
		}
	}
	t.Fatalf("column %s not found in the CREATE TABLE", column)
	return ""
}
