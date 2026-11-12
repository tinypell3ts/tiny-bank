package db

import (
	"context"
	"strings"
	"testing"
	"time"
)

const secret = "s3cret-password" // must never appear in an error

func TestOpenFailsWithoutLeakingThePassword(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr string // substring
	}{
		{
			name:    "not a URL",
			url:     "postgres://app:" + secret + "@localhost:notaport/app",
			wantErr: "invalid DATABASE_URL",
		},
		{
			name:    "key=value form",
			url:     "host=localhost port=x user=app password=" + secret,
			wantErr: "invalid DATABASE_URL",
		},
		{
			// Port 1 on localhost refuses connections straight away.
			name:    "nothing listening",
			url:     "postgres://app:" + secret + "@127.0.0.1:1/app?sslmode=disable",
			wantErr: "connection refused",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			pool, err := Open(ctx, tt.url)
			if err == nil {
				pool.Close()
				t.Fatal("Open succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %q, want it to contain %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("err leaks the password: %q", err)
			}
		})
	}
}

func TestOpenGivesUpQuicklyWhenUnreachable(t *testing.T) {
	// 10.255.255.1 is a private address nothing answers on, so the connection
	// attempt hangs until a timeout. The caller's context must win.
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	pool, err := Open(ctx, "postgres://app:"+secret+"@10.255.255.1:5432/app?sslmode=disable")
	if err == nil {
		pool.Close()
		t.Fatal("Open succeeded, want an error")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Open took %v, want it to stop when the context ends", took)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("err leaks the password: %q", err)
	}
}
