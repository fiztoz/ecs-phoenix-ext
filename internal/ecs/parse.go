package ecs

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"
	"time"
)

// BillingPayload is the parsed, unit-normalised result of one billing page.
type BillingPayload struct {
	Namespace        string
	NamespaceBytes   int64 // namespace-level summary (may not equal bucket sum)
	NamespaceObjects int64
	UptodateTill     time.Time // zero when absent
	Buckets          []BucketBilling
	NextMarker       string
}

// BucketBilling is one bucket row with bytes already converted.
type BucketBilling struct {
	Name         string
	Namespace    string
	UsedBytes    int64 // total_size + total_mpu_size (MPU occupies capacity)
	Objects      int64
	MPUBytes     int64
	MPUParts     int64
	UptodateTill time.Time // zero when absent
}

// parseTime accepts RFC3339 plus a couple of formats ECS management API has
// been seen to emit. Everything is normalised to UTC.
func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("ecs: unparseable timestamp %q", s)
}

func num(f json.Number) (float64, error) {
	if f == "" {
		return 0, nil
	}
	return f.Float64()
}

// --- JSON ---

type jsonBucket struct {
	Name          string      `json:"name"`
	Namespace     string      `json:"namespace"`
	TotalSize     json.Number `json:"total_size"`
	TotalSizeUnit string      `json:"total_size_unit"`
	TotalObjects  int64       `json:"total_objects"`
	UptodateTill  string      `json:"uptodate_till"`
	TotalMPUSize  json.Number `json:"total_mpu_size"`
	TotalMPUParts int64       `json:"total_mpu_parts"`
}

type jsonPayload struct {
	Name          string       `json:"name"`
	Namespace     string       `json:"namespace"`
	TotalSize     json.Number  `json:"total_size"`
	TotalSizeUnit string       `json:"total_size_unit"`
	TotalObjects  int64        `json:"total_objects"`
	UptodateTill  string       `json:"uptodate_till"`
	Buckets       []jsonBucket `json:"bucket_billing_info"`
	NextMarker    string       `json:"next_marker"`
}

// --- XML ---

type xmlBucket struct {
	XMLName       xml.Name `xml:"bucket_billing_info"`
	Name          string   `xml:"name"`
	Namespace     string   `xml:"namespace"`
	TotalSize     string   `xml:"total_size"`
	TotalSizeUnit string   `xml:"total_size_unit"`
	TotalObjects  int64    `xml:"total_objects"`
	UptodateTill  string   `xml:"uptodate_till"`
	TotalMPUSize  string   `xml:"total_mpu_size"`
	TotalMPUParts int64    `xml:"total_mpu_parts"`
}

type xmlPayload struct {
	XMLName       xml.Name    `xml:"namespace_billing_info"`
	Name          string      `xml:"name"`
	Namespace     string      `xml:"namespace"`
	TotalSize     string      `xml:"total_size"`
	TotalSizeUnit string      `xml:"total_size_unit"`
	TotalObjects  int64       `xml:"total_objects"`
	UptodateTill  string      `xml:"uptodate_till"`
	Buckets       []xmlBucket `xml:"bucket_billing_info"`
	NextMarker    string      `xml:"next_marker"`
}

// ParseBilling parses one billing page body. It accepts JSON when the
// content type says JSON or the body starts with '{'; otherwise XML.
func ParseBilling(body []byte, contentType string) (*BillingPayload, error) {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	isJSON := strings.Contains(strings.ToLower(contentType), "json") ||
		(len(trimmed) > 0 && trimmed[0] == '{')
	if isJSON {
		return parseJSON(trimmed)
	}
	return parseXML(trimmed)
}

func parseJSON(body []byte) (*BillingPayload, error) {
	var p jsonPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("ecs: parse billing JSON: %w", err)
	}
	out := &BillingPayload{
		Namespace:        p.Namespace,
		NamespaceObjects: p.TotalObjects,
		NextMarker:       p.NextMarker,
	}
	if out.Namespace == "" {
		out.Namespace = p.Name
	}
	total, err := num(p.TotalSize)
	if err != nil {
		return nil, fmt.Errorf("ecs: namespace total_size: %w", err)
	}
	if out.NamespaceBytes, err = ToBytes(total, p.TotalSizeUnit); err != nil {
		return nil, fmt.Errorf("ecs: namespace total_size: %w", err)
	}
	if out.UptodateTill, err = parseTime(p.UptodateTill); err != nil {
		return nil, err
	}
	for _, b := range p.Buckets {
		row, err := convertBucket(b.Name, b.Namespace, out.Namespace,
			b.TotalSize, b.TotalSizeUnit, b.TotalObjects, b.TotalMPUSize, b.TotalMPUParts,
			b.UptodateTill)
		if err != nil {
			return nil, fmt.Errorf("ecs: bucket %q: %w", b.Name, err)
		}
		out.Buckets = append(out.Buckets, row)
	}
	return out, nil
}

func parseXML(body []byte) (*BillingPayload, error) {
	var p xmlPayload
	if err := xml.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("ecs: parse billing XML: %w", err)
	}
	out := &BillingPayload{
		Namespace:        p.Namespace,
		NamespaceObjects: p.TotalObjects,
		NextMarker:       p.NextMarker,
	}
	if out.Namespace == "" {
		out.Namespace = p.Name
	}
	total, err := parseFloat(p.TotalSize)
	if err != nil {
		return nil, fmt.Errorf("ecs: namespace total_size: %w", err)
	}
	if out.NamespaceBytes, err = ToBytes(total, p.TotalSizeUnit); err != nil {
		return nil, fmt.Errorf("ecs: namespace total_size: %w", err)
	}
	if out.UptodateTill, err = parseTime(p.UptodateTill); err != nil {
		return nil, err
	}
	for _, b := range p.Buckets {
		row, err := convertBucketStr(b.Name, b.Namespace, out.Namespace,
			b.TotalSize, b.TotalSizeUnit, b.TotalObjects, b.TotalMPUSize, b.TotalMPUParts,
			b.UptodateTill)
		if err != nil {
			return nil, fmt.Errorf("ecs: bucket %q: %w", b.Name, err)
		}
		out.Buckets = append(out.Buckets, row)
	}
	return out, nil
}

func parseFloat(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
		return 0, err
	}
	return f, nil
}

func convertBucket(name, ns, parentNS string, size json.Number, unit string, objects int64,
	mpuSize json.Number, mpuParts int64, uptodate string) (BucketBilling, error) {
	sv, err := num(size)
	if err != nil {
		return BucketBilling{}, err
	}
	mv, err := num(mpuSize)
	if err != nil {
		return BucketBilling{}, err
	}
	return buildBucket(name, ns, parentNS, sv, unit, objects, mv, mpuParts, uptodate)
}

func convertBucketStr(name, ns, parentNS string, size, unit string, objects int64,
	mpuSize string, mpuParts int64, uptodate string) (BucketBilling, error) {
	sv, err := parseFloat(size)
	if err != nil {
		return BucketBilling{}, err
	}
	mv, err := parseFloat(mpuSize)
	if err != nil {
		return BucketBilling{}, err
	}
	return buildBucket(name, ns, parentNS, sv, unit, objects, mv, mpuParts, uptodate)
}

func buildBucket(name, ns, parentNS string, size float64, unit string, objects int64,
	mpuSize float64, mpuParts int64, uptodate string) (BucketBilling, error) {
	if name == "" {
		return BucketBilling{}, fmt.Errorf("missing name")
	}
	used, err := ToBytes(size, unit)
	if err != nil {
		return BucketBilling{}, err
	}
	mpu, err := ToBytes(mpuSize, unit)
	if err != nil {
		return BucketBilling{}, err
	}
	ut, err := parseTime(uptodate)
	if err != nil {
		return BucketBilling{}, err
	}
	if ns == "" {
		ns = parentNS // nested bucket rows may omit namespace — inherit parent
	}
	return BucketBilling{
		Name:         name,
		Namespace:    ns,
		UsedBytes:    used + mpu, // incomplete MPU occupies capacity
		Objects:      objects,
		MPUBytes:     mpu,
		MPUParts:     mpuParts,
		UptodateTill: ut,
	}, nil
}
