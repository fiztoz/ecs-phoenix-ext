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
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/quotas", strings.NewReader(`{"namespace":"prod-ns"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Errorf("POST removed route = %d, want 303 with error message", rr.Code)
	}
}

func TestNamespaceQuotaAPIValidation(t *testing.T) {
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

	if c := do(http.MethodPut, "/api/namespace-quotas/other-ns", `{"quota_bytes":100}`); c != http.StatusBadRequest {
		t.Fatalf("wrong namespace = %d, want 400", c)
	}
	if c := do(http.MethodPut, "/api/namespace-quotas/prod-ns", `{"quota_bytes":0}`); c != http.StatusBadRequest {
		t.Fatalf("zero quota = %d, want 400", c)
	}
	if c := do(http.MethodPut, "/api/namespace-quotas/prod-ns", `{"quota_bytes":107374182400}`); c != http.StatusOK {
		t.Fatalf("valid PUT = %d, want 200", c)
	}
	q, _ := mem.NamespaceQuota(context.Background(), "prod-ns")
	if q == nil || q.QuotaBytes != 107374182400 {
		t.Fatalf("quota not persisted: %+v", q)
	}
	if c := do(http.MethodDelete, "/api/namespace-quotas/prod-ns", ""); c != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", c)
	}
	q, _ = mem.NamespaceQuota(context.Background(), "prod-ns")
	if q != nil {
		t.Fatal("quota must be gone after DELETE")
	}
}

func TestHTMLQuotaFormPRG(t *testing.T) {
	mem := store.NewMem()
	s := newTestServer(t, goodSnapshot(), mem, "")
	h := s.Handler()

	// Namespace quota set via form (PRG).
	form := strings.NewReader("scope=namespace&quota=2&unit=GiB")
	req := httptest.NewRequest(http.MethodPost, "/", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("form POST = %d, want 303 (PRG)", rr.Code)
	}
	q, _ := mem.NamespaceQuota(context.Background(), "prod-ns")
	if q == nil || q.QuotaBytes != 2*1024*1024*1024 {
		t.Fatalf("form quota = %+v, want 2 GiB in bytes", q)
	}

	// Delete via action=delete.
	form = strings.NewReader("scope=namespace&action=delete")
	req = httptest.NewRequest(http.MethodPost, "/", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("delete form = %d, want 303", rr.Code)
	}
	q, _ = mem.NamespaceQuota(context.Background(), "prod-ns")
	if q != nil {
		t.Fatal("quota must be removed by action=delete")
	}

	// Bucket quotas are ECS-native: a bucket form post is rejected, and it
	// must not create any operator quota behind the scenes.
	form = strings.NewReader("bucket=sample-bkt&quota=2&unit=GiB")
	req = httptest.NewRequest(http.MethodPost, "/", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("bucket form POST = %d, want 303 (PRG with error message)", rr.Code)
	}
	if loc := rr.Header().Get("Location"); !strings.Contains(loc, "msg=") {
		t.Fatalf("bucket form must redirect with an error message, got %q", loc)
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

func TestDashboardZeroTimesRenderAsDash(t *testing.T) {
	snap := goodSnapshot()
	snap.Buckets[0].UptodateTill = time.Time{}
	s := newTestServer(t, snap, store.NewMem(), "")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard = %d", rr.Code)
	}
	for _, banned := range []string{"0001-01-01", "0000-00-00"} {
		if strings.Contains(rr.Body.String(), banned) {
			t.Errorf("dashboard renders zero time as %q", banned)
		}
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

func TestAPIBucketsIncludesInventory(t *testing.T) {
	block, notify, nsblock := int64(134217728), int64(1073741824), int64(134217728)
	snap := goodSnapshot()
	snap.InventoryOK = true
	snap.NamespaceDefaultBlock = &nsblock
	snap.Buckets[0].BlockSize = &block
	snap.Buckets[0].NotificationSize = &notify
	s := newTestServer(t, snap, store.NewMem(), "")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/buckets", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"inventory_ok", "inventory_error", "namespace_default_block_size"} {
		if _, ok := body[key]; !ok {
			t.Errorf("missing key %q", key)
		}
	}
	b0 := body["buckets"].([]any)[0].(map[string]any)
	for _, key := range []string{"block_size", "notification_size"} {
		if _, ok := b0[key]; !ok {
			t.Errorf("bucket missing key %q", key)
		}
	}
	if b0["block_size"].(float64) != 134217728 {
		t.Errorf("block_size = %v", b0["block_size"])
	}
	// Nil inventory renders as null, never 0.
	snap2 := goodSnapshot()
	s2 := newTestServer(t, snap2, store.NewMem(), "")
	rr2 := httptest.NewRecorder()
	s2.Handler().ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/api/buckets", nil))
	var body2 map[string]any
	_ = json.Unmarshal(rr2.Body.Bytes(), &body2)
	b2 := body2["buckets"].([]any)[0].(map[string]any)
	if b2["block_size"] != nil || b2["notification_size"] != nil {
		t.Fatalf("missing inventory must be null, got %v / %v", b2["block_size"], b2["notification_size"])
	}
}

func TestDashboardShowsBlockNotifyNotMPU(t *testing.T) {
	block, notify := int64(134217728), int64(1073741824)
	snap := goodSnapshot()
	snap.Buckets[0].BlockSize = &block
	snap.Buckets[0].NotificationSize = &notify
	s := newTestServer(t, snap, store.NewMem(), "")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rr.Body.String()
	for _, want := range []string{"128.00 MiB", "1.00 GiB", "Block", "Notify"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "data-mpu=") || strings.Contains(body, ">MPU<") {
		t.Error("dashboard must not show the MPU column anymore (folded into Used)")
	}
}

func TestAPIQuotaMode(t *testing.T) {
	block := int64(10737418240) // 10 GiB "Block Access at" from the ECS UI
	snap := goodSnapshot()
	snap.Buckets[0].BlockSize = &block
	snap.Buckets[0].NotificationSize = nil
	s := newTestServer(t, snap, store.NewMem(), "")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/buckets", nil))
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	b0 := body["buckets"].([]any)[0].(map[string]any)
	if b0["quota_mode"] != "block-only" {
		t.Errorf("quota_mode = %v, want block-only", b0["quota_mode"])
	}
	if b0["notification_size"] != nil {
		t.Errorf("notification_size must be null when ECS quota has no notify threshold, got %v", b0["notification_size"])
	}
	// Dashboard names the ECS-native mode under the bucket.
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rr2.Body.String(), "ECS quota: block-only") {
		t.Error("dashboard must label the ECS-native quota mode under the bucket name")
	}
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
	// The namespace total quota form stays: ECS exposes no namespace quota.
	if !strings.Contains(body, "Set namespace quota:") {
		t.Error("namespace quota form must stay (no ECS source for it)")
	}

	// Wallboard agrees: unlimited label, no block/notify numbers.
	rr2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/wallboard", nil))
	wb := rr2.Body.String()
	if !strings.Contains(wb, "unlimited quota") || !strings.Contains(wb, "Unlimited") {
		t.Error("wallboard must label quota-off buckets unlimited")
	}
}
