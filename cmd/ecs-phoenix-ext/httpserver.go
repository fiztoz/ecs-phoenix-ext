package main

import (
	"log/slog"

	"github.com/fiztoz/ecs-phoenix-ext/internal/config"
	srvhttp "github.com/fiztoz/ecs-phoenix-ext/internal/http"
	"github.com/fiztoz/ecs-phoenix-ext/internal/identity"
	"github.com/fiztoz/ecs-phoenix-ext/internal/poller"
	"github.com/fiztoz/ecs-phoenix-ext/internal/store"
)

// version is stamped at build time: -ldflags "-X main.version=0.4.0".
var version = "dev"

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
	deps.Version = version
	deps.ExtensionID = cfg.PhoenixExtensionID
	if cfg.PhoenixIdentitySecret != "" {
		v, err := identity.NewVerifier(cfg.PhoenixIdentitySecret, cfg.PhoenixExtensionID)
		if err != nil {
			return nil, err
		}
		deps.Identity = v
	}
	return srvhttp.New(deps)
}
