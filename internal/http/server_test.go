package http

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/ecs-phoenix-ext/internal/poller"
	"github.com/fiztoz/ecs-phoenix-ext/internal/store"
)

type fakeSnap struct {
	snap poller.Snapshot
	th   time.Duration
}

func (f *fakeSnap) Snapshot() poller.Snapshot     { return f.snap }
func (f *fakeSnap) StaleThreshold() time.Duration { return f.th }

func newTestServer(t *testing.T, snap poller.Snapshot, mem *store.Mem, ui string) *Server {
	t.Helper()
	s, err := New(Deps{
		Namespace: "prod-ns",
		BasePath:  "/",
		UIToken:   ui,
		Snapshots: &fakeSnap{snap: snap, th: time.Hour},
		Store:     mem,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func goodSnapshot() poller.Snapshot {
	q := int64(107374182400)
	pct := 11.0
	return poller.Snapshot{
		Namespace:        "prod-ns",
		PolledAt:         time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC),
		PollOK:           true,
		NamespaceBytes:   11811160064,
		NamespaceObjects: 2,
		Buckets: []poller.BucketState{{
			Name:         "sample-bkt",
			Namespace:    "prod-ns",
			UsedBytes:    11811160064,
			Objects:      2,
			QuotaBytes:   &q,
			UsedPercent:  &pct,
			SampleTime:   time.Date(2026, 8, 18, 9, 55, 0, 0, time.UTC),
			UptodateTill: time.Date(2026, 8, 18, 9, 50, 0, 0, time.UTC),
		}},
	}
}

func TestAPIBucketsWireShape(t *testing.T) {
	mem := store.NewMem()
	s := newTestServer(t, goodSnapshot(), mem, "")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/buckets", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}

	raw := rr.Body.Bytes()
	lower := strings.ToLower(string(raw))
	for _, banned := range []string{"\"token\"", "\"password\"", "x-sds-auth-token", "dsn"} {
		if strings.Contains(lower, banned) {
			t.Fatalf("wire body leaks %q: %s", banned, string(raw))
		}
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, key := range []string{"namespace", "polled_at", "poll_ok", "last_error",
		"namespace_used_bytes", "namespace_objects", "stale_after_seconds", "buckets"} {
		if _, ok := body[key]; !ok {
			t.Errorf("missing snake_case key %q", key)
		}
	}
	buckets := body["buckets"].([]any)
	b0 := buckets[0].(map[string]any)
	for _, key := range []string{"name", "used_bytes", "objects", "mpu_bytes", "quota_bytes",
		"used_percent", "sample_time", "uptodate_till", "stale", "over_streak", "confirmed_over"} {
		if _, ok := b0[key]; !ok {
			t.Errorf("bucket missing snake_case key %q", key)
		}
	}
	if b0["used_bytes"].(float64) != 11811160064 {
		t.Errorf("used_bytes = %v", b0["used_bytes"])
	}

	// quota_bytes must be null when unset
	snap := goodSnapshot()
	snap.Buckets[0].QuotaBytes = nil
	snap.Buckets[0].UsedPercent = nil
	s2 := newTestServer(t, snap, mem, "")
	rr2 := httptest.NewRecorder()
	s2.Handler().ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/api/buckets", nil))
	var body2 map[string]any
	_ = json.Unmarshal(rr2.Body.Bytes(), &body2)
	b2 := body2["buckets"].([]any)[0].(map[string]any)
	if b2["quota_bytes"] != nil {
		t.Fatalf("quota_bytes must be null when unset, got %v", b2["quota_bytes"])
	}
}

func TestCSPHeader(t *testing.T) {
	s := newTestServer(t, goodSnapshot(), store.NewMem(), "")
	for _, path := range []string{"/", "/api/buckets", "/wallboard", "/health/ready"} {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		csp := rr.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "frame-ancestors 'self'") {
			t.Errorf("%s: CSP = %q, want frame-ancestors 'self'", path, csp)
		}
		if rr.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing nosniff", path)
		}
	}
}

func TestHealthReady(t *testing.T) {
	ok := goodSnapshot()
	fail := goodSnapshot()
	fail.PollOK = false
	fail.LastError = "ecs: login returned HTTP 503"

	s := newTestServer(t, ok, store.NewMem(), "")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("ready with good poll = %d, want 200", rr.Code)
	}

	s = newTestServer(t, fail, store.NewMem(), "")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready after poll failure = %d, want 503", rr.Code)
	}
}

func TestHealthQuota(t *testing.T) {
	over := goodSnapshot()
	over.Buckets[0].ConfirmedOver = true
	over.Buckets[0].OverStreak = 2

	s := newTestServer(t, goodSnapshot(), store.NewMem(), "")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health/quota", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("quota all under = %d, want 200", rr.Code)
	}

	s = newTestServer(t, over, store.NewMem(), "")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health/quota", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("confirmed over = %d, want 503", rr.Code)
	}

	// Per bucket.
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health/quota/sample-bkt", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("confirmed over bucket = %d, want 503", rr.Code)
	}
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health/quota/ghost-bkt", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown bucket = %d, want 404", rr.Code)
	}
}

func TestUITokenGuard(t *testing.T) {
	s := newTestServer(t, goodSnapshot(), store.NewMem(), "test-ui-value")

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/buckets", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated API = %d, want 401", rr.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/buckets", nil)
	req.Header.Set("Authorization", "Bearer test-ui-value")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("authenticated API = %d, want 200", rr.Code)
	}

	// Health stays open — Phoenix monitors carry no Bearer credential.
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("health must stay open, got %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/icon.svg", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("icon must stay open, got %d", rr.Code)
	}
}

func TestQuotaAPIValidation(t *testing.T) {
	mem := store.NewMem()
	s := newTestServer(t, goodSnapshot(), mem, "")
	h := s.Handler()

	do := func(method, path, body string) int {
		rr := httptest.NewRecorder()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, rd)
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	if c := do(http.MethodPut, "/api/quotas/other-ns/sample-bkt", `{"quota_bytes":100}`); c != http.StatusBadRequest {
		t.Fatalf("wrong namespace = %d, want 400", c)
	}
	if c := do(http.MethodPut, "/api/quotas/prod-ns/ab", `{"quota_bytes":100}`); c != http.StatusBadRequest {
		t.Fatalf("short bucket = %d, want 400", c)
	}
	if c := do(http.MethodPut, "/api/quotas/prod-ns/bad%2Fname", `{"quota_bytes":100}`); c != http.StatusBadRequest {
		t.Fatalf("slash bucket = %d, want 400", c)
	}
	if c := do(http.MethodPut, "/api/quotas/prod-ns/good-bkt", `{"quota_bytes":0}`); c != http.StatusBadRequest {
		t.Fatalf("zero quota = %d, want 400", c)
	}
	if c := do(http.MethodPut, "/api/quotas/prod-ns/good-bkt", `{"quota_bytes":107374182400}`); c != http.StatusOK {
		t.Fatalf("valid PUT = %d, want 200", c)
	}
	q, _ := mem.Quotas(context.Background(), "prod-ns")
	if q["good-bkt"].QuotaBytes != 107374182400 {
		t.Fatalf("quota not persisted: %+v", q)
	}

	// POST body-identified variant.
	if c := do(http.MethodPost, "/api/quotas", `{"namespace":"prod-ns","bucket":"body-bkt","quota_bytes":2048}`); c != http.StatusOK {
		t.Fatalf("valid POST = %d, want 200", c)
	}

	if c := do(http.MethodDelete, "/api/quotas/prod-ns/good-bkt", ""); c != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", c)
	}
	q, _ = mem.Quotas(context.Background(), "prod-ns")
	if _, still := q["good-bkt"]; still {
		t.Fatal("quota must be gone after DELETE")
	}
}

func TestHTMLQuotaFormPRG(t *testing.T) {
	mem := store.NewMem()
	s := newTestServer(t, goodSnapshot(), mem, "")
	h := s.Handler()

	form := strings.NewReader("bucket=sample-bkt&quota=2&unit=GiB")
	req := httptest.NewRequest(http.MethodPost, "/", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("form POST = %d, want 303 (PRG)", rr.Code)
	}
	q, _ := mem.Quotas(context.Background(), "prod-ns")
	if q["sample-bkt"].QuotaBytes != 2*1024*1024*1024 {
		t.Fatalf("form quota = %+v, want 2 GiB in bytes", q)
	}

	// Delete via action=delete.
	form = strings.NewReader("bucket=sample-bkt&action=delete")
	req = httptest.NewRequest(http.MethodPost, "/", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete form = %d, want 303", rr.Code)
	}
	q, _ = mem.Quotas(context.Background(), "prod-ns")
	if _, still := q["sample-bkt"]; still {
		t.Fatal("quota must be removed by action=delete")
	}
}

func TestDashboardRenders(t *testing.T) {
	mem := store.NewMem()
	s := newTestServer(t, goodSnapshot(), mem, "")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"sample-bkt", "11.00 GiB", "prod-ns"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "11 GB") {
		t.Error("dashboard must render binary units (GiB), not GB")
	}
}

func TestBasePathPrefix(t *testing.T) {
	s, err := New(Deps{
		Namespace: "prod-ns",
		BasePath:  "/storage",
		Snapshots: &fakeSnap{snap: goodSnapshot(), th: time.Hour},
		Store:     store.NewMem(),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	for _, path := range []string{"/storage/", "/storage/api/buckets", "/storage/health/ready",
		"/health/ready", "/api/buckets"} { // doubled at root for local dev/probes
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code == http.StatusNotFound {
			t.Errorf("%s must be registered, got 404", path)
		}
	}
}
