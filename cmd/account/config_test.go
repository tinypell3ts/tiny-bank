package main

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

const testURL = "postgres://app:s3cret@db:5432/app" //nolint:gosec // fake credentials, to check errors never echo them

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    config
		wantErr string // substring; empty means no error expected
	}{
		{
			name: "defaults when only DATABASE_URL is set",
			env:  map[string]string{"DATABASE_URL": testURL},
			want: config{Port: "8080", LogLevel: slog.LevelInfo, ShutdownTimeout: 10 * time.Second, DatabaseURL: testURL},
		},
		{
			name: "all values set",
			env:  map[string]string{"PORT": "9000", "LOG_LEVEL": "debug", "SHUTDOWN_TIMEOUT": "30s", "DATABASE_URL": testURL},
			want: config{Port: "9000", LogLevel: slog.LevelDebug, ShutdownTimeout: 30 * time.Second, DatabaseURL: testURL},
		},
		{name: "database URL missing", env: map[string]string{}, wantErr: "DATABASE_URL is required"},
		{name: "port not a number", env: map[string]string{"PORT": "http", "DATABASE_URL": testURL}, wantErr: "PORT"},
		{name: "unknown log level", env: map[string]string{"LOG_LEVEL": "loud", "DATABASE_URL": testURL}, wantErr: "LOG_LEVEL"},
		{name: "timeout not positive", env: map[string]string{"SHUTDOWN_TIMEOUT": "0s", "DATABASE_URL": testURL}, wantErr: "SHUTDOWN_TIMEOUT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := loadConfig(func(k string) string { return tt.env[k] })

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want error containing %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "s3cret") {
					t.Errorf("error leaks the database password: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("config = %+v, want %+v", got, tt.want)
			}
		})
	}
}
