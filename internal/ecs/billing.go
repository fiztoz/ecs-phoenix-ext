package ecs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxBillingPages guards the pagination loop. Overflow is a poll error.
const maxBillingPages = 100

// NamespaceBilling polls the namespace billing endpoint, following
// pagination, and returns the merged payload.
//
// Locked URL:
//
//	GET /object/billing/namespace/{ns}/info?include_bucket_detail=true&sizeunit={unit}
func (c *Client) NamespaceBilling(ctx context.Context, namespace string) (*BillingPayload, error) {
	var merged *BillingPayload
	seenMarkers := make(map[string]bool)
	marker := ""
	for page := 0; ; page++ {
		if page >= maxBillingPages {
			return nil, fmt.Errorf("ecs: billing pagination exceeded %d pages", maxBillingPages)
		}
		p, err := c.fetchBillingPage(ctx, namespace, marker)
		if err != nil {
			return nil, err
		}
		if merged == nil {
			merged = p
		} else {
			merged.Buckets = append(merged.Buckets, p.Buckets...)
			if p.NextMarker != "" {
				merged.NextMarker = p.NextMarker
			}
		}
		if p.NextMarker == "" || seenMarkers[p.NextMarker] {
			merged.NextMarker = ""
			return merged, nil
		}
		seenMarkers[p.NextMarker] = true
		marker = p.NextMarker
	}
}

// fetchBillingPage fetches one page, handling token refresh (401 or
// redirect-to-/login exactly once) and the .json suffix fallback (on 406
// or HTML responses, exactly once).
func (c *Client) fetchBillingPage(ctx context.Context, namespace, marker string) (*BillingPayload, error) {
	useJSONSuffix := false
	for attempt := 0; ; attempt++ {
		if attempt > 1 {
			return nil, fmt.Errorf("ecs: billing poll failed after retry")
		}
		body, ct, status, loc, err := c.doBillingRequest(ctx, namespace, marker, useJSONSuffix)
		if err != nil {
			return nil, err
		}
		switch {
		case status == http.StatusUnauthorized:
			// Token expired: re-login once, then retry exactly once.
			c.invalidateToken()
			c.log("ecs: billing 401, re-login and retry")
			continue
		case status >= 300 && status < 400 && strings.Contains(loc, "/login"):
			// Expired tokens may redirect to /login on first use.
			c.invalidateToken()
			c.log("ecs: billing redirected to /login, re-login and retry")
			continue
		case status == http.StatusNotAcceptable || strings.Contains(strings.ToLower(ct), "text/html"):
			if !useJSONSuffix {
				useJSONSuffix = true
				c.log("ecs: billing returned HTTP %d / HTML, retrying with .json suffix", status)
				continue
			}
			return nil, fmt.Errorf("ecs: billing returned HTTP %d even with .json suffix", status)
		case status != http.StatusOK:
			return nil, fmt.Errorf("ecs: billing returned HTTP %d", status)
		}
		return ParseBilling(body, ct)
	}
}

// doBillingRequest executes one GET and returns body, content type, status,
// Location header and error.
func (c *Client) doBillingRequest(ctx context.Context, namespace, marker string, jsonSuffix bool) ([]byte, string, int, string, error) {
	tok, err := c.Token(ctx)
	if err != nil {
		return nil, "", 0, "", err
	}
	suffix := ""
	if jsonSuffix {
		suffix = ".json"
	}
	u := fmt.Sprintf("%s/object/billing/namespace/%s/info%s",
		c.baseURL, url.PathEscape(namespace), suffix)
	q := url.Values{}
	q.Set("include_bucket_detail", "true")
	q.Set("sizeunit", c.sizeUnit)
	if marker != "" {
		q.Set("marker", marker)
	}
	u += "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", 0, "", err
	}
	req.Header.Set(tokenHeader, tok)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", 0, "", fmt.Errorf("ecs: billing request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, "", 0, "", fmt.Errorf("ecs: billing body: %w", err)
	}
	return body, resp.Header.Get("Content-Type"), resp.StatusCode, resp.Header.Get("Location"), nil
}
