package ecs

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeECS simulates the Dell ECS management endpoints.
type fakeECS struct {
	mu          sync.Mutex
	logins      int
	logouts     int
	logoutQuery string
	logoutTok   string

	billingCalls int
	// billing is called with the request and the 1-based call index.
	billing func(w http.ResponseWriter, r *http.Request, call int)
}

func (f *fakeECS) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/login":
			f.mu.Lock()
			f.logins++
			n := f.logins
			f.mu.Unlock()
			w.Header().Set("X-SDS-AUTH-TOKEN", fmt.Sprintf("token-%d", n))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<loggedIn><user>ecs-usage</user></loggedIn>"))
		case r.URL.Path == "/logout":
			f.mu.Lock()
			f.logouts++
			f.logoutQuery = r.URL.RawQuery
			f.logoutTok = r.Header.Get("X-SDS-AUTH-TOKEN")
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(r.URL.Path, "/object/billing/namespace/"):
			f.mu.Lock()
			f.billingCalls++
			n := f.billingCalls
			h := f.billing
			f.mu.Unlock()
			if h == nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			h(w, r, n)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func newTestClient(t *testing.T, base string) *Client {
	t.Helper()
	c, err := NewClient(ClientConfig{
		BaseURL:  base,
		Username: "ecs-usage",
		Userpass: "test-only-fixture",
		SizeUnit: "KB",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestLoginReadsTokenHeaderAndCaches(t *testing.T) {
	f := &fakeECS{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	ctx := context.Background()

	tok1, err := c.Token(ctx)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok1 != "token-1" {
		t.Fatalf("token = %q, want token-1 (from X-SDS-AUTH-TOKEN header)", tok1)
	}
	tok2, err := c.Token(ctx)
	if err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if tok2 != tok1 {
		t.Fatalf("token changed between calls: %q vs %q", tok1, tok2)
	}
	f.mu.Lock()
	got := f.logins
	f.mu.Unlock()
	if got != 1 {
		t.Fatalf("logins = %d, want 1 (token must be cached)", got)
	}
}

func TestBilling401ReloginsExactlyOnce(t *testing.T) {
	f := &fakeECS{}
	f.billing = func(w http.ResponseWriter, r *http.Request, call int) {
		if call == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		b, _ := os.ReadFile("testdata/namespace_info.json")
		_, _ = w.Write(b)
	}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	p, err := c.NamespaceBilling(context.Background(), "prod-ns")
	if err != nil {
		t.Fatalf("NamespaceBilling: %v", err)
	}
	if len(p.Buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(p.Buckets))
	}
	f.mu.Lock()
	logins, billings := f.logins, f.billingCalls
	f.mu.Unlock()
	if logins != 2 {
		t.Fatalf("logins = %d, want 2 (initial + exactly one re-login)", logins)
	}
	if billings != 2 {
		t.Fatalf("billing calls = %d, want 2 (401 + retry)", billings)
	}
}

func TestBillingRedirectToLoginTreatedAsExpired(t *testing.T) {
	f := &fakeECS{}
	f.billing = func(w http.ResponseWriter, r *http.Request, call int) {
		if call == 1 {
			w.Header().Set("Location", "/login")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		b, _ := os.ReadFile("testdata/namespace_info.json")
		_, _ = w.Write(b)
	}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if _, err := c.NamespaceBilling(context.Background(), "prod-ns"); err != nil {
		t.Fatalf("NamespaceBilling: %v", err)
	}
	f.mu.Lock()
	logins := f.logins
	f.mu.Unlock()
	if logins != 2 {
		t.Fatalf("logins = %d, want 2 (redirect-to-/login must trigger one re-login)", logins)
	}
}

func TestLogoutWithoutForce(t *testing.T) {
	f := &fakeECS{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	ctx := context.Background()
	if _, err := c.Token(ctx); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if err := c.Logout(ctx); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logouts != 1 {
		t.Fatalf("logouts = %d, want 1", f.logouts)
	}
	if strings.Contains(f.logoutQuery, "force") {
		t.Fatalf("logout sent force=true (%q): would kill every session for the user", f.logoutQuery)
	}
	if f.logoutTok != "token-1" {
		t.Fatalf("logout token = %q, want cached token-1", f.logoutTok)
	}
}

func TestJSONFixture(t *testing.T) {
	f := &fakeECS{}
	f.billing = func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := os.ReadFile("testdata/namespace_info.json")
		_, _ = w.Write(b)
	}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	p, err := c.NamespaceBilling(context.Background(), "prod-ns")
	if err != nil {
		t.Fatalf("NamespaceBilling: %v", err)
	}
	if p.Namespace != "prod-ns" {
		t.Errorf("namespace = %q", p.Namespace)
	}
	if p.NamespaceBytes != 11811160064 {
		t.Errorf("namespace bytes = %d, want 11811160064 (11 GiB)", p.NamespaceBytes)
	}
	if p.NamespaceObjects != 2 {
		t.Errorf("namespace objects = %d, want 2", p.NamespaceObjects)
	}
	if len(p.Buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(p.Buckets))
	}
	b := p.Buckets[0]
	if b.Name != "sample-bkt" {
		t.Errorf("bucket name = %q", b.Name)
	}
	if b.UsedBytes != 11811160064 {
		t.Errorf("used = %d, want 11811160064", b.UsedBytes)
	}
	if b.MPUBytes != 0 {
		t.Errorf("mpu = %d, want 0", b.MPUBytes)
	}
	if b.SampleTime.IsZero() || b.UptodateTill.IsZero() {
		t.Errorf("sample/uptodate times must parse: %v / %v", b.SampleTime, b.UptodateTill)
	}
	if b.SampleTime.Location() != time.UTC || b.UptodateTill.Location() != time.UTC {
		t.Errorf("timestamps must be UTC: %v / %v", b.SampleTime.Location(), b.UptodateTill.Location())
	}
}

func TestMPUIncludedInUsedBytes(t *testing.T) {
	body := `{
	  "namespace": "prod-ns",
	  "total_size": 5, "total_size_unit": "KB", "total_objects": 3,
	  "sample_time": "2026-08-18T09:55:00Z",
	  "bucket_billing_info": [
	    {"name": "mpu-bkt", "total_size": 5, "total_size_unit": "KB",
	     "total_objects": 3, "sample_time": "2026-08-18T09:55:00Z",
	     "total_mpu_size": 1024, "total_mpu_parts": 4}
	  ]
	}`
	f := &fakeECS{}
	f.billing = func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	p, err := c.NamespaceBilling(context.Background(), "prod-ns")
	if err != nil {
		t.Fatalf("NamespaceBilling: %v", err)
	}
	b := p.Buckets[0]
	wantMPU := int64(1024 * 1024) // 1024 KB in the same total_size_unit
	if b.MPUBytes != wantMPU {
		t.Fatalf("mpu = %d, want %d", b.MPUBytes, wantMPU)
	}
	if b.UsedBytes != 5*1024+wantMPU {
		t.Fatalf("used = %d, want total+mpu = %d", b.UsedBytes, 5*1024+wantMPU)
	}
}

func TestXMLFixtureMatchesJSON(t *testing.T) {
	f := &fakeECS{}
	f.billing = func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/xml")
		b, _ := os.ReadFile("testdata/namespace_info.xml")
		_, _ = w.Write(b)
	}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	p, err := c.NamespaceBilling(context.Background(), "prod-ns")
	if err != nil {
		t.Fatalf("NamespaceBilling: %v", err)
	}
	if p.NamespaceBytes != 11811160064 || p.NamespaceObjects != 2 {
		t.Fatalf("XML numbers differ from JSON fixture: %d / %d", p.NamespaceBytes, p.NamespaceObjects)
	}
	if len(p.Buckets) != 1 || p.Buckets[0].UsedBytes != 11811160064 || p.Buckets[0].Objects != 2 {
		t.Fatalf("XML bucket rows differ from JSON fixture: %+v", p.Buckets)
	}
}

func TestPaginationFollowsNextMarker(t *testing.T) {
	var markers []string
	f := &fakeECS{}
	f.billing = func(w http.ResponseWriter, r *http.Request, call int) {
		markers = append(markers, r.URL.Query().Get("marker"))
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			fmt.Fprint(w, `{"namespace":"ns","total_size":1,"total_size_unit":"KB","total_objects":2,
			  "sample_time":"2026-08-18T09:55:00Z",
			  "bucket_billing_info":[{"name":"b1","total_size":1,"total_size_unit":"KB","total_objects":1,"sample_time":"2026-08-18T09:55:00Z"}],
			  "next_marker":"m1"}`)
		default:
			fmt.Fprint(w, `{"namespace":"ns","total_size":1,"total_size_unit":"KB","total_objects":2,
			  "sample_time":"2026-08-18T09:55:00Z",
			  "bucket_billing_info":[{"name":"b2","total_size":1,"total_size_unit":"KB","total_objects":1,"sample_time":"2026-08-18T09:55:00Z"}],
			  "next_marker":""}`)
		}
	}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	p, err := c.NamespaceBilling(context.Background(), "ns")
	if err != nil {
		t.Fatalf("NamespaceBilling: %v", err)
	}
	if len(p.Buckets) != 2 || p.Buckets[0].Name != "b1" || p.Buckets[1].Name != "b2" {
		t.Fatalf("buckets = %+v, want b1 then b2", p.Buckets)
	}
	if markers[0] != "" || markers[1] != "m1" {
		t.Fatalf("markers = %v, want [\"\", \"m1\"]", markers)
	}
}

func TestPaginationLoopGuard(t *testing.T) {
	var calls int32
	f := &fakeECS{}
	f.billing = func(w http.ResponseWriter, r *http.Request, _ int) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		// Always the same non-empty marker: the client must stop instead
		// of looping forever.
		fmt.Fprint(w, `{"namespace":"ns","total_size":1,"total_size_unit":"KB","total_objects":1,
		  "sample_time":"2026-08-18T09:55:00Z",
		  "bucket_billing_info":[{"name":"b1","total_size":1,"total_size_unit":"KB","total_objects":1,"sample_time":"2026-08-18T09:55:00Z"}],
		  "next_marker":"same"}`)
	}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if _, err := c.NamespaceBilling(context.Background(), "ns"); err != nil {
		t.Fatalf("NamespaceBilling: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("billing calls = %d, want 2 (repeat marker must stop the loop)", got)
	}
}

func TestPaginationPageCap(t *testing.T) {
	var calls int32
	f := &fakeECS{}
	f.billing = func(w http.ResponseWriter, r *http.Request, call int) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"namespace":"ns","total_size":1,"total_size_unit":"KB","total_objects":1,
		  "sample_time":"2026-08-18T09:55:00Z",
		  "bucket_billing_info":[{"name":"b%d","total_size":1,"total_size_unit":"KB","total_objects":1,"sample_time":"2026-08-18T09:55:00Z"}],
		  "next_marker":"m%d"}`, call, call)
	}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.NamespaceBilling(context.Background(), "ns")
	if err == nil {
		t.Fatal("expected pagination overflow error")
	}
	if got := atomic.LoadInt32(&calls); got != int32(maxBillingPages) {
		t.Fatalf("billing calls = %d, want cap %d", got, maxBillingPages)
	}
}

func TestJSONSuffixFallbackOn406(t *testing.T) {
	var paths []string
	f := &fakeECS{}
	f.billing = func(w http.ResponseWriter, r *http.Request, _ int) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, ".json") {
			w.Header().Set("Content-Type", "application/json")
			b, _ := os.ReadFile("testdata/namespace_info.json")
			_, _ = w.Write(b)
			return
		}
		w.WriteHeader(http.StatusNotAcceptable)
	}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	p, err := c.NamespaceBilling(context.Background(), "prod-ns")
	if err != nil {
		t.Fatalf("NamespaceBilling: %v", err)
	}
	if len(p.Buckets) != 1 {
		t.Fatalf("buckets = %d, want 1", len(p.Buckets))
	}
	if len(paths) != 2 || !strings.HasSuffix(paths[1], "/info.json") {
		t.Fatalf("paths = %v, want /info then /info.json", paths)
	}
}
