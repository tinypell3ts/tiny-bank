package main

import (
	"errors"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
)

// config is the shared kit.Config (PORT, LOG_LEVEL, SHUTDOWN_TIMEOUT) plus
// what only the account service needs.
type config struct {
	kit.Config
	DatabaseURL string // DATABASE_URL, required. Set by the Helm chart from CloudNativePG's secret
}

// loadConfig takes getenv instead of calling os.Getenv so tests can pass a fake environment.
func loadConfig(getenv func(string) string) (config, error) {
	base, err := kit.LoadConfig(getenv)
	if err != nil {
		return config{}, err
	}
	cfg := config{Config: base, DatabaseURL: getenv("DATABASE_URL")}
	if cfg.DatabaseURL == "" {
		// Never echo the value: it contains the database password.
		return config{}, errors.New("DATABASE_URL is required")
	}
	return cfg, nil
}
