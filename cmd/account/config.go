package main

import (
	"errors"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
)

// config is the shared kit.Config (PORT, LOG_LEVEL, SHUTDOWN_TIMEOUT) plus
// what only the account service needs.
type config struct {
	kit.Config
	DatabaseURL kit.Secret // DATABASE_URL, required. Set by the Helm chart from CloudNativePG's secret; it contains the password, so it never prints
}

// loadConfig takes getenv instead of calling os.Getenv so tests can pass a fake environment.
func loadConfig(getenv func(string) string) (config, error) {
	base, err := kit.LoadConfig(getenv)
	if err != nil {
		return config{}, err
	}
	cfg := config{Config: base, DatabaseURL: kit.Secret(getenv("DATABASE_URL"))}
	if cfg.DatabaseURL == "" {
		return config{}, errors.New("DATABASE_URL is required")
	}
	return cfg, nil
}
