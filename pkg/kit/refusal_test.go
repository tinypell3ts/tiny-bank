package kit

import (
	"errors"
	"fmt"
	"testing"

	"github.com/monzo/terrors"
)

func TestRefusal(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode string
		want     bool
	}{
		{"a bad request", terrors.BadRequest("amount", "x", nil), "bad_request.amount", true},
		{"not found", terrors.NotFound("account", "x", nil), "not_found.account", true},
		{"a precondition, wrapped", fmt.Errorf("post: %w", terrors.PreconditionFailed("insufficient_funds", "x", nil)), "precondition_failed.insufficient_funds", true},
		{"an internal error", terrors.InternalService("db", "x", nil), "", false},
		{"a timeout", terrors.Timeout("deadline", "x", nil), "", false},
		{"not a terror", errors.New("connection refused"), "", false},
		{"no error", nil, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code, ok := Refusal(tt.err); code != tt.wantCode || ok != tt.want {
				t.Errorf("Refusal = %q, %v; want %q, %v", code, ok, tt.wantCode, tt.want)
			}
		})
	}
}
