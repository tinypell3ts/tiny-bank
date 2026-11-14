package kit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const value = "postgres://app:hunter2@db:5432/app" //nolint:gosec // fake credentials, to check they never print

// config is the kind of struct a secret lives in, and what code might log.
type config struct {
	Port        string
	DatabaseURL Secret
}

func TestSecretNeverPrints(t *testing.T) {
	s := Secret(value)
	cfg := config{Port: "8080", DatabaseURL: s}

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	logger.Info("starting", "database_url", s, "config", cfg)
	jsonCfg, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	for name, out := range map[string]string{
		"%v":                     fmt.Sprintf("%v", s),
		"%s":                     fmt.Sprintf("%s", s), //nolint:staticcheck // testing fmt's %s path on purpose
		"%q":                     fmt.Sprintf("%q", s),
		"%+v of a struct":        fmt.Sprintf("%+v", cfg),
		"%#v of a struct":        fmt.Sprintf("%#v", cfg),
		"in a wrapped error":     fmt.Errorf("load config %v: %w", cfg, errors.New("bad")).Error(),
		"Sprint":                 fmt.Sprint(s),
		"slog, alone and nested": logs.String(),
		"JSON":                   string(jsonCfg),
	} {
		if strings.Contains(out, "hunter2") || strings.Contains(out, "postgres://") {
			t.Errorf("%s leaks the secret: %s", name, out)
		}
		if !strings.Contains(out, "xxxxx") {
			t.Errorf("%s should show xxxxx, got: %s", name, out)
		}
	}
}

func TestSecretReveal(t *testing.T) {
	if got := Secret(value).Reveal(); got != value {
		t.Errorf("Reveal() = %q, want the real value", got)
	}
	if Secret("") != "" {
		t.Error("an empty Secret should equal the empty string, so required-checks still work")
	}
}
