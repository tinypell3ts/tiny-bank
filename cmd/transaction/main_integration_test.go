//go:build integration

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tinypell3ts/tiny-bank/pkg/db/dbtest"
	"github.com/tinypell3ts/tiny-bank/pkg/kit/kittest"
)

// post sends body as JSON and returns the status, headers and body. It reads
// and closes the response itself.
func post(t *testing.T, url, body string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(b)
}

// TestTransactionAPIEndToEnd runs the real service against Postgres: it
// migrates, saves a transfer as pending, answers a retry with the same
// transfer, and serves it back. Calling the ledger comes in the next step.
func TestTransactionAPIEndToEnd(t *testing.T) {
	base, logs := kittest.Start(t, run, map[string]string{"DATABASE_URL": dbtest.StartPostgres(t, "s3cret")})
	if !strings.Contains(logs.String(), `"applied":["00001_create_transfers.sql"]`) {
		t.Errorf("expected the migration to be logged, got:\n%s", logs)
	}

	body := `{"idempotency_key":"t1","from_account_id":"a11ce000-0000-4000-8000-000000000001",` +
		`"to_account_id":"b0b00000-0000-4000-8000-000000000002","currency":"GBP","amount":300}`
	status, header, first := post(t, base+"/transfers", body)
	var tr struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(first), &tr); err != nil || status != http.StatusAccepted || tr.State != "TRANSFER_STATE_PENDING" {
		t.Fatalf("POST /transfers = %d %s, want 202 pending", status, first)
	}
	if header.Get("Location") != "/transfers/"+tr.ID {
		t.Errorf("Location = %q, want /transfers/%s", header.Get("Location"), tr.ID)
	}
	if again, _, b := post(t, base+"/transfers", body); again != http.StatusAccepted || !strings.Contains(b, tr.ID) {
		t.Errorf("retry = %d %s, want 202 with the same transfer", again, b)
	}
	if status, got := kittest.Get(t, base+"/transfers/"+tr.ID); status != http.StatusOK || !strings.Contains(got, `"idempotency_key":"t1"`) {
		t.Errorf("GET = %d %s, want the transfer", status, got)
	}
	if strings.Contains(logs.String(), "s3cret") {
		t.Error("logs leak the database password")
	}
}
