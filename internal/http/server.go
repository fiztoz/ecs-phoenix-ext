// Package http serves the ecs-phoenix-ext dashboard, wallboard, JSON API and
// health endpoints. It renders server-side Go templates — no JS framework.
package http

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/fiztoz/ecs-phoenix-ext/internal/identity"
	"github.com/fiztoz/ecs-phoenix-ext/internal/mcp"
	"github.com/fiztoz/ecs-phoenix-ext/internal/ops"
	"github.com/fiztoz/ecs-phoenix-ext/internal/poller"
	"github.com/fiztoz/ecs-phoenix-ext/internal/store"
)

//go:embed views/dashboard.html views/wallboard.html views/icon.svg
var assets embed.FS

// SnapshotSource is what the HTTP layer reads from the poller.
type SnapshotSource interface {
	Snapshot() poller.Snapshot
	StaleThreshold() time.Duration
}

// Deps wires the server.
type Deps struct {
	Namespace string
	BasePath  string // e.g. "/storage"; "/" for local dev
	UIToken   string // empty = UI open (health is always open)
	// Identity verifies X-Phoenix-Identity from the Phoenix extension
	// gateway; nil means the header is refused (PHOENIX_IDENTITY_SECRET
	// unset). See callerAuth.
	Identity *identity.Verifier
	Version  string // reported as MCP serverInfo.version
	// ExtensionID is this extension's id in Phoenix's catalog; the usage
	// guide builds the gateway URL from it.
	ExtensionID string
	Snapshots   SnapshotSource
	Store       store.Store
	Log         *slog.Logger
}

// Server is the ecs-phoenix-ext HTTP server.
type Server struct {
	deps Deps
	tmpl *template.Template
	icon []byte
	ops  *ops.Service
	mcp  *mcp.Server
}

// New parses templates and builds the server.
func New(deps Deps) (*Server, error) {
	funcs := template.FuncMap{
		"bytes": HumanBytes,
		"pct": func(p *float64) string {
			if p == nil {
				return "—"
			}
			return fmt.Sprintf("%.1f%%", *p)
		},
		"ts": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.UTC().Format("2006-01-02 15:04 UTC")
		},
		"age": func(t time.Time, now time.Time) string {
			if t.IsZero() {
				return "—"
			}
			d := now.Sub(t)
			switch {
			case d < time.Minute:
				return fmt.Sprintf("%ds", int(d.Seconds()))
			case d < time.Hour:
				return fmt.Sprintf("%dm", int(d.Minutes()))
			case d < 24*time.Hour:
				return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
			default:
				// ECS can report metering current only to a timestamp days
				// old; "51h20m" hides that, "2d3h" does not.
				return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
			}
		},
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(assets, "views/*.html")
	if err != nil {
		return nil, fmt.Errorf("http: parse templates: %w", err)
	}
	icon, err := assets.ReadFile("views/icon.svg")
	if err != nil {
		return nil, fmt.Errorf("http: read icon: %w", err)
	}
	svc := &ops.Service{DefaultNamespace: deps.Namespace, Snapshots: deps.Snapshots, Store: deps.Store}
	version := deps.Version
	if version == "" {
		version = "dev"
	}
	return &Server{deps: deps, tmpl: tmpl, icon: icon, ops: svc,
		mcp: mcp.New(svc, deps.Log, "ecs-phoenix-ext", version)}, nil
}

// Handler returns the mux with routes registered under BASE_PATH and, when
// BASE_PATH != "/", also at the root (local `go run` convenience and so
// probes work with or without the Ingress prefix).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.register(mux, s.deps.BasePath)
	if s.deps.BasePath != "/" {
		s.register(mux, "/")
	}
	return mux
}

func (s *Server) register(mux *http.ServeMux, prefix string) {
	p := strings.TrimRight(prefix, "/")

	// Open routes.
	mux.HandleFunc("GET "+p+"/icon.svg", s.securityHeaders(s.handleIcon))
	mux.HandleFunc("GET "+p+"/health/live", s.securityHeaders(s.handleLive))
	mux.HandleFunc("GET "+p+"/health/ready", s.securityHeaders(s.handleReady))
	mux.HandleFunc("GET "+p+"/health/quota", s.securityHeaders(s.handleQuotaAll))
	mux.HandleFunc("GET "+p+"/health/quota/{bucket}", s.securityHeaders(s.handleQuotaBucket))

	// UI-token-guarded routes (open when UI_TOKEN is empty).
	mux.HandleFunc("GET "+p+"/", s.securityHeaders(s.uiAuth(s.handleDashboard)))
	mux.HandleFunc("GET "+p+"/wallboard", s.securityHeaders(s.uiAuth(s.handleWallboard)))
	mux.HandleFunc("GET "+p+"/api/buckets", s.securityHeaders(s.uiAuth(s.handleAPIBuckets)))

	// Programmatic API and MCP: per-caller scopes (see callerAuth).
	read := func(h http.HandlerFunc) http.HandlerFunc {
		return s.securityHeaders(s.callerAuth(identity.ScopeRead, h))
	}
	mux.HandleFunc("GET "+p+"/api/v1/whoami", read(s.handleV1WhoAmI))
	mux.HandleFunc("GET "+p+"/api/v1/namespace", read(s.handleV1Namespace))
	mux.HandleFunc("GET "+p+"/api/v1/buckets", read(s.handleV1Buckets))
	mux.HandleFunc("GET "+p+"/api/v1/buckets/{bucket}", read(s.handleV1Bucket))
	mux.HandleFunc("GET "+p+"/api/v1/buckets/{bucket}/activity", read(s.handleV1BucketActivity))
	// MCP checks per-tool scopes itself; callerAuth only requires a caller.
	// GET/DELETE reach the handler so it can answer the transport's 405.
	mcpHandler := s.securityHeaders(s.callerAuth("", s.mcp.ServeHTTP))
	for _, m := range []string{"POST", "GET", "DELETE"} {
		mux.HandleFunc(m+" "+p+"/mcp", mcpHandler)
	}
}

// securityHeaders sets the locked header set on every response.
// frame-ancestors 'self' lets a same-host Phoenix admin iframe work and
// blocks random sites. Never '*' in v1.
func (s *Server) securityHeaders(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "frame-ancestors 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next(w, r)
	}
}

// uiAuth guards dashboard/API/form routes when UI_TOKEN is configured.
// Health endpoints are deliberately NOT behind this guard: Phoenix HTTP
// monitors hit them without a Bearer token.
//
// Credential hand-off: a valid Authorization: Bearer or ui_token parameter
// (the form Phoenix's gated /frame redirect uses) is exchanged for a session
// cookie, because the extension's own links and form posts cannot carry the
// token forward. The iframe embedding Phoenix is same-host, so the cookie is
// first-party; SameSite=Lax keeps cross-site top-level POSTs from replaying
// it.
func (s *Server) uiAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want := s.deps.UIToken
		if want == "" {
			next(w, r)
			return
		}
		// Session cookie from an earlier hand-off.
		if c, err := r.Cookie(uiCookieName); err == nil &&
			subtle.ConstantTimeCompare([]byte(c.Value), []byte(want)) == 1 {
			next(w, r)
			return
		}
		got := bearerToken(r)
		if got == "" {
			if err := r.ParseForm(); err == nil {
				got = r.Form.Get("ui_token")
			}
		}
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			if strings.HasPrefix(r.URL.Path, "/api/") || strings.Contains(r.URL.Path, "/api/") ||
				strings.Contains(r.Header.Get("Accept"), "application/json") {
				writeJSONErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Valid credential: swap it for the session cookie so follow-up
		// navigations inside the extension stay authenticated without the
		// token reappearing in URLs.
		http.SetCookie(w, &http.Cookie{
			Name:     uiCookieName,
			Value:    want,
			Path:     uiCookiePath(s.deps.BasePath),
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   12 * 60 * 60,
		})
		next(w, r)
	}
}

// uiCookieName is the session cookie set after a successful UI_TOKEN hand-off.
const uiCookieName = "ecs_ui_session"

// uiCookiePath scopes the session cookie to the extension's Ingress prefix so
// it is never sent to Phoenix or sibling extensions on the same host.
func uiCookiePath(basePath string) string {
	p := strings.TrimRight(basePath, "/")
	if p == "" {
		return "/"
	}
	return p
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// --- health ---

func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	snap := s.deps.Snapshots.Snapshot()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if snap.PollOK {
		_, _ = w.Write([]byte("ok"))
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	msg := "last poll failed"
	if snap.LastError != "" {
		msg = snap.LastError
	}
	_, _ = w.Write([]byte(msg))
}

func (s *Server) handleQuotaAll(w http.ResponseWriter, _ *http.Request) {
	snap := s.deps.Snapshots.Snapshot()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if snap.NamespaceConfirmedOver {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, "namespace over quota (%d consecutive samples)", snap.NamespaceOverStreak)
		return
	}
	for _, b := range snap.Buckets {
		if b.ConfirmedOver {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, "bucket %s over quota (%d consecutive samples)", b.Name, b.OverStreak)
			return
		}
	}
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleQuotaBucket(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("bucket")
	snap := s.deps.Snapshots.Snapshot()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, b := range snap.Buckets {
		if b.Name == name {
			if b.ConfirmedOver {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprintf(w, "bucket %s over quota", name)
				return
			}
			_, _ = w.Write([]byte("ok"))
			return
		}
	}
	// Unknown bucket: 404 without leaking other namespaces.
	http.Error(w, "unknown bucket", http.StatusNotFound)
}

// --- icon ---

func (s *Server) handleIcon(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	_, _ = w.Write(s.icon)
}

// --- JSON API ---

// The legacy /api/buckets wire types live in package ops so REST v1 and MCP
// serve the identical shapes.
type (
	apiBucket   = ops.Bucket
	apiActivity = ops.Activity
)

func (s *Server) handleAPIBuckets(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.ops.Report())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
