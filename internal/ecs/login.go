package ecs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	tokenHeader = "X-SDS-AUTH-TOKEN"
	// ECS tokens live ~8h; re-login proactively at 7h.
	tokenMaxAge = 7 * time.Hour
)

// Client is a minimal Dell ECS management API client with a single cached
// token. Credentials come from the environment and are never logged.
type Client struct {
	baseURL  string
	username string
	userpass string
	sizeUnit string
	log      func(msg string, args ...any)

	http *http.Client

	mu      sync.Mutex
	token   string
	tokenAt time.Time

	// now is injectable for tests.
	now func() time.Time
}

// ClientConfig carries construction options for Client. Values are supplied
// by the caller from environment variables / Kubernetes Secrets.
type ClientConfig struct {
	BaseURL  string
	Username string
	// Userpass is the management user's credential, injected from the
	// environment; it is never written to logs or responses.
	Userpass string
	SizeUnit string
	Timeout  time.Duration
	CAFile   string // path to PEM
	CAInline string // inline PEM
	Insecure bool   // lab only
	Logger   func(msg string, args ...any)
}

// NewClient builds a Client with TLS configured from the given options.
func NewClient(cfg ClientConfig) (*Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CAFile != "" || cfg.CAInline != "" {
		pool := x509.NewCertPool()
		if cfg.CAFile != "" {
			pem, err := os.ReadFile(cfg.CAFile)
			if err != nil {
				return nil, fmt.Errorf("ecs: read ECS_TLS_CA_FILE: %w", err)
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("ecs: ECS_TLS_CA_FILE contains no usable PEM")
			}
		}
		if cfg.CAInline != "" {
			if !pool.AppendCertsFromPEM([]byte(cfg.CAInline)) {
				return nil, fmt.Errorf("ecs: ECS_TLS_CA contains no usable PEM")
			}
		}
		tlsCfg.RootCAs = pool
	}
	if cfg.Insecure {
		tlsCfg.InsecureSkipVerify = true
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	sizeUnit := cfg.SizeUnit
	if sizeUnit == "" {
		sizeUnit = "KB"
	}
	logf := cfg.Logger
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Client{
		baseURL:  cfg.BaseURL,
		username: cfg.Username,
		userpass: cfg.Userpass,
		sizeUnit: sizeUnit,
		log:      logf,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				TLSClientConfig: tlsCfg,
			},
			// Billing calls must not silently follow redirects: an expired
			// token can 302 to /login and we must detect that.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now: time.Now,
	}, nil
}

// Login performs GET /login with HTTP Basic auth and caches the token
// returned in the X-SDS-AUTH-TOKEN response header (the body is not the
// credential).
func (c *Client) Login(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/login", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.username, c.userpass)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ecs: login: %w", err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)); err != nil {
		return fmt.Errorf("ecs: login: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ecs: login returned HTTP %d", resp.StatusCode)
	}
	tok := resp.Header.Get(tokenHeader)
	if tok == "" {
		return fmt.Errorf("ecs: login response missing %s header", tokenHeader)
	}
	c.mu.Lock()
	c.token = tok
	c.tokenAt = c.now()
	c.mu.Unlock()
	return nil
}

// Token returns a cached token, logging in when missing or near expiry.
func (c *Client) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	tok, at := c.token, c.tokenAt
	c.mu.Unlock()
	if tok != "" && c.now().Sub(at) < tokenMaxAge {
		return tok, nil
	}
	if err := c.Login(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token, nil
}

// invalidateToken forces a re-login on the next Token() call.
func (c *Client) invalidateToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

// Logout releases the cached token. It never sends ?force=true (that would
// kill every session for the user, including humans in the ECS UI).
func (c *Client) Logout(ctx context.Context) error {
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/logout", nil)
	if err != nil {
		return err
	}
	req.Header.Set(tokenHeader, tok)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ecs: logout: %w", err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)); err != nil {
		return fmt.Errorf("ecs: logout: %w", err)
	}
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("ecs: logout returned HTTP %d", resp.StatusCode)
	}
	return nil
}
