package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fiztoz/ecs-phoenix-ext/internal/identity"
)

// headerRT adds fixed headers to every request, the way an MCP client
// configured with --header does.
type headerRT struct{ h map[string]string }

func (rt headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range rt.h {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// connect opens an SDK client session against a real HTTP server running the
// extension handler, so the official client and server must agree on the
// wire protocol.
func connect(t *testing.T, s *Server, hdr map[string]string) (*mcp.ClientSession, error) {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1"}, nil)
	return client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   ts.URL + "/storage/mcp",
		HTTPClient: &http.Client{Transport: headerRT{hdr}},
		MaxRetries: -1,
	}, nil)
}

func mustConnect(t *testing.T, s *Server, hdr map[string]string) *mcp.ClientSession {
	t.Helper()
	cs, err := connect(t, s, hdr)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var out []string
	for _, tl := range res.Tools {
		out = append(out, tl.Name)
	}
	return out
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error %v", name, err)
	}
	return res
}

func TestMCPInitialize(t *testing.T) {
	cs := mustConnect(t, newIdentityServer(t, "", false), nil)
	init := cs.InitializeResult()
	if init.ServerInfo.Name != "ecs-phoenix-ext" {
		t.Errorf("serverInfo = %+v", init.ServerInfo)
	}
	if !strings.Contains(init.Instructions, "Read/GET traffic is invisible") {
		t.Error("instructions must state the activity limit")
	}
}

func TestMCPToolsFollowScopes(t *testing.T) {
	s := newIdentityServer(t, "", true)

	cs := mustConnect(t, s, map[string]string{identity.Header: phoenixToken("read")})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 4 {
		t.Fatalf("read caller sees %d tools, want 4", len(res.Tools))
	}
	for _, tl := range res.Tools {
		if a := tl.Annotations; a == nil || !a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint {
			t.Errorf("%s is not annotated read-only/non-destructive", tl.Name)
		}
	}

	// A caller without read sees nothing and cannot call around the list.
	cs = mustConnect(t, s, map[string]string{identity.Header: phoenixToken("other")})
	if n := toolNames(t, cs); len(n) != 0 {
		t.Errorf("scope-less caller sees %v", n)
	}
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "ecs_namespace_summary"}); err == nil {
		t.Error("scope-less tools/call succeeded")
	}
}

func TestMCPToolCalls(t *testing.T) {
	cs := mustConnect(t, newIdentityServer(t, "", false), nil)

	res := call(t, cs, "ecs_list_buckets", map[string]any{"sort": "activity", "hot_only": true})
	if res.IsError {
		t.Fatalf("list: %+v", res.Content)
	}
	var list struct {
		Total   int `json:"total"`
		Buckets []struct {
			Name string `json:"name"`
		} `json:"buckets"`
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if list.Total != 1 || list.Buckets[0].Name != "hot-bkt" {
		t.Errorf("hot_only list = %s", raw)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, `"hot-bkt"`) {
		t.Errorf("text content does not mirror structuredContent: %s", text)
	}

	for name, args := range map[string]any{
		"ecs_namespace_summary":       nil,
		"ecs_get_bucket":              map[string]any{"bucket": "hot-bkt"},
		"ecs_bucket_activity_history": map[string]any{"bucket": "hot-bkt", "hours": 3},
		"ecs_list_buckets":            map[string]any{},
	} {
		if res := call(t, cs, name, args); res.IsError {
			t.Errorf("%s: tool error %+v", name, res.Content[0])
		}
	}

	// Caller mistakes come back as tool errors (or argument validation
	// failures) so the model can correct itself.
	for name, args := range map[string]map[string]any{
		"unknown bucket": {"bucket": "nope"},
		"missing bucket": {},
	} {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "ecs_get_bucket", Arguments: args})
		if err == nil && !res.IsError {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, args := range map[string]map[string]any{
		"bad sort":     {"sort": "size"},
		"limit 0":      {"limit": 0},
		"limit huge":   {"limit": 5000},
		"unknown args": {"bogus": true},
	} {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "ecs_list_buckets", Arguments: args})
		if err == nil && !res.IsError {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "ecs_delete_bucket"}); err == nil {
		t.Error("unknown tool accepted")
	}
}

func TestMCPTransportEdges(t *testing.T) {
	s := newIdentityServer(t, "", false)
	post := func(hdr map[string]string, body string) int {
		req := httptest.NewRequest(http.MethodPost, "/storage/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr.Code
	}
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/storage/mcp", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d, want 405", rr.Code)
	}
	if code := post(map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}, ping); code != http.StatusForbidden {
		t.Errorf("cross-site browser POST: %d, want 403", code)
	}
	if code := post(map[string]string{"MCP-Protocol-Version": "1999-01-01"}, ping); code != http.StatusBadRequest {
		t.Errorf("bad protocol header: %d, want 400", code)
	}
	if code := post(nil, strings.Repeat(" ", 2<<20)+ping); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d, want 413", code)
	}
}

func TestMCPRequiresAuth(t *testing.T) {
	s := newIdentityServer(t, "tok", false)
	if _, err := connect(t, s, nil); err == nil {
		t.Fatal("unauthenticated MCP connect succeeded")
	}
	cs := mustConnect(t, s, map[string]string{"Authorization": "Bearer tok"})
	if n := toolNames(t, cs); len(n) != 4 {
		t.Fatalf("ui token caller sees %v", n)
	}
}
