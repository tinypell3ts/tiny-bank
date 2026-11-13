package kit

import (
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// Config is what every service reads from its environment (12-factor). In
// Kubernetes these come from the Deployment's env and ConfigMap. A service
// with more settings embeds Config in its own config struct.
type Config struct {
	Port            string        // PORT, default 8080
	LogLevel        slog.Level    // LOG_LEVEL: debug, info, warn, error. Default info
	ShutdownTimeout time.Duration // SHUTDOWN_TIMEOUT, e.g. 10s. Default 10s
}

// LoadConfig reads Config. It takes getenv instead of calling os.Getenv so
// tests can pass a fake environment.
func LoadConfig(getenv func(string) string) (Config, error) {
	cfg := Config{
		Port:            "8080",
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: 10 * time.Second,
	}

	if v := getenv("PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 65535 {
			return Config{}, fmt.Errorf("PORT %q: must be a number from 0 to 65535", v)
		}
		cfg.Port = v
	}

	if v := getenv("LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			return Config{}, fmt.Errorf("LOG_LEVEL %q: %w", v, err)
		}
	}

	if v := getenv("SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("SHUTDOWN_TIMEOUT %q: must be a positive duration like 10s", v)
		}
		cfg.ShutdownTimeout = d
	}

	return cfg, nil
}
