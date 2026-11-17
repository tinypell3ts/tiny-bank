package kit

import (
	"strings"
	"testing"
)

func TestServiceURL(t *testing.T) {
	tests := map[string]struct{ raw, want, wantErr string }{
		"the cluster's":    {raw: "http://account", want: "http://account"},
		"a trailing slash": {raw: "http://account/", want: "http://account"},
		"with a port":      {raw: "http://127.0.0.1:8081", want: "http://127.0.0.1:8081"},
		"missing":          {raw: "", wantErr: "ACCOUNT_URL is required"},
		"no scheme":        {raw: "account", wantErr: "ACCOUNT_URL must be"},
		"another scheme":   {raw: "ftp://account", wantErr: "ACCOUNT_URL must be"},
		"with a path":      {raw: "http://account/accounts", wantErr: "ACCOUNT_URL must be"},
		"with a query":     {raw: "http://account?x=1", wantErr: "ACCOUNT_URL must be"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ServiceURL(func(string) string { return tt.raw }, "ACCOUNT_URL")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ServiceURL(%q) = %q, %v; want an error containing %q", tt.raw, got, err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ServiceURL(%q) = %q, %v; want %q", tt.raw, got, err, tt.want)
			}
		})
	}
}
