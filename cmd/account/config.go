package main

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// config is everything the service reads from its environment (12-factor).
// In Kubernetes these come from the Deployment's env / ConfigMap.
type config struct {
	Port            string        // PORT, default 8080
	LogLevel        slog.Level    // LOG_LEVEL: debug, info, warn, error. Default info
	ShutdownTimeout time.Duration // SHUTDOWN_TIMEOUT, e.g. 10s. Default 10s
	DatabaseURL     string        // DATABASE_URL, required. Set by the Helm chart from CloudNativePG's secret
}

// loadConfig takes getenv instead of calling os.Getenv so tests can pass a fake environment.
func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		Port:            "8080",
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: 10 * time.Second,
	}

	if v := getenv("PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 65535 {
			return config{}, fmt.Errorf("PORT %q: must be a number from 0 to 65535", v)
		}
		cfg.Port = v
	}

	if v := getenv("LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			return config{}, fmt.Errorf("LOG_LEVEL %q: %w", v, err)
		}
	}

	if v := getenv("SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return config{}, fmt.Errorf("SHUTDOWN_TIMEOUT %q: must be a positive duration like 10s", v)
		}
		cfg.ShutdownTimeout = d
	}

	cfg.DatabaseURL = getenv("DATABASE_URL")
	if cfg.DatabaseURL == "" {
		// Never echo the value: it contains the database password.
		return config{}, errors.New("DATABASE_URL is required")
	}

	return cfg, nil
}
