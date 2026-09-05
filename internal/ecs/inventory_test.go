package ecs

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestParseBucketListFixture(t *testing.T) {
	b, _ := os.ReadFile("testdata/bucket_list.json")
	metas, next, err := ParseBucketList(b, "application/json")
	if err != nil {
		t.Fatalf("ParseBucketList: %v", err)
	}
	if next != "" {
		t.Fatalf("next = %q, want empty", next)
	}
	if len(metas) != 2 {
		t.Fatalf("metas = %d, want 2", len(metas))
	}
	m0 := metas[0]
	if m0.Name != "sample-bkt" || !m0.HasBlock || !m0.HasNotify {
		t.Fatalf("m0 = %+v, want block+notify present", m0)
	}
	// ECS reports GiB; the parser normalizes to bytes.
	if m0.BlockSize != 128*1024*1024*1024 {
		t.Errorf("block = %d, want 128 GiB in bytes", m0.BlockSize)
	}
	if m0.NotificationSize != 1024*1024*1024*1024 {
		t.Errorf("notify = %d, want 1024 GiB in bytes", m0.NotificationSize)
	}
	// String-encoded numbers must parse too.
	if metas[1].BlockSize != 64*1024*1024*1024 {
		t.Errorf("string block = %d, want 64 GiB in bytes", metas[1].BlockSize)
	}
}

func TestParseNamespaceMetaFixture(t *testing.T) {
	b, _ := os.ReadFile("testdata/namespace_meta.json")
	m, err := ParseNamespaceMeta(b, "application/json")
	if err != nil {
		t.Fatalf("ParseNamespaceMeta: %v", err)
	}
	if m.Name != "prod-ns" {
		t.Errorf("name = %q", m.Name)
	}
	if !m.HasDefaultBucketBlock || m.DefaultBucketBlock != 134217728 {
		t.Errorf("default block = %d/%v, want 134217728/true (raw bytes chunk size)", m.DefaultBucketBlock, m.HasDefaultBucketBlock)
	}
}

func TestParseBucketListXML(t *testing.T) {
	body := `<?xml version="1.0"?><bucket_list>` +
		`<object_bucket><name>b1</name><namespace>ns</namespace>` +
		`<block_size>128</block_size><notification_size>1024</notification_size></object_bucket>` +
		`<next_marker></next_marker></bucket_list>`
	metas, _, err := ParseBucketList([]byte(body), "application/xml")
	if err != nil {
		t.Fatalf("XML: %v", err)
	}
	if len(metas) != 1 || metas[0].BlockSize != 128*1024*1024*1024 {
		t.Fatalf("XML metas = %+v, want 128 GiB in bytes", metas)
	}
}

func TestInventory401ReloginsOnce(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/login":
			calls++
			w.Header().Set("X-SDS-AUTH-TOKEN", fmt.Sprintf("tok-%d", calls))
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(r.URL.Path, "/object/bucket"):
			if r.Header.Get("X-SDS-AUTH-TOKEN") == "tok-1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			b, _ := os.ReadFile("testdata/bucket_list.json")
			_, _ = w.Write(b)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	metas, err := c.BucketMetas(context.Background(), "prod-ns")
	if err != nil {
		t.Fatalf("BucketMetas: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("metas = %d, want 2", len(metas))
	}
	if calls != 2 {
		t.Fatalf("logins = %d, want 2 (initial + one retry)", calls)
	}
}

func TestNamespaceMetaInfoRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/login":
			w.Header().Set("X-SDS-AUTH-TOKEN", "tok-1")
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/object/namespaces/namespace/prod-ns":
			w.Header().Set("Content-Type", "application/json")
			b, _ := os.ReadFile("testdata/namespace_meta.json")
			_, _ = w.Write(b)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	m, err := c.NamespaceMetaInfo(context.Background(), "prod-ns")
	if err != nil {
		t.Fatalf("NamespaceMetaInfo: %v", err)
	}
	if m.DefaultBucketBlock != 134217728 {
		t.Fatalf("default block = %d", m.DefaultBucketBlock)
	}
}

func TestQuotaModeVariations(t *testing.T) {
	body := `{"object_bucket": [
	  {"name": "off-bkt", "namespace": "ns", "block_size": 0, "notification_size": 0},
	  {"name": "notify-bkt", "namespace": "ns", "notification_size": 1024},
	  {"name": "block-bkt", "namespace": "ns", "block_size": 10},
	  {"name": "both-bkt", "namespace": "ns", "block_size": 10, "notification_size": 8}
	], "next_marker": ""}`
	metas, _, err := ParseBucketList([]byte(body), "application/json")
	if err != nil {
		t.Fatalf("ParseBucketList: %v", err)
	}
	byName := map[string]BucketMeta{}
	for _, m := range metas {
		byName[m.Name] = m
	}
	// 0 counts as unset → quota off, no sizes recorded.
	if m := byName["off-bkt"]; m.QuotaMode() != "off" || m.HasBlock || m.HasNotify {
		t.Errorf("off-bkt = %+v, want mode off with no sizes", m)
	}
	if m := byName["notify-bkt"]; m.QuotaMode() != "notify-only" || m.HasBlock || !m.HasNotify {
		t.Errorf("notify-bkt = %+v, want notify-only", m)
	}
	if m := byName["block-bkt"]; m.QuotaMode() != "block-only" || !m.HasBlock || m.HasNotify {
		t.Errorf("block-bkt = %+v, want block-only", m)
	}
	if m := byName["both-bkt"]; m.QuotaMode() != "block-notify" || !m.HasBlock || !m.HasNotify {
		t.Errorf("both-bkt = %+v, want block-notify", m)
	}
	if got := byName["both-bkt"].BlockSize; got != 10*1024*1024*1024 {
		t.Errorf("both block = %d, want 10 GiB in bytes", got)
	}
	if got := byName["notify-bkt"].NotificationSize; got != 1024*1024*1024*1024 {
		t.Errorf("notify size = %d, want 1024 GiB in bytes", got)
	}
}

func TestPluralObjectBucketsEnvelopeJSON(t *testing.T) {
	body := `{"object_buckets": [{"name": "b1", "namespace": "ns", "block_size": 1024}], "Filter": "x"}`
	metas, _, err := ParseBucketList([]byte(body), "application/json")
	if err != nil {
		t.Fatalf("ParseBucketList: %v", err)
	}
	if len(metas) != 1 || metas[0].Name != "b1" || !metas[0].HasBlock {
		t.Fatalf("plural envelope not parsed: %+v", metas)
	}
}

// TestRealBucketListXMLShape mirrors the exact ECS response shape: root
// <object_buckets>, Filter with an escaped ampersand, extra elements
// (softquota, retention, search metadata, link) and -1 unset sentinels.
func TestRealBucketListXMLShape(t *testing.T) {
	body := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<object_buckets>
    <Filter>namespace=ns1&amp;name=*</Filter>
    <object_bucket>
        <api_type>S3</api_type>
        <block_size>-1</block_size>
        <owner>s3test</owner>
        <created>2020-03-05T17:03:46.608Z</created>
        <id>ns1.bucket1</id>
        <link rel="self" href="/object/bucket/ns1.bucket1"/>
        <locked>false</locked>
        <search_metadata><isEnabled>false</isEnabled><maxKeys>0</maxKeys></search_metadata>
        <name>bucket1</name>
        <namespace>ns1</namespace>
        <notification_size>-1</notification_size>
        <vpool>urn:storageos:ReplicationGroupInfo:61983759:global</vpool>
        <retention>0</retention>
        <softquota>-1</softquota>
        <TagSet/>
    </object_bucket>
    <object_bucket>
        <api_type>CAS</api_type>
        <block_size>-1</block_size>
        <owner>casprofile1</owner>
        <created>2019-03-25T16:26:16.897Z</created>
        <search_metadata><isEnabled>true</isEnabled><maxKeys>0</maxKeys>
            <metadata><datatype>datetime</datatype><name>CreateTime</name><type>System</type></metadata>
        </search_metadata>
        <name>casbucket1</name>
        <namespace>ns1</namespace>
        <notification_size>-1</notification_size>
        <vpool>urn:storageos:ReplicationGroupInfo:61983759:global</vpool>
        <retention>0</retention>
        <softquota>-1</softquota>
    </object_bucket>
</object_buckets>`
	metas, next, err := ParseBucketList([]byte(body), "application/xml")
	if err != nil {
		t.Fatalf("ParseBucketList: %v", err)
	}
	if next != "" {
		t.Fatalf("next = %q, want empty", next)
	}
	if len(metas) != 2 {
		t.Fatalf("metas = %d, want 2", len(metas))
	}
	// -1 sentinel means quota off / unlimited, but identity still parses.
	for _, m := range metas {
		if m.QuotaMode() != "off" || m.HasBlock || m.HasNotify {
			t.Errorf("%s = %+v, want mode off with no sizes", m.Name, m)
		}
	}
	if metas[0].Owner != "s3test" || metas[0].APIType != "S3" {
		t.Errorf("identity not parsed: %+v", metas[0])
	}
	if metas[1].APIType != "CAS" {
		t.Errorf("CAS row not parsed: %+v", metas[1])
	}
}

// TestRealNamespaceXMLShape mirrors the exact ECS namespace response:
// -1 sentinels everywhere plus camelCase blockSize/notificationSize.
func TestRealNamespaceXMLShape(t *testing.T) {
	body := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<namespace>
    <creation_time>1553479125783</creation_time>
    <id>ns1</id>
    <inactive>false</inactive>
    <link rel="self" href="/object/namespaces/namespace/ns1"/>
    <name>ns1</name>
    <default_bucket_block_size>-1</default_bucket_block_size>
    <blockSize>-1</blockSize>
    <is_compliance_enabled>false</is_compliance_enabled>
    <is_encryption_enabled>false</is_encryption_enabled>
    <is_stale_allowed>false</is_stale_allowed>
    <notificationSize>-1</notificationSize>
    <default_data_services_vpool>urn:storageos:ReplicationGroupInfo:61983759:global</default_data_services_vpool>
</namespace>`
	m, err := ParseNamespaceMeta([]byte(body), "application/xml")
	if err != nil {
		t.Fatalf("ParseNamespaceMeta: %v", err)
	}
	if m.Name != "ns1" || m.ID != "ns1" {
		t.Fatalf("identity = %+v, want ns1", m)
	}
	if m.HasDefaultBucketBlock || m.HasBlock || m.HasNotify {
		t.Fatalf("all -1 must be unset: %+v", m)
	}

	// Positive camelCase namespace thresholds are captured.
	body2 := `<namespace><name>ns1</name><id>ns1</id>` +
		`<default_bucket_block_size>-1</default_bucket_block_size>` +
		`<blockSize>10</blockSize><notificationSize>8</notificationSize></namespace>`
	m2, err := ParseNamespaceMeta([]byte(body2), "application/xml")
	if err != nil {
		t.Fatalf("ParseNamespaceMeta: %v", err)
	}
	if !m2.HasBlock || m2.BlockSize != 10*1024*1024*1024 {
		t.Errorf("namespace block not captured: %+v", m2)
	}
	if !m2.HasNotify || m2.NotificationSize != 8*1024*1024*1024 {
		t.Errorf("namespace notify not captured: %+v", m2)
	}
	if m2.HasDefaultBucketBlock {
		t.Errorf("default -1 must stay unset: %+v", m2)
	}
}
