package http

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"

	"github.com/fiztoz/ecs-phoenix-ext/internal/identity"
	"github.com/fiztoz/ecs-phoenix-ext/internal/ops"
)

// callerAuth resolves who is calling /api/v1 and /mcp, then requires scope
// (empty scope: any authenticated caller). In order:
//
//  1. X-Phoenix-Identity present: it must verify against
//     PHOENIX_IDENTITY_SECRET, and its scopes are the caller's scopes. A
//     present-but-bad header is a 401, never a fallback, and so is the header
//     arriving while no secret is configured — a misconfigured gateway must
//     fail loudly instead of silently downgrading.
//  2. UI_TOKEN set: Bearer or the session cookie grants read. Query
//     parameters are not accepted here; API clients have headers.
//  3. Neither PHOENIX_IDENTITY_SECRET nor UI_TOKEN set: open read, matching
//     the legacy /api/buckets behaviour.
//
// With only PHOENIX_IDENTITY_SECRET set, direct (un-proxied) calls are 401.
func (s *Server) callerAuth(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var who identity.Principal
		switch raw := r.Header.Get(identity.Header); {
		case raw != "":
			if s.deps.Identity == nil {
				s.deps.Log.Warn("phoenix identity header refused: PHOENIX_IDENTITY_SECRET is not set")
				unauthorized(w)
				return
			}
			p, err := s.deps.Identity.Verify(raw)
			if err != nil {
				s.deps.Log.Info("phoenix identity rejected", "err", err)
				unauthorized(w)
				return
			}
			who = p
		case s.deps.UIToken != "":
			if !s.uiCredentialOK(r) {
				unauthorized(w)
				return
			}
			who = identity.Principal{Subject: "ui-token", Kind: "ui_token", Scopes: []string{identity.ScopeRead}}
		case s.deps.Identity == nil:
			who = identity.Principal{Subject: "anonymous", Kind: "anonymous", Scopes: []string{identity.ScopeRead}}
		default:
			unauthorized(w)
			return
		}
		if scope != "" && !who.Has(scope) {
			writeJSONErr(w, http.StatusForbidden, "missing scope: "+scope)
			return
		}
		next(w, r.WithContext(identity.WithPrincipal(r.Context(), who)))
	}
}

// uiCredentialOK reports a valid UI_TOKEN as Bearer or session cookie.
func (s *Server) uiCredentialOK(r *http.Request) bool {
	want := []byte(s.deps.UIToken)
	if c, err := r.Cookie(uiCookieName); err == nil && subtle.ConstantTimeCompare([]byte(c.Value), want) == 1 {
		return true
	}
	got := bearerToken(r)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), want) == 1
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="ecs-phoenix-ext"`)
	writeJSONErr(w, http.StatusUnauthorized, "unauthorized")
}

func (s *Server) handleV1WhoAmI(w http.ResponseWriter, r *http.Request) {
	who, _ := identity.FromContext(r.Context())
	scopes := who.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"subject": who.Subject,
		"name":    who.Name,
		"kind":    who.Kind,
		"scopes":  scopes,
	})
}

func (s *Server) handleV1Namespace(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.ops.Namespace())
}

// handleV1Buckets lists buckets. Query: sort (ops.SortKeys), prefix,
// at_quota=true, hot=true, limit (1..1000).
func (s *Server) handleV1Buckets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := ops.ListOptions{Sort: q.Get("sort"), Prefix: q.Get("prefix")}
	var err error
	if opts.AtQuotaOnly, err = boolParam(q.Get("at_quota")); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "at_quota must be true or false")
		return
	}
	if opts.HotOnly, err = boolParam(q.Get("hot")); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "hot must be true or false")
		return
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > ops.MaxListLimit {
			writeJSONErr(w, http.StatusBadRequest, "limit must be between 1 and 1000")
			return
		}
		opts.Limit = n
	}
	list, err := s.ops.ListBuckets(opts)
	if err != nil {
		writeJSONErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleV1Bucket(w http.ResponseWriter, r *http.Request) {
	b, err := s.ops.Bucket(r.PathValue("bucket"))
	if err != nil {
		writeJSONErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// handleV1BucketActivity returns hourly derived activity. Query: hours
// (1..2160, default 24).
func (s *Server) handleV1BucketActivity(w http.ResponseWriter, r *http.Request) {
	hours := 24
	if v := r.URL.Query().Get("hours"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > ops.MaxHistoryHours {
			writeJSONErr(w, http.StatusBadRequest, "hours must be between 1 and 2160")
			return
		}
		hours = n
	}
	h, err := s.ops.ActivityHistory(r.Context(), r.PathValue("bucket"), hours)
	switch {
	case errors.Is(err, ops.ErrNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case err != nil:
		s.deps.Log.Error("activity history", "err", err)
		writeJSONErr(w, http.StatusInternalServerError, "internal error")
	default:
		writeJSON(w, http.StatusOK, h)
	}
}

func boolParam(v string) (bool, error) {
	if v == "" {
		return false, nil
	}
	return strconv.ParseBool(v)
}
