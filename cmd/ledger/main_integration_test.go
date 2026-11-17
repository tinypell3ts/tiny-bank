//go:build integration

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/tinypell3ts/tiny-bank/pkg/db/dbtest"
	"github.com/tinypell3ts/tiny-bank/pkg/kit/kittest"
)

func post(t *testing.T, url, body string) (int, string) {
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
	return resp.StatusCode, string(b)
}

// TestLedgerAPIEndToEnd runs the real service against Postgres: it migrates,
// takes money in from the GBP funding account, moves some on, refuses an
// overdraft, and answers a retry with the first journal.
func TestLedgerAPIEndToEnd(t *testing.T) {
	url := dbtest.StartPostgres(t, "s3cret")
	base, logs := kittest.Start(t, run, map[string]string{"DATABASE_URL": url})
	if !strings.Contains(logs.String(), `"applied":["00001_create_ledger.sql"]`) {
		t.Errorf("expected the migration to be logged, got:\n%s", logs)
	}

	const (
		funding = "00000000-0000-4000-8000-000000000826"
		alice   = "a11ce000-0000-4000-8000-000000000001"
		bob     = "b0b00000-0000-4000-8000-000000000002"
	)
	journal := func(key, from, to string, amount int) string {
		return `{"idempotency_key":"` + key + `","currency":"GBP","entries":[` +
			`{"account_id":"` + from + `","amount":-` + strconv.Itoa(amount) + `},{"account_id":"` + to + `","amount":` + strconv.Itoa(amount) + `}]}`
	}

	if status, body := post(t, base+"/journals", journal("deposit-1", funding, alice, 1000)); status != http.StatusCreated {
		t.Fatalf("deposit = %d %s, want 201", status, body)
	}
	status, first := post(t, base+"/journals", journal("t1", alice, bob, 300))
	if status != http.StatusCreated {
		t.Fatalf("transfer = %d %s, want 201", status, first)
	}
	if status, again := post(t, base+"/journals", journal("t1", alice, bob, 300)); status != http.StatusOK || again != first {
		t.Errorf("retry = %d %s, want 200 and the first journal %s", status, again, first)
	}
	if status, body := post(t, base+"/journals", journal("t2", alice, bob, 701)); status != http.StatusPreconditionFailed ||
		!strings.Contains(body, "precondition_failed.insufficient_funds") {
		t.Errorf("overdraft = %d %s, want 412 insufficient_funds", status, body)
	}

	for account, want := range map[string]string{alice: "700", bob: "300", funding: "-1000"} {
		status, body := kittest.Get(t, base+"/accounts/"+account+"/balance")
		var got struct {
			Amount string `json:"amount"`
		}
		if err := json.Unmarshal([]byte(body), &got); status != http.StatusOK || err != nil || got.Amount != want {
			t.Errorf("balance of %s = %d %s, want %s", account, status, body, want)
		}
	}

	var j struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(first), &j); err != nil {
		t.Fatal(err)
	}
	if status, body := kittest.Get(t, base+"/journals/"+j.ID); status != http.StatusOK || body != first {
		t.Errorf("GET journal = %d %s, want %s", status, body, first)
	}
	if strings.Contains(logs.String(), "s3cret") {
		t.Error("logs leak the database password")
	}
}
