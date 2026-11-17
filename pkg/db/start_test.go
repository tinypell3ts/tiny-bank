package db

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
)

func TestLoadConfig(t *testing.T) {
	url := "postgres://app:" + secret + "@db:5432/app"
	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr string // substring; empty means no error expected
	}{
		{
			name: "defaults when only DATABASE_URL is set",
			env:  map[string]string{"DATABASE_URL": url},
			want: Config{Config: kit.Config{Port: "8080", LogLevel: slog.LevelInfo, ShutdownTimeout: 10 * time.Second}, DatabaseURL: kit.Secret(url)},
		},
		{
			name: "kit's settings are read too",
			env:  map[string]string{"PORT": "9000", "LOG_LEVEL": "debug", "SHUTDOWN_TIMEOUT": "30s", "DATABASE_URL": url},
			want: Config{Config: kit.Config{Port: "9000", LogLevel: slog.LevelDebug, ShutdownTimeout: 30 * time.Second}, DatabaseURL: kit.Secret(url)},
		},
		{name: "database URL missing", env: map[string]string{}, wantErr: "DATABASE_URL is required"},
		{name: "a bad kit setting", env: map[string]string{"PORT": "http", "DATABASE_URL": url}, wantErr: "PORT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := LoadConfig(func(k string) string { return tt.env[k] })
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want error containing %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaks the database password: %v", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("LoadConfig = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestConfigNeverPrintsTheDatabasePassword(t *testing.T) {
	url := "postgres://app:" + secret + "@db:5432/app"
	cfg, err := LoadConfig(func(k string) string { return map[string]string{"DATABASE_URL": url}[k] })
	if err != nil {
		t.Fatal(err)
	}
	// Logging or printing the whole config, a common debugging move, is safe.
	for _, out := range []string{fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg)} {
		if strings.Contains(out, secret) {
			t.Errorf("config printed the password: %s", out)
		}
	}
}

func TestStartGivesUpWhenTheDatabaseNeverAnswers(t *testing.T) {
	// Nothing listens on port 1, so every attempt fails. The caller's
	// deadline (here 3s instead of StartupTimeout) must stop the retries.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	var logs strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	_, err := Start(ctx, logger, "postgres://app:"+secret+"@127.0.0.1:1/app?sslmode=disable", fstest.MapFS{})
	if err == nil || !strings.Contains(err.Error(), "database not reachable before startup timeout") {
		t.Fatalf("err = %v, want the startup timeout error", err)
	}
	if !strings.Contains(logs.String(), "database not ready, retrying") {
		t.Errorf("expected retry log lines, got:\n%s", logs.String())
	}
	if strings.Contains(err.Error()+logs.String(), secret) {
		t.Error("error or logs leak the database password")
	}
}

func TestRunRejectsBadConfig(t *testing.T) {
	run := Run("test", fstest.MapFS{}, func(*typhon.Router, Deps) error {
		t.Error("routes added without a config")
		return nil
	})
	err := run(t.Context(), func(string) string { return "" }, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL is required") {
		t.Fatalf("err = %v, want the missing DATABASE_URL error", err)
	}
}
