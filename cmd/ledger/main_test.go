package main

import (
	"strings"
	"testing"

	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/kit/kittest"
)

// TestConformance checks the ledger follows every rule services get from
// pkg/kit: health, request IDs, errors and metrics.
func TestConformance(t *testing.T) {
	env := map[string]string{"ACCOUNT_URL": "http://account"}
	kittest.ConformanceOf(t, func(r *typhon.Router) {
		if err := routes(r, nil, func(k string) string { return env[k] }); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRoutesNeedsAnAccountURL(t *testing.T) {
	tests := map[string]struct{ raw, want, wantErr string }{
		"the cluster's":    {raw: "http://account", want: "http://account"},
		"a trailing slash": {raw: "http://account/", want: "http://account"},
		"with a port":      {raw: "http://127.0.0.1:8081", want: "http://127.0.0.1:8081"},
		"missing":          {raw: "", wantErr: "ACCOUNT_URL is required"},
		"no scheme":        {raw: "account", wantErr: "ACCOUNT_URL must be"},
		"another scheme":   {raw: "ftp://account", wantErr: "ACCOUNT_URL must be"},
		"with a path":      {raw: "http://account/accounts", wantErr: "ACCOUNT_URL must be"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := baseURL(tt.raw)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("baseURL(%q) = %q, %v; want an error containing %q", tt.raw, got, err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("baseURL(%q) = %q, %v; want %q", tt.raw, got, err, tt.want)
			}
		})
	}
}
