// Package config loads ecs-phoenix-ext configuration from the environment.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config is the full environment surface of ecs-phoenix-ext.
type Config struct {
	ECSMgmtURL   string `env:"ECS_MGMT_URL,required"`  // https://ecs.example.com:4443 — no trailing slash
	ECSNamespace string `env:"ECS_NAMESPACE,required"` // one namespace in v1
	ECSUsername  string `env:"ECS_USERNAME,required"`  // ECS management user (example: ecs-usage)
	ECSCred      string `env:"ECS_PASSWORD,required"`  // from Secret; never logged
	SizeUnit     string `env:"ECS_SIZEUNIT" envDefault:"KB"`

	PollInterval time.Duration `env:"ECS_POLL_INTERVAL" envDefault:"15m"`
	TLSCAFile    string        `env:"ECS_TLS_CA_FILE"`
	TLSCA        string        `env:"ECS_TLS_CA"`
	TLSInsecure  bool          `env:"ECS_TLS_INSECURE" envDefault:"false"`
	HTTPTimeout  time.Duration `env:"ECS_HTTP_TIMEOUT" envDefault:"60s"`

	ListenAddr     string `env:"LISTEN_ADDR" envDefault:":80"`
	DatabaseDSN    string `env:"DATABASE_DSN,required"` // MariaDB DSN or file:/data/ecs-phoenix-ext.db
	DatabaseEngine string `env:"DATABASE_ENGINE"`       // mariadb | sqlite; inferred when empty
	BasePath       string `env:"BASE_PATH" envDefault:"/storage"`
	PublicURL      string `env:"PUBLIC_URL"`
	UIToken        string `env:"UI_TOKEN"`
	LogLevel       string `env:"LOG_LEVEL" envDefault:"info"`
	BucketMapFile  string `env:"BUCKET_MAP_FILE"` // reserved; unused in v1 logic
}

// Load parses the environment and validates locked constraints.
func Load() (*Config, error) {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	cfg.ECSMgmtURL = strings.TrimRight(strings.TrimSpace(cfg.ECSMgmtURL), "/")
	if cfg.ECSMgmtURL == "" {
		return nil, fmt.Errorf("config: ECS_MGMT_URL must not be empty")
	}
	cfg.SizeUnit = strings.ToUpper(strings.TrimSpace(cfg.SizeUnit))
	if cfg.SizeUnit == "" {
		cfg.SizeUnit = "KB"
	}
	if cfg.PollInterval < time.Minute {
		return nil, fmt.Errorf("config: ECS_POLL_INTERVAL must be >= 1m, got %s", cfg.PollInterval)
	}

	cfg.BasePath = strings.TrimSpace(cfg.BasePath)
	if cfg.BasePath == "" {
		cfg.BasePath = "/"
	}
	if !strings.HasPrefix(cfg.BasePath, "/") {
		cfg.BasePath = "/" + cfg.BasePath
	}
	cfg.BasePath = strings.TrimRight(cfg.BasePath, "/")
	if cfg.BasePath == "" {
		cfg.BasePath = "/"
	}

	switch cfg.DatabaseEngine {
	case "":
		if strings.HasPrefix(cfg.DatabaseDSN, "file:") {
			cfg.DatabaseEngine = "sqlite"
		} else {
			cfg.DatabaseEngine = "mariadb"
		}
	case "mariadb", "sqlite":
	default:
		return nil, fmt.Errorf("config: DATABASE_ENGINE must be mariadb or sqlite, got %q", cfg.DatabaseEngine)
	}

	return &cfg, nil
}
