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
	if m0.BlockSize != 134217728 {
		t.Errorf("block = %d, want 134217728", m0.BlockSize)
	}
	if m0.NotificationSize != 1073741824 {
		t.Errorf("notify = %d, want 1073741824", m0.NotificationSize)
	}
	// String-encoded numbers must parse too.
	if metas[1].BlockSize != 67108864 {
		t.Errorf("string block = %d, want 67108864", metas[1].BlockSize)
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
		t.Errorf("default block = %d/%v, want 134217728/true", m.DefaultBucketBlock, m.HasDefaultBucketBlock)
	}
}

func TestParseBucketListXML(t *testing.T) {
	body := `<?xml version="1.0"?><bucket_list>` +
		`<object_bucket><name>b1</name><namespace>ns</namespace>` +
		`<block_size>134217728</block_size><notification_size>1024</notification_size></object_bucket>` +
		`<next_marker></next_marker></bucket_list>`
	metas, _, err := ParseBucketList([]byte(body), "application/xml")
	if err != nil {
		t.Fatalf("XML: %v", err)
	}
	if len(metas) != 1 || metas[0].BlockSize != 134217728 {
		t.Fatalf("XML metas = %+v", metas)
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
	  {"name": "notify-bkt", "namespace": "ns", "notification_size": 1073741824},
	  {"name": "block-bkt", "namespace": "ns", "block_size": 10737418240},
	  {"name": "both-bkt", "namespace": "ns", "block_size": 10737418240, "notification_size": 8589934592}
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
	if got := byName["both-bkt"].BlockSize; got != 10737418240 {
		t.Errorf("both block = %d, want 10737418240 (10 GiB UI value in bytes)", got)
	}
}
