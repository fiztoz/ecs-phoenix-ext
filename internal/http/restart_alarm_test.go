package http

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	ecsphoenixext "github.com/fiztoz/ecs-phoenix-ext"
	"github.com/fiztoz/ecs-phoenix-ext/internal/ecs"
	"github.com/fiztoz/ecs-phoenix-ext/internal/poller"
	"github.com/fiztoz/ecs-phoenix-ext/internal/store"
)

type scriptedECS struct {
	payload *ecs.BillingPayload
	metas   map[string]ecs.BucketMeta
	nsmeta  *ecs.NamespaceMeta
}

func (s *scriptedECS) NamespaceBilling(context.Context, string) (*ecs.BillingPayload, error) {
	return s.payload, nil
}
func (s *scriptedECS) BucketMetas(context.Context, string) (map[string]ecs.BucketMeta, error) {
	return s.metas, nil
}
func (s *scriptedECS) NamespaceMetaInfo(context.Context, string) (*ecs.NamespaceMeta, error) {
	return s.nsmeta, nil
}

func TestProductionQuotaHandlerKeepsRestoredAlarm(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "alarm.db")
	ctx := context.Background()
	open := func() *store.SQLite {
		t.Helper()
		st, err := store.OpenSQLite(dsn, ecsphoenixext.Migrations)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		return st
	}
	st := open()
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	src := &scriptedECS{
		payload: &ecs.BillingPayload{
			Namespace: "prod-ns", NamespaceBytes: 600, UptodateTill: now,
			Buckets: []ecs.BucketBilling{{Name: "bkt-one", Namespace: "prod-ns", UsedBytes: 600, UptodateTill: now}},
		},
		metas:  map[string]ecs.BucketMeta{"bkt-one": {Name: "bkt-one", HasBlock: true, BlockSize: 100}},
		nsmeta: &ecs.NamespaceMeta{Name: "prod-ns", HasBlock: true, BlockSize: 500},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := poller.New(ctx, src, st, "prod-ns", 15*time.Minute, log)
	p.PollOnce(ctx)
	p.PollOnce(ctx)
	if code := quotaCode(t, p, st); code != http.StatusServiceUnavailable {
		t.Fatalf("confirmed alarm handler = %d, want 503", code)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st = open()
	t.Cleanup(func() { _ = st.Close() })
	restarted := poller.New(ctx, src, st, "prod-ns", 15*time.Minute, log)
	if code := quotaCode(t, restarted, st); code != http.StatusServiceUnavailable {
		t.Fatalf("restarted handler = %d, want 503 before the next poll", code)
	}
}

func quotaCode(t *testing.T, p *poller.Poller, st store.Store) int {
	t.Helper()
	s, err := New(Deps{Namespace: "prod-ns", BasePath: "/storage", Snapshots: p, Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/storage/health/quota", nil))
	return rr.Code
}
