//go:build integration

package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/db"
	"github.com/tinypell3ts/tiny-bank/pkg/db/dbtest"
	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/kit/kittest"
	"github.com/tinypell3ts/tiny-bank/pkg/obs"
	"github.com/tinypell3ts/tiny-bank/services/account"
)

// post sends body as JSON, with requestID as its X-Request-Id if set.
func post(t *testing.T, url, body, requestID string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if requestID != "" {
		req.Header.Set("X-Request-Id", requestID)
	}
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

// accountService runs the real account service's routes, on its own
// Postgres, behind kit.Handler, and returns its URL and logs. The ledger
// checks accounts with it.
func accountService(t *testing.T) (string, *kittest.Logs) {
	t.Helper()
	router := typhon.Router{}
	account.RegisterRoutes(&router, account.NewStore(dbtest.Migrated(t, account.Migrations())))
	logs := &kittest.Logs{}
	var ready atomic.Bool
	ready.Store(true)
	srv := httptest.NewServer(kit.Handler(kit.NewLogger(logs, slog.LevelInfo, "account"), obs.NewRegistry(), &router, &ready, nil))
	t.Cleanup(srv.Close)
	return srv.URL, logs
}

// openAccount opens a GBP account with the account service and returns its ID.
func openAccount(t *testing.T, accountURL string) string {
	t.Helper()
	status, body := post(t, accountURL+"/accounts", `{"customer_id":"c0ffee00-0000-4000-8000-000000000001","holder_name":"Ada","currency":"GBP"}`, "")
	var acc struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &acc); status != http.StatusCreated || err != nil {
		t.Fatalf("open account = %d %s", status, body)
	}
	return acc.ID
}

// TestLedgerAPIEndToEnd runs the real ledger against Postgres and a real
// account service: it migrates, takes money in from the GBP funding account,
// moves some on, refuses an overdraft and an account that doesn't exist, and
// answers a retry with the first journal.
func TestLedgerAPIEndToEnd(t *testing.T) {
	accountURL, accountLogs := accountService(t)
	alice, bob := openAccount(t, accountURL), openAccount(t, accountURL)

	url := dbtest.StartPostgres(t, "s3cret")
	base, logs := kittest.Start(t, run, map[string]string{"DATABASE_URL": url, "ACCOUNT_URL": accountURL})
	if !strings.Contains(logs.String(), `"applied":["00001_create_ledger.sql","00002_append_only_journals.sql"]`) {
		t.Errorf("expected the migration to be logged, got:\n%s", logs)
	}

	const funding = "00000000-0000-4000-8000-000000000826"
	journal := func(key, from, to string, amount int) string {
		return `{"idempotency_key":"` + key + `","currency":"GBP","entries":[` +
			`{"account_id":"` + from + `","amount":-` + strconv.Itoa(amount) + `},{"account_id":"` + to + `","amount":` + strconv.Itoa(amount) + `}]}`
	}

	if status, body := post(t, base+"/journals", journal("deposit-1", funding, alice, 1000), "trace-me-1"); status != http.StatusCreated {
		t.Fatalf("deposit = %d %s, want 201", status, body)
	}
	// The ledger checked alice with the account service, passing our request ID on.
	if !strings.Contains(accountLogs.String(), `"request_id":"trace-me-1"`) {
		t.Errorf("the account service didn't see the ledger's request ID; its logs:\n%s", accountLogs)
	}
	// And the journal records it, so the audit trail leads back to the logs.
	pool, err := db.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var requestID string
	if err := pool.QueryRow(t.Context(), `SELECT request_id FROM journals WHERE idempotency_key = 'deposit-1'`).Scan(&requestID); err != nil || requestID != "trace-me-1" {
		t.Errorf("journal's request_id = %q, %v; want trace-me-1", requestID, err)
	}

	status, first := post(t, base+"/journals", journal("t1", alice, bob, 300), "")
	if status != http.StatusCreated {
		t.Fatalf("transfer = %d %s, want 201", status, first)
	}
	if status, again := post(t, base+"/journals", journal("t1", alice, bob, 300), ""); status != http.StatusOK || again != first {
		t.Errorf("retry = %d %s, want 200 and the first journal %s", status, again, first)
	}
	if status, body := post(t, base+"/journals", journal("t2", alice, bob, 701), ""); status != http.StatusPreconditionFailed ||
		!strings.Contains(body, "precondition_failed.insufficient_funds") {
		t.Errorf("overdraft = %d %s, want 412 insufficient_funds", status, body)
	}
	typo := "b0b00000-0000-4000-8000-000000000002" // no such account
	if status, body := post(t, base+"/journals", journal("t3", alice, typo, 100), ""); status != http.StatusNotFound ||
		!strings.Contains(body, "not_found.account") {
		t.Errorf("transfer to a missing account = %d %s, want 404 not_found.account", status, body)
	}

	for id, want := range map[string]string{alice: "700", bob: "300", funding: "-1000"} {
		status, body := kittest.Get(t, base+"/accounts/"+id+"/balance")
		var got struct {
			Amount string `json:"amount"`
		}
		if err := json.Unmarshal([]byte(body), &got); status != http.StatusOK || err != nil || got.Amount != want {
			t.Errorf("balance of %s = %d %s, want %s", id, status, body, want)
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
	// A request the handler refuses before Post is counted too.
	if status, _ := post(t, base+"/journals", `{"idempotency_key":"t4","currency":"XYZ","entries":[]}`, ""); status != http.StatusBadRequest {
		t.Errorf("unsupported currency = %d, want 400", status)
	}

	// The business metrics: two journals posted (the deposit and t1), one
	// retry, and two refusals.
	_, metrics := kittest.Get(t, base+"/metrics")
	for _, want := range []string{
		`ledger_journals_total{currency="GBP",outcome="posted",reason=""} 2`,
		`ledger_journals_total{currency="GBP",outcome="retried",reason=""} 1`,
		`ledger_journals_total{currency="GBP",outcome="refused",reason="precondition_failed.insufficient_funds"} 1`,
		`ledger_journals_total{currency="GBP",outcome="refused",reason="not_found.account"} 1`,
		`ledger_moved_minor_units_total{currency="GBP"} 1300`,
		`ledger_journals_total{currency="unknown",outcome="refused",reason="bad_request.currency"} 1`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("/metrics is missing %s", want)
		}
	}
	if strings.Contains(logs.String(), "s3cret") {
		t.Error("logs leak the database password")
	}
}
