package identity

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func newTestVerifier(t *testing.T, now time.Time) *Verifier {
	t.Helper()
	v, err := NewVerifier(testSecret, "ecs-phoenix-ext")
	if err != nil {
		t.Fatal(err)
	}
	v.SetClock(func() time.Time { return now })
	return v
}

func TestVerifyRoundTrip(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(t, now)
	tok := Sign(testSecret, "ecs-phoenix-ext",
		Principal{Subject: "42", Name: "alice", Kind: "api_key", Scopes: []string{"read", "write"}}, now, time.Minute)
	p, err := v.Verify(tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.Subject != "42" || p.Name != "alice" || p.Kind != "api_key" || !p.Has("read") || !p.Has("write") || p.Has("admin") {
		t.Fatalf("principal = %+v", p)
	}
}

func TestVerifyRejects(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(t, now)
	good := Principal{Subject: "42", Scopes: []string{"read"}}
	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	valid := Sign(testSecret, "ecs-phoenix-ext", good, now, time.Minute)
	body := strings.Split(valid, ".")[1]

	cases := map[string]string{
		"wrong secret":    Sign(strings.Repeat("x", 32), "ecs-phoenix-ext", good, now, time.Minute),
		"wrong audience":  Sign(testSecret, "kafka-phoenix-ext", good, now, time.Minute),
		"expired":         Sign(testSecret, "ecs-phoenix-ext", good, now.Add(-10*time.Minute), time.Minute),
		"future":          Sign(testSecret, "ecs-phoenix-ext", good, now.Add(10*time.Minute), time.Minute),
		"long-lived":      Sign(testSecret, "ecs-phoenix-ext", good, now, time.Hour),
		"no subject":      Sign(testSecret, "ecs-phoenix-ext", Principal{Scopes: []string{"read"}}, now, time.Minute),
		"alg none":        none + "." + body + ".",
		"tampered claims": strings.Replace(valid, body, body+"x", 1),
		"garbage":         "not-a-token",
	}
	for name, tok := range cases {
		if _, err := v.Verify(tok); err == nil {
			t.Errorf("%s: Verify accepted the token", name)
		}
	}
}

func TestVerifyLeeway(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	v := newTestVerifier(t, now)
	// Expired 10s ago: inside the clock-skew leeway.
	tok := Sign(testSecret, "ecs-phoenix-ext", Principal{Subject: "1", Scopes: []string{"read"}},
		now.Add(-70*time.Second), time.Minute)
	if _, err := v.Verify(tok); err != nil {
		t.Fatalf("Verify within leeway: %v", err)
	}
}

func TestNewVerifierValidates(t *testing.T) {
	if _, err := NewVerifier("short", "ecs-phoenix-ext"); err == nil {
		t.Error("short secret accepted")
	}
	if _, err := NewVerifier(testSecret, " "); err == nil {
		t.Error("empty audience accepted")
	}
}

// goldenToken is the cross-repo contract vector, built outside Go (Python
// hmac/json): secret testSecret, aud ecs-phoenix-ext, sub 42, scope read,
// iat 1793534400, exp iat+60. The Phoenix gateway's tests must produce a
// token this verifier accepts from the same inputs; keep the two in step.
const goldenToken = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
	"eyJpc3MiOiJ1cHRpbWUtcGhvZW5peCIsImF1ZCI6ImVjcy1waG9lbml4LWV4dCIsInN1YiI6IjQyIiwibmFtZSI6ImFsaWNlIiwia2luZCI6ImFwaV9rZXkiLCJzY29wZSI6InJlYWQiLCJpYXQiOjE3OTM1MzQ0MDAsImV4cCI6MTc5MzUzNDQ2MH0." +
	"vi5rjKLJaV4ExsI56kWYFmofsmzfe1fusNVKZtYKqNU"

func TestVerifyGoldenVector(t *testing.T) {
	v := newTestVerifier(t, time.Unix(1793534410, 0))
	p, err := v.Verify(goldenToken)
	if err != nil {
		t.Fatalf("golden vector rejected: %v", err)
	}
	if p.Subject != "42" || p.Name != "alice" || p.Kind != "api_key" || !p.Has(ScopeRead) {
		t.Fatalf("golden principal = %+v", p)
	}
}
