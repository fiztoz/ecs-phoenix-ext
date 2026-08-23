package main

import (
	"log/slog"

	"github.com/fiztoz/ecs-phoenix-ext/internal/config"
	"github.com/fiztoz/ecs-phoenix-ext/internal/ecs"
)

// newECSClient wires the management-plane client from environment config.
// The credential flows env → client only; it is never logged.
func newECSClient(cfg *config.Config, log *slog.Logger) (*ecs.Client, error) {
	cc := ecs.ClientConfig{
		BaseURL:  cfg.ECSMgmtURL,
		Username: cfg.ECSUsername,
		SizeUnit: cfg.SizeUnit,
		Timeout:  cfg.HTTPTimeout,
		CAFile:   cfg.TLSCAFile,
		CAInline: cfg.TLSCA,
		Insecure: cfg.TLSInsecure,
		Logger:   func(msg string, args ...any) { log.Debug(msg, args...) },
	}
	cc.Userpass = cfg.ECSCred
	return ecs.NewClient(cc)
}
