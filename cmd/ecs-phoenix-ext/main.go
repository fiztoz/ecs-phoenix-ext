// Command ecs-phoenix-ext is the Uptime Phoenix storage extension: it polls Dell
// ECS namespace billing, renders a dashboard/wallboard, and exposes
// /health/ready + /health/quota for Phoenix HTTP monitors.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	ecsphoenixext "github.com/fiztoz/ecs-phoenix-ext"
	"github.com/fiztoz/ecs-phoenix-ext/internal/config"
	"github.com/fiztoz/ecs-phoenix-ext/internal/ecs"
	"github.com/fiztoz/ecs-phoenix-ext/internal/poller"
	"github.com/fiztoz/ecs-phoenix-ext/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ecs-phoenix-ext:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err // bad env: crash fast, this is unrecoverable
	}

	level := slog.LevelInfo
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := st.Migrate(ctx); err != nil {
		_ = st.Close()
		return fmt.Errorf("migrate: %w", err)
	}

	client, err := newECSClient(cfg, log)
	if err != nil {
		_ = st.Close()
		return err
	}

	// ECS may be down at startup: never crash-loop for that. HTTP comes up,
	// /health/live is 200 and /health/ready is 503 until a poll succeeds.
	p := poller.New(ctx, client, st, cfg.ECSNamespace, cfg.PollInterval, log)
	go p.Run(ctx)

	srv, err := newHTTPServer(cfg, p, st, log)
	if err != nil {
		_ = st.Close()
		return err
	}

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	httpErr := make(chan error, 1)
	go func() {
		log.Info("ecs-phoenix-ext listening", "addr", cfg.ListenAddr, "base_path", cfg.BasePath)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErr <- err
		}
	}()

	select {
	case err := <-httpErr:
		stop()
		shutdown(client, st, log)
		return fmt.Errorf("http: %w", err) // cannot bind: crash
	case <-ctx.Done():
	}

	// Graceful shutdown: HTTP (5s) → poller stops via ctx →
	// logout (2-3s, never force=true) → close DB.
	shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := httpServer.Shutdown(shCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	cancel()
	shutdown(client, st, log)
	log.Info("ecs-phoenix-ext stopped")
	return nil
}

func shutdown(client *ecs.Client, st store.Store, log *slog.Logger) {
	logCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Logout(logCtx); err != nil {
		log.Warn("ecs logout", "err", err)
	}
	if err := st.Close(); err != nil {
		log.Warn("store close", "err", err)
	}
}

func openStore(cfg *config.Config) (store.Store, error) {
	switch cfg.DatabaseEngine {
	case "sqlite":
		return store.OpenSQLite(cfg.DatabaseDSN, ecsphoenixext.Migrations)
	default:
		return store.OpenMariaDB(cfg.DatabaseDSN, ecsphoenixext.Migrations)
	}
}
