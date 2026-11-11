package main

import (
	"io"
	"strings"
	"testing"
)

func TestRunRejectsBadConfig(t *testing.T) {
	getenv := func(k string) string {
		if k == "PORT" {
			return "not-a-port"
		}
		return ""
	}

	err := run(t.Context(), getenv, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "load config") {
		t.Fatalf("err = %v, want a load config error", err)
	}
}
