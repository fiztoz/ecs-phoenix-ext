package http

import (
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
		"used_percent", "stale", "at_quota", "over_streak", "confirmed_over"} {
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

// The Phoenix iframe flow: the gated /frame redirect arrives once with the
// credential in the query, and every later navigation (wallboard link, form
// POST) must keep working off the exchanged session cookie alone.
func TestUITokenCookieExchange(t *testing.T) {
	s := newTestServer(t, goodSnapshot(), store.NewMem(), "test-ui-value")
	h := s.Handler()

	// Hand-off: ui_token query parameter authenticates and sets the cookie.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/?ui_token=test-ui-value", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("ui_token hand-off = %d, want 200", rr.Code)
	}
	var session *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "ecs_ui_session" {
			session = c
		}
	}
	if session == nil {
		t.Fatal("hand-off did not set the ecs_ui_session cookie")
	}
	if !session.HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
	if session.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, want Lax", session.SameSite)
	}

	// Follow-up navigation with ONLY the cookie — no token in the URL.
	for _, path := range []string{"/", "/wallboard", "/api/buckets"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(session)
		rr = httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("cookie-authenticated GET %s = %d, want 200", path, rr.Code)
		}
	}

	// A wrong cookie value is rejected.
	req := httptest.NewRequest(http.MethodGet, "/api/buckets", nil)
	req.AddCookie(&http.Cookie{Name: "ecs_ui_session", Value: "wrong"})
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("wrong cookie = %d, want 401", rr.Code)
	}

	// Bearer hand-off also sets the cookie.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer test-ui-value")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("bearer hand-off = %d, want 200", rr.Code)
	}
	found := false
	for _, c := range rr.Result().Cookies() {
		if c.Name == "ecs_ui_session" {
			found = true
		}
	}
	if !found {
		t.Error("bearer hand-off did not set the session cookie")
	}
}

// With UI_TOKEN unset no cookie is ever issued — the UI is open.
func TestUITokenCookieNotSetWhenOpen(t *testing.T) {
	s := newTestServer(t, goodSnapshot(), store.NewMem(), "")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("open UI = %d, want 200", rr.Code)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "ecs_ui_session" {
			t.Fatal("session cookie set although UI_TOKEN is empty")
		}
	}
}

func TestBucketQuotaAPIRemoved(t *testing.T) {
	s := newTestServer(t, goodSnapshot(), store.NewMem(), "")
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

	// Bucket quotas come from the ECS inventory poll; manual set/delete is
	// gone. PUT/DELETE hit no route (405 via the dashboard catch-all);
	// POST lands on the quota form, which rejects bucket quotas with an
	// explanatory redirect.
	if c := do(http.MethodPut, "/api/quotas/prod-ns/good-bkt", `{"quota_bytes":107374182400}`); c != http.StatusMethodNotAllowed {
		t.Errorf("PUT removed route = %d, want 405", c)
	}
	if c := do(http.MethodDelete, "/api/quotas/prod-ns/good-bkt", ""); c != http.StatusMethodNotAllowed {
		t.Errorf("DELETE removed route = %d, want 405", c)
	}
	if c := do(http.MethodPost, "/api/quotas", `{"namespace":"prod-ns"}`); c != http.StatusMethodNotAllowed {
		t.Errorf("POST removed route = %d, want 405 (quota form gone)", c)
	}
}

func TestNamespaceQuotaAPIAndFormsRemoved(t *testing.T) {
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

	// Namespace quota is ECS-native now: the manual API and both HTML form
	// paths (namespace + bucket) are gone.
	for _, tc := range [][3]string{
		{http.MethodPut, "/api/namespace-quotas/prod-ns", `{"quota_bytes":107374182400}`},
		{http.MethodDelete, "/api/namespace-quotas/prod-ns", ""},
		{http.MethodPost, "/", "scope=namespace&quota=2&unit=GiB"},
		{http.MethodPost, "/", "bucket=sample-bkt&quota=2&unit=GiB"},
	} {
		if c := do(tc[0], tc[1], tc[2]); c == http.StatusOK || c == http.StatusSeeOther {
			t.Errorf("%s %s = %d, want removed (404/405)", tc[0], tc[1], c)
		}
	}
	_ = do // silence unused-parameter lint on the helper when methods vary
}

func TestDashboardUnlimitedQuotaOff(t *testing.T) {
	// No ECS block threshold (quota off in the ECS UI) renders as Unlimited
	// with no manual Set form anywhere on the page.
	snap := goodSnapshot()
	snap.Buckets[0].QuotaBytes = nil
	snap.Buckets[0].UsedPercent = nil
	snap.Buckets[0].BlockSize = nil
	snap.Buckets[0].NotificationSize = nil
	s := newTestServer(t, snap, store.NewMem(), "")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rr.Body.String()
	if !strings.Contains(body, "Unlimited") {
		t.Error("quota-off bucket must show Unlimited")
	}
	if strings.Contains(body, `name="bucket"`) {
		t.Error("bucket quota Set form must be gone (quotas come from ECS)")
	}
	// Namespace and bucket quotas are both ECS-native: no manual forms remain.
	if strings.Contains(body, "scope=namespace") || strings.Contains(body, `name="bucket"`) {
		t.Error("manual quota forms must be gone (quotas come from ECS)")
	}
	if strings.Contains(body, "Set namespace quota") {
		t.Error("namespace quota Set form must be gone")
	}

	// Wallboard agrees: unlimited label, no block/notify numbers.
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/wallboard", nil))
	wb := rr2.Body.String()
	if !strings.Contains(wb, "unlimited quota") || !strings.Contains(wb, "Unlimited") {
		t.Error("wallboard must label quota-off buckets unlimited")
	}
}

func TestDashboardNamespaceThresholds(t *testing.T) {
	nsBSize, nsNotify := int64(10737418240), int64(8589934592)
	snap := goodSnapshot()
	snap.NamespaceBlockSize = &nsBSize
	snap.NamespaceNotificationSize = &nsNotify
	s := newTestServer(t, snap, store.NewMem(), "")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rr.Body.String()
	// 10 GiB block / 8 GiB notify chips appear only because ECS set them.
	for _, want := range []string{"10.00 GiB", "8.00 GiB"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing namespace threshold %q", want)
		}
	}
}
