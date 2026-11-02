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

	"github.com/fiztoz/ecs-phoenix-ext/internal/identity"
	"github.com/fiztoz/ecs-phoenix-ext/internal/store"
)

const idSecret = "0123456789abcdef0123456789abcdef"

func newIdentityServer(t *testing.T, uiToken string, withIdentity bool) *Server {
	t.Helper()
	deps := Deps{
		Namespace: "prod-ns",
		BasePath:  "/storage",
		UIToken:   uiToken,
		Snapshots: &fakeSnap{snap: activitySnapshot(), th: time.Hour},
		Store:     store.NewMem(),
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if withIdentity {
		v, err := identity.NewVerifier(idSecret, "ecs-phoenix-ext")
		if err != nil {
			t.Fatal(err)
		}
		deps.Identity = v
	}
	s, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func phoenixToken(scopes ...string) string {
	return identity.Sign(idSecret, "ecs-phoenix-ext",
		identity.Principal{Subject: "42", Name: "alice", Kind: "api_key", Scopes: scopes}, time.Now(), time.Minute)
}

func TestV1AuthMatrix(t *testing.T) {
	cases := []struct {
		name         string
		uiToken      string
		withIdentity bool
		header       map[string]string
		want         int
	}{
		{"open when nothing configured", "", false, nil, 200},
		{"identity header without secret is refused", "", false,
			map[string]string{identity.Header: phoenixToken("read")}, 401},
		{"ui token required", "tok", false, nil, 401},
		{"ui token bearer", "tok", false, map[string]string{"Authorization": "Bearer tok"}, 200},
		{"identity only: direct call refused", "", true, nil, 401},
		{"identity read", "", true, map[string]string{identity.Header: phoenixToken("read")}, 200},
		{"identity without read scope", "", true, map[string]string{identity.Header: phoenixToken("write")}, 403},
		{"bad identity never falls back to ui token", "tok", true,
			map[string]string{identity.Header: "garbage", "Authorization": "Bearer tok"}, 401},
		{"ui token still works next to identity", "tok", true, map[string]string{"Authorization": "Bearer tok"}, 200},
	}
	for _, tc := range cases {
		s := newIdentityServer(t, tc.uiToken, tc.withIdentity)
		req := httptest.NewRequest(http.MethodGet, "/storage/api/v1/namespace", nil)
		for k, v := range tc.header {
			req.Header.Set(k, v)
		}
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != tc.want {
			t.Errorf("%s: status = %d, want %d (%s)", tc.name, rr.Code, tc.want, rr.Body.String())
		}
	}
}

func TestV1WhoAmIEchoesPhoenixIdentity(t *testing.T) {
	s := newIdentityServer(t, "", true)
	req := httptest.NewRequest(http.MethodGet, "/storage/api/v1/whoami", nil)
	req.Header.Set(identity.Header, phoenixToken("read"))
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	var got struct {
		Subject string   `json:"subject"`
		Kind    string   `json:"kind"`
		Scopes  []string `json:"scopes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Subject != "42" || got.Kind != "api_key" || len(got.Scopes) != 1 {
		t.Fatalf("whoami = %+v", got)
	}
}

func TestV1Endpoints(t *testing.T) {
	s := newIdentityServer(t, "", false)
	get := func(path string) (int, map[string]any) {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		var body map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		return rr.Code, body
	}

	code, body := get("/storage/api/v1/namespace")
	if code != 200 || body["bucket_count"] == nil || body["hot_buckets"].(float64) != 1 {
		t.Errorf("namespace: %d %v", code, body)
	}
	code, body = get("/storage/api/v1/buckets?sort=activity&limit=1")
	if code != 200 || body["total"].(float64) < 2 || body["truncated"] != true {
		t.Errorf("buckets: %d %v", code, body)
	} else if b := body["buckets"].([]any)[0].(map[string]any); b["name"] != "hot-bkt" {
		t.Errorf("top activity bucket = %v, want hot-bkt", b["name"])
	}
	if code, _ = get("/storage/api/v1/buckets?sort=size"); code != 400 {
		t.Errorf("bad sort: %d, want 400", code)
	}
	if code, _ = get("/storage/api/v1/buckets?limit=0"); code != 400 {
		t.Errorf("bad limit: %d, want 400", code)
	}
	if code, body = get("/storage/api/v1/buckets/hot-bkt"); code != 200 || body["name"] != "hot-bkt" {
		t.Errorf("bucket: %d %v", code, body)
	}
	if code, _ = get("/storage/api/v1/buckets/nope"); code != 404 {
		t.Errorf("unknown bucket: %d, want 404", code)
	}
	if code, body = get("/storage/api/v1/buckets/hot-bkt/activity?hours=6"); code != 200 || body["hours"] == nil {
		t.Errorf("activity: %d %v", code, body)
	}
	if code, _ = get("/storage/api/v1/buckets/hot-bkt/activity?hours=99999"); code != 400 {
		t.Errorf("bad hours: %d, want 400", code)
	}
}

func TestDashboardGuideFollowsAccessMode(t *testing.T) {
	cases := []struct {
		name         string
		uiToken      string
		withIdentity bool
		want, reject []string
	}{
		{"open", "", false,
			[]string{`id="guide"`, "API &amp; MCP", "/storage/mcp", ">open<"},
			[]string{"ApiKey", "shared token"}},
		{"ui token", "s3cret-ui-token", false,
			[]string{"Bearer $ECS_UI_TOKEN", "shared token"},
			[]string{"s3cret-ui-token", "ApiKey"}},
		{"phoenix gateway", "s3cret-ui-token", true,
			[]string{"/api/extensions/ecs-phoenix-ext/call/mcp", "ApiKey $PHOENIX_API_KEY", "per-user",
				"/api/extensions/ecs-phoenix-ext/call/api/v1/buckets"},
			[]string{"s3cret-ui-token"}},
	}
	for _, tc := range cases {
		s := newIdentityServer(t, tc.uiToken, tc.withIdentity)
		s.deps.ExtensionID = "ecs-phoenix-ext"
		req := httptest.NewRequest(http.MethodGet, "/storage/", nil)
		req.Header.Set("Authorization", "Bearer "+tc.uiToken)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: dashboard status %d", tc.name, rr.Code)
		}
		body := rr.Body.String()
		for _, w := range tc.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: guide missing %q", tc.name, w)
			}
		}
		for _, r := range tc.reject {
			if strings.Contains(body, r) {
				t.Errorf("%s: guide must not contain %q", tc.name, r)
			}
		}
	}
}
