package main

import (
	"log/slog"

	"github.com/fiztoz/ecs-phoenix-ext/internal/config"
	srvhttp "github.com/fiztoz/ecs-phoenix-ext/internal/http"
	"github.com/fiztoz/ecs-phoenix-ext/internal/poller"
	"github.com/fiztoz/ecs-phoenix-ext/internal/store"
)

// newHTTPServer wires the dashboard/API server deps from environment config.
func newHTTPServer(cfg *config.Config, p *poller.Poller, st store.Store, log *slog.Logger) (*srvhttp.Server, error) {
	deps := srvhttp.Deps{
		Namespace: cfg.ECSNamespace,
		BasePath:  cfg.BasePath,
		Snapshots: p,
		Store:     st,
		Log:       log,
	}
	deps.UIToken = cfg.UIToken
	return srvhttp.New(deps)
}
