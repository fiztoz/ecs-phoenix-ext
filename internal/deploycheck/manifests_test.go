package deploycheck

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", rel)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStandaloneNetworkPolicyAllowsMariaDBOnlyNarrowly(t *testing.T) {
	body := repoFile(t, "deploy/manifests/networkpolicy.yaml")
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "0.0.0.0/0") {
			t.Fatal("egress must not allow every destination")
		}
	}
	if !strings.Contains(body, "app.kubernetes.io/component: mariadb") {
		t.Fatal("in-cluster MariaDB selector missing")
	}
	if !strings.Contains(body, "port: 3306") {
		t.Fatal("MariaDB port missing")
	}
	if !strings.Contains(body, "port: 4443") {
		t.Fatal("ECS management port missing")
	}
	if !strings.Contains(body, "203.0.113.10/32") || !strings.Contains(body, "port: 3307") {
		t.Fatal("external database / nondefault port example missing")
	}
}

func TestReadinessProbeIsServingNotDependencyHealth(t *testing.T) {
	body := repoFile(t, "deploy/manifests/deployment.yaml")
	if !strings.Contains(body, "path: /storage/health/live") {
		t.Fatal("readiness probe must use the serving live endpoint")
	}
	if strings.Contains(body, "path: /storage/health/ready") {
		t.Fatal("readiness probe still uses ECS dependency health")
	}
	readme := repoFile(t, "README.md")
	if !strings.Contains(readme, "readinessPath: /health/live") {
		t.Fatal("Phoenix readinessPath override not documented")
	}
}

func TestGrantsDoNotUseTablePrefixWildcard(t *testing.T) {
	for _, rel := range []string{"README.md", "deploy/grants.sql"} {
		for _, line := range strings.Split(repoFile(t, rel), "\n") {
			upper := strings.ToUpper(line)
			if strings.Contains(upper, "GRANT ") && strings.Contains(line, "%") && strings.Contains(line, "ext_ecs_usage") {
				t.Fatalf("%s still documents an invalid table-prefix grant: %s", rel, line)
			}
		}
	}
	grants := repoFile(t, "deploy/grants.sql")
	if !strings.Contains(grants, "ON ecs_usage.*") {
		t.Fatal("dedicated schema grant missing")
	}
	if !strings.Contains(grants, "ext_ecs_usage_namespace_state") {
		t.Fatal("maintenance list must include tables added by later migrations")
	}
	if strings.Contains(grants, "ON phoenix.*") {
		t.Fatal("must not grant the shared Phoenix schema")
	}
}
