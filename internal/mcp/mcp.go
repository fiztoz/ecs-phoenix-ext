// Package mcp serves the extension's read operations as Model Context
// Protocol tools, using the official Go SDK over Streamable HTTP.
//
// The handler is stateless and answers with application/json: every client
// message is one HTTP POST with one JSON-RPC response, and no Mcp-Session-Id
// is issued. Any replica can answer any request and the Phoenix gateway
// needs no session affinity. GET and DELETE answer 405 (no server-initiated
// stream).
//
// Stateless mode builds one SDK server per request. That is where scopes are
// applied: the server for a request registers only the tools its caller's
// scopes allow, so tools/list and tools/call agree by construction, and the
// tool handlers close over the caller for audit logging. Schemas are
// inferred once and cached.
//
// Tools are thin wrappers over package ops — the same code path as the REST
// API.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fiztoz/ecs-phoenix-ext/internal/identity"
	"github.com/fiztoz/ecs-phoenix-ext/internal/ops"
)

// maxBody bounds one JSON-RPC message.
const maxBody = 1 << 20

// Server is the MCP endpoint. The caller must authenticate the request and
// store the principal with identity.WithPrincipal first; a request without
// one is refused.
type Server struct {
	ops     *ops.Service
	log     *slog.Logger
	impl    *mcp.Implementation
	cache   *mcp.SchemaCache
	tools   []toolSpec
	handler http.Handler
}

// New builds the endpoint.
func New(svc *ops.Service, log *slog.Logger, name, version string) *Server {
	s := &Server{
		ops:   svc,
		log:   log,
		impl:  &mcp.Implementation{Name: name, Title: "Dell ECS storage usage", Version: version},
		cache: mcp.NewSchemaCache(),
	}
	s.tools = s.toolTable()
	s.handler = mcp.NewStreamableHTTPHandler(s.serverFor, &mcp.StreamableHTTPOptions{
		Stateless:           true,
		JSONResponse:        true,
		MaxRequestBodyBytes: maxBody,
		// A browser on another origin must not drive the endpoint with a
		// user's cookie. Non-browser MCP clients send no Origin/Sec-Fetch-*.
		CrossOriginProtection: http.NewCrossOriginProtection(),
	})
	return s
}

// ServeHTTP implements the Streamable HTTP transport.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

const instructions = `Read-only view of one Dell ECS namespace, refreshed by a background poller (see polled_at; poll_ok=false means the numbers are the last good poll).
Sizes are bytes; ECS "GB" thresholds are binary (GiB).
Bucket quotas are ECS-native: block_size is the Block Access threshold (writes rejected at or above it), notification_size only alerts. quota_mode off means unlimited.
Activity is DERIVED from billing deltas between polls: write-side net movement only. Read/GET traffic is invisible, and overwrite-in-place or write-then-delete look idle. Never describe it as request rate or traffic.
activity.state: moved | idle (ECS republished, nothing moved) | unrefreshed (ECS has not republished) | unchanged (cannot tell idle from unrefreshed). A null activity means no baseline yet.`

// serverFor builds the per-request server holding only the caller's tools.
func (s *Server) serverFor(r *http.Request) *mcp.Server {
	who, ok := identity.FromContext(r.Context())
	if !ok {
		return nil
	}
	srv := mcp.NewServer(s.impl, &mcp.ServerOptions{Instructions: instructions, SchemaCache: s.cache})
	for _, t := range s.tools {
		if who.Has(t.scope) {
			t.register(srv, who)
		}
	}
	return srv
}

// toolSpec is one tool and the scope it needs.
type toolSpec struct {
	scope    string
	register func(srv *mcp.Server, who identity.Principal)
}

// addTool registers a typed read-only tool whose calls are audit-logged.
func addTool[In, Out any](s *Server, srv *mcp.Server, who identity.Principal, t *mcp.Tool,
	run func(ctx context.Context, in In) (Out, error)) {
	t.Annotations = readOnly(t.Title)
	mcp.AddTool(srv, t, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		start := time.Now()
		out, err := run(ctx, in)
		s.log.Info("mcp tool call", "tool", t.Name, "subject", who.Subject, "name", who.Name,
			"kind", who.Kind, "ok", err == nil, "duration_ms", time.Since(start).Milliseconds())
		return nil, out, err
	})
}

func readOnly(title string) *mcp.ToolAnnotations {
	no := false
	return &mcp.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    true,
		IdempotentHint:  true,
		DestructiveHint: &no,
		OpenWorldHint:   &no,
	}
}

type noArgs struct{}

type listArgs struct {
	Sort        string `json:"sort,omitempty" jsonschema:"used and objects sort biggest first; used_percent and activity put unknown values last"`
	Prefix      string `json:"prefix,omitempty" jsonschema:"only buckets whose name starts with this"`
	AtQuotaOnly bool   `json:"at_quota_only,omitempty" jsonschema:"only buckets at or over their Block Access threshold"`
	HotOnly     bool   `json:"hot_only,omitempty" jsonschema:"only buckets with write movement in most measured hours"`
	Limit       int    `json:"limit,omitempty" jsonschema:"maximum buckets returned"`
}

type bucketArgs struct {
	Bucket string `json:"bucket" jsonschema:"exact bucket name"`
}

type historyArgs struct {
	Bucket string `json:"bucket" jsonschema:"exact bucket name"`
	Hours  int    `json:"hours,omitempty" jsonschema:"lookback in hours"`
}

// defaultListLimit keeps an unprompted list small enough for a model's context.
const defaultListLimit = 50

func (s *Server) toolTable() []toolSpec {
	listSchema := schemaFor[listArgs](func(p map[string]*jsonschema.Schema) {
		p["sort"].Enum = enum(ops.SortKeys)
		p["sort"].Default = rawJSON(ops.SortName)
		bound(p["limit"], 1, ops.MaxListLimit, defaultListLimit)
	})
	historySchema := schemaFor[historyArgs](func(p map[string]*jsonschema.Schema) {
		bound(p["hours"], 1, ops.MaxHistoryHours, 24)
	})

	return []toolSpec{
		{scope: identity.ScopeRead, register: func(srv *mcp.Server, who identity.Principal) {
			addTool(s, srv, who, &mcp.Tool{
				Name:  "ecs_namespace_summary",
				Title: "ECS namespace summary",
				Description: "Namespace-level storage usage, ECS-native namespace quota, poll health and bucket roll-up counts " +
					"(at quota, confirmed over quota, stale, hot). Start here.",
			}, func(context.Context, noArgs) (ops.NamespaceSummary, error) {
				return s.ops.Namespace(), nil
			})
		}},
		{scope: identity.ScopeRead, register: func(srv *mcp.Server, who identity.Principal) {
			addTool(s, srv, who, &mcp.Tool{
				Name:  "ecs_list_buckets",
				Title: "List ECS buckets",
				Description: "Buckets with usage, objects, ECS-native quota thresholds and derived write activity. " +
					"Check total/truncated before concluding a bucket is absent.",
				InputSchema: listSchema,
			}, func(_ context.Context, a listArgs) (ops.BucketList, error) {
				if a.Limit == 0 {
					a.Limit = defaultListLimit
				}
				return s.ops.ListBuckets(ops.ListOptions{Sort: a.Sort, Prefix: a.Prefix,
					AtQuotaOnly: a.AtQuotaOnly, HotOnly: a.HotOnly, Limit: a.Limit})
			})
		}},
		{scope: identity.ScopeRead, register: func(srv *mcp.Server, who identity.Principal) {
			addTool(s, srv, who, &mcp.Tool{
				Name:        "ecs_get_bucket",
				Title:       "Get one ECS bucket",
				Description: "One bucket's usage, ECS-native quota thresholds, quota alarm state and derived write activity.",
			}, func(_ context.Context, a bucketArgs) (ops.Bucket, error) {
				return s.ops.Bucket(a.Bucket)
			})
		}},
		{scope: identity.ScopeRead, register: func(srv *mcp.Server, who identity.Principal) {
			addTool(s, srv, who, &mcp.Tool{
				Name:  "ecs_bucket_activity_history",
				Title: "ECS bucket activity history",
				Description: "Hourly derived write activity for one bucket (objects/bytes added and removed, in-flight multipart parts). " +
					"Only measured hours are listed; a missing hour means no observation, not idleness. Reads are never visible.",
				InputSchema: historySchema,
			}, func(ctx context.Context, a historyArgs) (ops.ActivityHistory, error) {
				return s.ops.ActivityHistory(ctx, a.Bucket, a.Hours)
			})
		}},
	}
}

// schemaFor infers T's input schema once and lets the caller add the
// constraints struct tags cannot express (enums, bounds, defaults). The SDK
// validates arguments against it before a handler runs.
func schemaFor[T any](edit func(props map[string]*jsonschema.Schema)) *jsonschema.Schema {
	sch, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("mcp: infer schema for %T: %v", *new(T), err))
	}
	edit(sch.Properties)
	return sch
}

func bound(p *jsonschema.Schema, lo, hi, def int) {
	l, h := float64(lo), float64(hi)
	p.Minimum, p.Maximum = &l, &h
	p.Default = rawJSON(def)
}

func enum(vals []string) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = v
	}
	return out
}

func rawJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
