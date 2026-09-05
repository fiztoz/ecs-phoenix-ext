package ecs

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Inventory types are the *necessary* subset of the Dell ECS management API,
// not the full object dump. Bucket rows carry block_size and
// notification_size; namespace rows carry default_bucket_block_size plus
// namespace-level blockSize/notificationSize. Real ECS answers are XML with
// root <object_buckets> (rows in <object_bucket>) and <namespace>; an unset
// threshold is "-1" (also treated as unset: 0/missing/null).
//
// Sizes are byte counts. If lab traffic ever shows small values that are
// clearly MB, fix the multiplier in exactly one place: the parse helpers below.

// BucketMeta is the necessary per-bucket inventory: the ECS-native quota
// thresholds (see the ECS UI Quota panel: "Block Access at" + "Send
// Notification at") plus the identity needed to tell buckets apart.
// Everything else (tags, search metadata, ACLs, …) is dropped.
//
// Quota modes from the UI map onto the two thresholds as:
//
//	Off                           → neither set
//	Notification Only at X         → notify only
//	Block Access Only at X         → block only
//	Block Access at X + Notify at Y → both set
//
// A threshold of 0/missing/null all mean "unset" — ECS has no 0-byte quota.
type BucketMeta struct {
	Name             string
	Namespace        string
	BlockSize        int64 // bytes
	NotificationSize int64 // bytes
	HasBlock         bool
	HasNotify        bool
	Owner            string
	VPool            string
	APIType          string
}

// QuotaMode derives the ECS UI quota mode from the two thresholds.
// One of: "off", "notify-only", "block-only", "block-notify".
func (b BucketMeta) QuotaMode() string {
	switch {
	case b.HasBlock && b.HasNotify:
		return "block-notify"
	case b.HasBlock:
		return "block-only"
	case b.HasNotify:
		return "notify-only"
	default:
		return "off"
	}
}

// NamespaceMeta is the necessary namespace inventory: identity, the
// default block size new buckets inherit, and the namespace-level block /
// notify thresholds when ECS reports them (positive only; "-1" means unset).
type NamespaceMeta struct {
	Name                  string
	ID                    string
	DefaultBucketBlock    int64 // bytes
	HasDefaultBucketBlock bool
	BlockSize             int64 // namespace-level blockSize, bytes
	HasBlock              bool
	NotificationSize      int64 // namespace-level notificationSize, bytes
	HasNotify             bool
}

const maxInventoryPages = 100

// BucketMetas lists buckets in a namespace:
//
//	GET /object/bucket?namespace={ns}[&marker=…][&limit=…]
//
// It follows marker pagination like billing and returns metas keyed by
// bucket name. A missing bucket list is empty, not an error.
func (c *Client) BucketMetas(ctx context.Context, namespace string) (map[string]BucketMeta, error) {
	out := make(map[string]BucketMeta)
	seenMarkers := make(map[string]bool)
	marker := ""
	for page := 0; ; page++ {
		if page >= maxInventoryPages {
			return nil, fmt.Errorf("ecs: bucket list pagination exceeded %d pages", maxInventoryPages)
		}
		metas, next, err := c.fetchBucketPage(ctx, namespace, marker)
		if err != nil {
			return nil, err
		}
		for _, m := range metas {
			out[m.Name] = m
		}
		if next == "" || seenMarkers[next] {
			return out, nil
		}
		seenMarkers[next] = true
		marker = next
	}
}

// NamespaceMetaInfo fetches one namespace:
//
//	GET /object/namespaces/namespace/{ns}
func (c *Client) NamespaceMetaInfo(ctx context.Context, namespace string) (*NamespaceMeta, error) {
	for attempt := 0; ; attempt++ {
		if attempt > 1 {
			return nil, fmt.Errorf("ecs: namespace info failed after retry")
		}
		u := fmt.Sprintf("%s/object/namespaces/namespace/%s", c.baseURL, url.PathEscape(namespace))
		body, ct, status, loc, err := c.doAuthedGET(ctx, u)
		if err != nil {
			return nil, err
		}
		switch {
		case status == http.StatusUnauthorized:
			c.invalidateToken()
			c.log("ecs: namespace info 401, re-login and retry")
			continue
		case status >= 300 && status < 400 && strings.Contains(loc, "/login"):
			c.invalidateToken()
			c.log("ecs: namespace info redirected to /login, re-login and retry")
			continue
		case status != http.StatusOK:
			return nil, fmt.Errorf("ecs: namespace info returned HTTP %d", status)
		}
		return ParseNamespaceMeta(body, ct)
	}
}

func (c *Client) fetchBucketPage(ctx context.Context, namespace, marker string) ([]BucketMeta, string, error) {
	for attempt := 0; ; attempt++ {
		if attempt > 1 {
			return nil, "", fmt.Errorf("ecs: bucket list failed after retry")
		}
		u := fmt.Sprintf("%s/object/bucket?namespace=%s", c.baseURL, url.QueryEscape(namespace))
		if marker != "" {
			u += "&marker=" + url.QueryEscape(marker)
		}
		body, ct, status, loc, err := c.doAuthedGET(ctx, u)
		if err != nil {
			return nil, "", err
		}
		switch {
		case status == http.StatusUnauthorized:
			c.invalidateToken()
			c.log("ecs: bucket list 401, re-login and retry")
			continue
		case status >= 300 && status < 400 && strings.Contains(loc, "/login"):
			c.invalidateToken()
			c.log("ecs: bucket list redirected to /login, re-login and retry")
			continue
		case status != http.StatusOK:
			return nil, "", fmt.Errorf("ecs: bucket list returned HTTP %d", status)
		}
		return ParseBucketList(body, ct)
	}
}

// doAuthedGET executes one GET with the cached token and returns the raw
// body, content type, status and Location header. Redirects are NOT followed
// (an expired token can 302 to /login and the caller must detect that).
func (c *Client) doAuthedGET(ctx context.Context, u string) ([]byte, string, int, string, error) {
	tok, err := c.Token(ctx)
	if err != nil {
		return nil, "", 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", 0, "", err
	}
	req.Header.Set(tokenHeader, tok)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", 0, "", fmt.Errorf("ecs: inventory request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, "", 0, "", fmt.Errorf("ecs: inventory body: %w", err)
	}
	return body, resp.Header.Get("Content-Type"), resp.StatusCode, resp.Header.Get("Location"), nil
}

// --- parsing ---

// ParseBucketList parses one bucket-list page. JSON envelope keys are
// tolerant (object_bucket / buckets / bucket); per-bucket size keys accept
// snake_case and camelCase, number or numeric string. XML is accepted when
// the content type says XML or the body starts with '<'.
func ParseBucketList(body []byte, contentType string) ([]BucketMeta, string, error) {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) > 0 && trimmed[0] == '<' && !strings.Contains(strings.ToLower(contentType), "json") {
		return parseBucketListXML(trimmed)
	}
	return parseBucketListJSON(trimmed)
}

func parseBucketListJSON(body []byte) ([]BucketMeta, string, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var env map[string]json.RawMessage
	if err := dec.Decode(&env); err != nil {
		return nil, "", fmt.Errorf("ecs: parse bucket list JSON: %w", err)
	}
	// Envelope may also be a bare array (defensive).
	var raws []json.RawMessage
	for _, k := range []string{"object_buckets", "object_bucket", "buckets", "bucket", "data", "items"} {
		if v, ok := env[k]; ok {
			if err := json.Unmarshal(v, &raws); err == nil {
				break
			}
			// Single-object envelope (one bucket, not an array).
			var one json.RawMessage
			if err := json.Unmarshal(v, &one); err == nil {
				raws = []json.RawMessage{one}
				break
			}
		}
	}
	if raws == nil {
		// Try the whole body as an array.
		if err := json.Unmarshal(body, &raws); err != nil {
			return nil, "", nil // no recognisable bucket list: empty, not an error
		}
	}
	metas := make([]BucketMeta, 0, len(raws))
	for _, r := range raws {
		m, err := bucketMetaFromMap(r)
		if err != nil {
			return nil, "", err
		}
		if m.Name == "" {
			continue
		}
		metas = append(metas, m)
	}
	return metas, nextMarkerFromMap(env), nil
}

func bucketMetaFromMap(raw json.RawMessage) (BucketMeta, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]json.RawMessage
	if err := dec.Decode(&m); err != nil {
		return BucketMeta{}, fmt.Errorf("ecs: bucket row: %w", err)
	}
	var b BucketMeta
	b.Name = strField(m, "name")
	b.Namespace = strField(m, "namespace")
	b.Owner = strField(m, "owner")
	b.VPool = strField(m, "vpool", "vPool", "replication_group")
	b.APIType = strField(m, "api_type", "apiType")
	if v, ok := numField(m, "block_size", "blockSize"); ok && v > 0 {
		b.BlockSize, b.HasBlock = v, true
	}
	if v, ok := numField(m, "notification_size", "notificationSize"); ok && v > 0 {
		b.NotificationSize, b.HasNotify = v, true
	}
	return b, nil
}

func nextMarkerFromMap(env map[string]json.RawMessage) string {
	for _, k := range []string{"next_marker", "NextMarker", "marker", "Marker", "nextMarker"} {
		if v, ok := env[k]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func strField(m map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil {
				return s
			}
			// Non-string scalar (number/bool): render plainly.
			var n json.Number
			if json.Unmarshal(v, &n) == nil {
				return n.String()
			}
		}
	}
	return ""
}

func numField(m map[string]json.RawMessage, keys ...string) (int64, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		var n json.Number
		if json.Unmarshal(v, &n) == nil {
			if f, err := n.Float64(); err == nil {
				return int64(f), true
			}
		}
		var s string
		if json.Unmarshal(v, &s) == nil {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			var f float64
			if _, err := fmt.Sscanf(s, "%g", &f); err == nil {
				return int64(f), true
			}
		}
	}
	return 0, false
}

// ParseNamespaceMeta parses one namespace object. Only the necessary fields
// are extracted; everything else is ignored.
func ParseNamespaceMeta(body []byte, contentType string) (*NamespaceMeta, error) {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) > 0 && trimmed[0] == '<' && !strings.Contains(strings.ToLower(contentType), "json") {
		return parseNamespaceMetaXML(trimmed)
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var m map[string]json.RawMessage
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("ecs: parse namespace JSON: %w", err)
	}
	out := &NamespaceMeta{
		Name: strField(m, "name", "namespace"),
		ID:   strField(m, "id"),
	}
	if v, ok := numField(m, "default_bucket_block_size", "defaultBucketBlockSize"); ok && v > 0 {
		out.DefaultBucketBlock, out.HasDefaultBucketBlock = v, true
	}
	if v, ok := numField(m, "block_size", "blockSize"); ok && v > 0 {
		out.BlockSize, out.HasBlock = v, true
	}
	if v, ok := numField(m, "notification_size", "notificationSize"); ok && v > 0 {
		out.NotificationSize, out.HasNotify = v, true
	}
	if out.Name == "" {
		return nil, fmt.Errorf("ecs: namespace info missing name")
	}
	return out, nil
}

// --- XML fallback (ECS answers XML unless Accept: application/json wins) ---

type xmlBucketMeta struct {
	Name             string `xml:"name"`
	Namespace        string `xml:"namespace"`
	BlockSize        string `xml:"block_size"`
	NotificationSize string `xml:"notification_size"`
	Owner            string `xml:"owner"`
	VPool            string `xml:"vpool"`
	APIType          string `xml:"api_type"`
}

type xmlBucketList struct {
	Buckets    []xmlBucketMeta `xml:"object_bucket"`
	AltBuckets []xmlBucketMeta `xml:"bucket"`
	NextMarker string          `xml:"next_marker"`
}

type xmlNamespaceMeta struct {
	XMLName      xml.Name `xml:"namespace"`
	Name         string   `xml:"name"`
	ID           string   `xml:"id"`
	DefaultBlock string   `xml:"default_bucket_block_size"`
	// Real ECS also reports namespace-level thresholds in camelCase;
	// snake_case variants are accepted defensively.
	BlockSize        string `xml:"blockSize"`
	BlockSizeSnake   string `xml:"block_size"`
	NotificationSize string `xml:"notificationSize"`
	NotifySizeSnake  string `xml:"notification_size"`
}

func parseBucketListXML(body []byte) ([]BucketMeta, string, error) {
	var l xmlBucketList
	if err := xml.Unmarshal(body, &l); err != nil {
		return nil, "", fmt.Errorf("ecs: parse bucket list XML: %w", err)
	}
	rows := append(l.Buckets, l.AltBuckets...)
	metas := make([]BucketMeta, 0, len(rows))
	for _, r := range rows {
		if strings.TrimSpace(r.Name) == "" {
			continue
		}
		m := BucketMeta{Name: strings.TrimSpace(r.Name), Namespace: strings.TrimSpace(r.Namespace),
			Owner: strings.TrimSpace(r.Owner), VPool: strings.TrimSpace(r.VPool), APIType: strings.TrimSpace(r.APIType)}
		if v, err := parseFloat(strings.TrimSpace(r.BlockSize)); err == nil && strings.TrimSpace(r.BlockSize) != "" && v > 0 {
			m.BlockSize, m.HasBlock = int64(v), true
		}
		if v, err := parseFloat(strings.TrimSpace(r.NotificationSize)); err == nil && strings.TrimSpace(r.NotificationSize) != "" && v > 0 {
			m.NotificationSize, m.HasNotify = int64(v), true
		}
		metas = append(metas, m)
	}
	return metas, strings.TrimSpace(l.NextMarker), nil
}

func parseNamespaceMetaXML(body []byte) (*NamespaceMeta, error) {
	var n xmlNamespaceMeta
	if err := xml.Unmarshal(body, &n); err != nil {
		return nil, fmt.Errorf("ecs: parse namespace XML: %w", err)
	}
	out := &NamespaceMeta{Name: strings.TrimSpace(n.Name), ID: strings.TrimSpace(n.ID)}
	if s := strings.TrimSpace(n.DefaultBlock); s != "" {
		if v, err := parseFloat(s); err == nil && v > 0 {
			out.DefaultBucketBlock, out.HasDefaultBucketBlock = int64(v), true
		}
	}
	for _, s := range []string{strings.TrimSpace(n.BlockSize), strings.TrimSpace(n.BlockSizeSnake)} {
		if s == "" {
			continue
		}
		if v, err := parseFloat(s); err == nil && v > 0 {
			out.BlockSize, out.HasBlock = int64(v), true
			break
		}
	}
	for _, s := range []string{strings.TrimSpace(n.NotificationSize), strings.TrimSpace(n.NotifySizeSnake)} {
		if s == "" {
			continue
		}
		if v, err := parseFloat(s); err == nil && v > 0 {
			out.NotificationSize, out.HasNotify = int64(v), true
			break
		}
	}
	if out.Name == "" {
		return nil, fmt.Errorf("ecs: namespace info missing name")
	}
	return out, nil
}
