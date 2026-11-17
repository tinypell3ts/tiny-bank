//go:build integration

package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/monzo/typhon"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/tinypell3ts/tiny-bank/pkg/db/dbtest"
	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/kit/kittest"
	"github.com/tinypell3ts/tiny-bank/pkg/obs"
	"github.com/tinypell3ts/tiny-bank/services/account"
	"github.com/tinypell3ts/tiny-bank/services/ledger"
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

// startService runs routes behind kit.Handler on a test server, as a real
// service runs, and returns its URL.
func startService(t *testing.T, routes func(*typhon.Router)) string {
	t.Helper()
	router := typhon.Router{}
	routes(&router)
	var ready atomic.Bool
	ready.Store(true)
	srv := httptest.NewServer(kit.Handler(slog.New(slog.DiscardHandler), obs.NewRegistry(), &router, &ready, nil))
	t.Cleanup(srv.Close)
	return srv.URL
}

type transferBody struct {
	ID            string `json:"id"`
	State         string `json:"state"`
	JournalID     string `json:"journal_id"`
	FailureReason string `json:"failure_reason"`
}

func decode(t *testing.T, body string) transferBody {
	t.Helper()
	var b transferBody
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		t.Fatalf("not a transfer: %v: %s", err, body)
	}
	return b
}

// TestTransactionAPIEndToEnd runs three real services, each on its own
// Postgres: the account service, the ledger (checking accounts with it), and
// the transaction service (posting to the ledger). It deposits money, moves
// some, answers a retry with the same transfer, and records a refusal as a
// failed transfer, and the ledger's balances agree.
func TestTransactionAPIEndToEnd(t *testing.T) {
	accountURL := startService(t, func(r *typhon.Router) {
		account.RegisterRoutes(r, account.NewStore(dbtest.Migrated(t, account.Migrations())))
	})
	ledgerURL := startService(t, func(r *typhon.Router) {
		store := ledger.NewStore(dbtest.Migrated(t, ledger.Migrations()))
		ledger.RegisterRoutes(r, ledger.NewService(store, ledger.AccountService{BaseURL: accountURL}, prometheus.NewRegistry()))
	})
	base, logs := kittest.Start(t, run, map[string]string{"DATABASE_URL": dbtest.StartPostgres(t, "s3cret"), "LEDGER_URL": ledgerURL})
	if !strings.Contains(logs.String(), `"applied":["00001_create_transfers.sql"]`) {
		t.Errorf("expected the migration to be logged, got:\n%s", logs)
	}

	open := func() string {
		_, _, body := post(t, accountURL+"/accounts", `{"customer_id":"c0ffee00-0000-4000-8000-000000000001","holder_name":"Ada","currency":"GBP"}`)
		var acc struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(body), &acc); err != nil || acc.ID == "" {
			t.Fatalf("open account: %s", body)
		}
		return acc.ID
	}
	alice, bob := open(), open()

	status, _, body := post(t, base+"/deposits", `{"idempotency_key":"d1","account_id":"`+alice+`","currency":"GBP","amount":1000}`)
	if d := decode(t, body); status != http.StatusCreated || d.State != "TRANSFER_STATE_COMPLETED" || d.JournalID == "" {
		t.Fatalf("deposit = %d %s, want 201 completed with a journal", status, body)
	}

	transfer := `{"idempotency_key":"t1","from_account_id":"` + alice + `","to_account_id":"` + bob + `","currency":"GBP","amount":300}`
	status, header, first := post(t, base+"/transfers", transfer)
	tr := decode(t, first)
	if status != http.StatusCreated || tr.State != "TRANSFER_STATE_COMPLETED" || header.Get("Location") != "/transfers/"+tr.ID {
		t.Fatalf("transfer = %d %s, want 201 completed with its Location", status, first)
	}
	if again, _, b := post(t, base+"/transfers", transfer); again != http.StatusOK || decode(t, b).ID != tr.ID {
		t.Errorf("retry = %d %s, want 200 with the same transfer", again, b)
	}

	tooMuch := `{"idempotency_key":"t2","from_account_id":"` + alice + `","to_account_id":"` + bob + `","currency":"GBP","amount":5000}`
	status, _, body = post(t, base+"/transfers", tooMuch)
	if f := decode(t, body); status != http.StatusCreated || f.State != "TRANSFER_STATE_FAILED" || f.FailureReason != "precondition_failed.insufficient_funds" {
		t.Errorf("overdraft = %d %s, want 201 failed for insufficient funds", status, body)
	}

	for id, want := range map[string]string{alice: "700", bob: "300"} {
		_, got := kittest.Get(t, ledgerURL+"/accounts/"+id+"/balance")
		if !strings.Contains(got, `"amount":"`+want+`"`) {
			t.Errorf("ledger balance of %s = %s, want %s", id, got, want)
		}
	}
	if status, got := kittest.Get(t, base+"/transfers/"+tr.ID); status != http.StatusOK || decode(t, got).JournalID != tr.JournalID {
		t.Errorf("GET = %d %s, want the completed transfer", status, got)
	}
	if strings.Contains(logs.String(), "s3cret") {
		t.Error("logs leak the database password")
	}
}
