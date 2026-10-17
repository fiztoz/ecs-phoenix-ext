package deploycheck

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var createTableRe = regexp.MustCompile(`(?i)CREATE TABLE(?: IF NOT EXISTS)?\s+([A-Za-z0-9_]+)`)

// TestGrantsListEveryMigratedTable pins the README rule: a deployment that
// cannot use a dedicated schema grants each extension table explicitly, so
// every table a migration creates must appear in deploy/grants.sql. Without
// this, a new table ships and the extension starts failing on INSERT for
// installations that followed the explicit-grant path.
func TestGrantsListEveryMigratedTable(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..")
	migrations, err := filepath.Glob(filepath.Join(root, "migrations", "*.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) == 0 {
		t.Fatal("no up migrations found; the glob is wrong")
	}
	grants := repoFile(t, "deploy/grants.sql")

	seen := map[string]bool{}
	for _, path := range migrations {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range createTableRe.FindAllStringSubmatch(string(body), -1) {
			table := m[1]
			seen[table] = true
			if !strings.Contains(grants, table) {
				t.Errorf("%s creates %s, but deploy/grants.sql does not list it: "+
					"installations without a dedicated schema would be denied access",
					filepath.Base(path), table)
			}
		}
	}
	// Guard the guard: the migrations really do create the extension tables.
	for _, want := range []string{"ext_ecs_usage_state", "ext_ecs_usage_activity_hourly"} {
		if !seen[want] {
			t.Errorf("migrations no longer create %s; update this test", want)
		}
	}
}
