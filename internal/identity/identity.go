// Package identity verifies the caller identity Uptime Phoenix attaches when
// it proxies a request to this extension, and carries the resulting
// principal through a request context.
//
// Phoenix owns users, API keys and capabilities; this extension owns ECS. The
// Phoenix extension gateway authenticates the caller, checks its extension
// scopes, and forwards the request with a short-lived HS256 JWT in the
// X-Phoenix-Identity header, signed with a secret shared only between
// Phoenix and this extension. The extension trusts nothing else in the
// request about who the caller is.
//
// Claims (all required unless noted):
//
//	iss   "uptime-phoenix"
//	aud   the extension id (PHOENIX_EXTENSION_ID)
//	sub   Phoenix user id, as a string
//	name  Phoenix username (optional, for logs)
//	kind  "session" | "api_key" (optional, for logs)
//	scope space-separated extension-local scopes, e.g. "read"
//	iat   issued at (unix seconds)
//	exp   expiry (unix seconds); at most MaxLifetime after iat
//
// Only HS256 is accepted. The token is per request: a long-lived one is
// refused even when correctly signed, so a leaked header is a short replay
// window rather than a credential.
package identity

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Header carries the Phoenix-signed identity token.
const Header = "X-Phoenix-Identity"

// Issuer is the only accepted iss claim.
const Issuer = "uptime-phoenix"

// MinSecretLen is the shortest accepted shared secret, in bytes.
const MinSecretLen = 32

// MaxLifetime bounds exp - iat.
const MaxLifetime = 5 * time.Minute

// Leeway absorbs clock skew between the Phoenix and extension pods.
const Leeway = 30 * time.Second

// Scopes this extension understands. Phoenix maps its own grants onto these
// before signing; the extension never sees Phoenix-wide scopes.
const (
	ScopeRead = "read"
)

// Principal is an authenticated caller.
type Principal struct {
	Subject string   // Phoenix user id, or "ui-token" / "anonymous" for the local fallbacks
	Name    string   // display name for logs; may be empty
	Kind    string   // session | api_key | ui_token | anonymous
	Scopes  []string // extension-local scopes
}

// Has reports whether the principal holds scope.
func (p Principal) Has(scope string) bool {
	return slices.Contains(p.Scopes, scope)
}

// Verifier checks X-Phoenix-Identity tokens.
type Verifier struct {
	secret   []byte
	audience string
	now      func() time.Time
}

// NewVerifier builds a verifier for tokens addressed to audience.
func NewVerifier(secret, audience string) (*Verifier, error) {
	if len(secret) < MinSecretLen {
		return nil, fmt.Errorf("identity: secret must be at least %d bytes", MinSecretLen)
	}
	if strings.TrimSpace(audience) == "" {
		return nil, errors.New("identity: audience must not be empty")
	}
	return &Verifier{secret: []byte(secret), audience: audience, now: time.Now}, nil
}

// SetClock replaces the verifier clock (tests).
func (v *Verifier) SetClock(now func() time.Time) { v.now = now }

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type claims struct {
	Iss   string          `json:"iss"`
	Aud   json.RawMessage `json:"aud"` // string or array of strings
	Sub   string          `json:"sub"`
	Name  string          `json:"name"`
	Kind  string          `json:"kind"`
	Scope string          `json:"scope"`
	Iat   int64           `json:"iat"`
	Exp   int64           `json:"exp"`
}

// ErrInvalid wraps every verification failure. Callers answer 401 and must
// not echo the detail to the client.
var ErrInvalid = errors.New("identity: invalid token")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// Verify checks token and returns its principal.
func (v *Verifier) Verify(token string) (Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Principal{}, invalid("not a compact JWT")
	}
	var h header
	if err := decodeSegment(parts[0], &h); err != nil {
		return Principal{}, invalid("header: %v", err)
	}
	if h.Alg != "HS256" {
		return Principal{}, invalid("alg %q not accepted", h.Alg)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Principal{}, invalid("signature encoding")
	}
	mac := hmac.New(sha256.New, v.secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return Principal{}, invalid("bad signature")
	}

	var c claims
	if err := decodeSegment(parts[1], &c); err != nil {
		return Principal{}, invalid("claims: %v", err)
	}
	if c.Iss != Issuer {
		return Principal{}, invalid("iss %q", c.Iss)
	}
	if !audienceMatches(c.Aud, v.audience) {
		return Principal{}, invalid("aud does not name %q", v.audience)
	}
	if c.Sub == "" {
		return Principal{}, invalid("missing sub")
	}
	if c.Iat == 0 || c.Exp == 0 {
		return Principal{}, invalid("missing iat/exp")
	}
	iat, exp := time.Unix(c.Iat, 0), time.Unix(c.Exp, 0)
	now := v.now()
	switch {
	case exp.Sub(iat) > MaxLifetime:
		return Principal{}, invalid("lifetime %s exceeds %s", exp.Sub(iat), MaxLifetime)
	case now.After(exp.Add(Leeway)):
		return Principal{}, invalid("expired")
	case iat.After(now.Add(Leeway)):
		return Principal{}, invalid("issued in the future")
	}

	kind := c.Kind
	if kind == "" {
		kind = "session"
	}
	return Principal{Subject: c.Sub, Name: c.Name, Kind: kind, Scopes: strings.Fields(c.Scope)}, nil
}

func decodeSegment(seg string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func audienceMatches(raw json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return slices.Contains(many, want)
	}
	return false
}

// Sign mints a token the way the Phoenix gateway must. It exists so tests
// and local tooling can exercise the verifier end to end; production tokens
// come from Phoenix.
func Sign(secret string, aud string, p Principal, iat time.Time, ttl time.Duration) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	head := enc(header{Alg: "HS256", Typ: "JWT"})
	body := enc(map[string]any{
		"iss":   Issuer,
		"aud":   aud,
		"sub":   p.Subject,
		"name":  p.Name,
		"kind":  p.Kind,
		"scope": strings.Join(p.Scopes, " "),
		"iat":   iat.Unix(),
		"exp":   iat.Add(ttl).Unix(),
	})
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(head + "." + body))
	return head + "." + body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

type ctxKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the principal stored by WithPrincipal.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
