//go:build integration

package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/monzo/typhon"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/tinypell3ts/tiny-bank/pkg/db/dbtest"
	"github.com/tinypell3ts/tiny-bank/pkg/kit/kittest"
	"github.com/tinypell3ts/tiny-bank/services/account"
	"github.com/tinypell3ts/tiny-bank/services/ledger"
)

func post(t *testing.T, url, body string) (int, http.Header, string) {
	t.Helper()
	return kittest.Post(t, url, body, nil)
}

// startService runs routes as a service, for the transaction service to call.
func startService(t *testing.T, name string, routes func(*typhon.Router)) string {
	t.Helper()
	url, _ := kittest.Serve(t, name, routes)
	return url
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

// bank is three real services, each on its own Postgres: the account
// service, the ledger (checking accounts with it), and the transaction
// service (posting to the ledger).
type bank struct {
	base, ledgerURL, accountURL string
	logs                        *kittest.Logs
}

func startBank(t *testing.T) bank {
	t.Helper()
	accountURL := startService(t, "account", func(r *typhon.Router) {
		account.RegisterRoutes(r, account.NewStore(dbtest.Migrated(t, account.Migrations())))
	})
	ledgerURL := startService(t, "ledger", func(r *typhon.Router) {
		store := ledger.NewStore(dbtest.Migrated(t, ledger.Migrations()))
		ledger.RegisterRoutes(r, ledger.NewService(store, ledger.AccountService{BaseURL: accountURL}, prometheus.NewRegistry()))
	})
	base, logs := kittest.Start(t, run, map[string]string{"DATABASE_URL": dbtest.Database(t), "LEDGER_URL": ledgerURL})
	return bank{base: base, ledgerURL: ledgerURL, accountURL: accountURL, logs: logs}
}

// open opens a GBP account with the account service and returns its ID.
func (b bank) open(t *testing.T) string {
	t.Helper()
	_, _, body := post(t, b.accountURL+"/accounts", `{"customer_id":"c0ffee00-0000-4000-8000-000000000001","holder_name":"Ada","currency":"GBP"}`)
	var acc struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &acc); err != nil || acc.ID == "" {
		t.Fatalf("open account: %s", body)
	}
	return acc.ID
}

// TestTransactionAPIEndToEnd deposits money, moves some, answers a retry with
// the same transfer, and records a refusal as a failed transfer, and the
// ledger's balances agree.
func TestTransactionAPIEndToEnd(t *testing.T) {
	b := startBank(t)
	if !strings.Contains(b.logs.String(), `"applied":["00001_create_transfers.sql","00002_index_due_transfers.sql","00003_lease_transfers.sql"]`) {
		t.Errorf("expected the migration to be logged, got:\n%s", b.logs)
	}
	alice, bob := b.open(t), b.open(t)

	status, _, body := post(t, b.base+"/deposits", `{"idempotency_key":"d1","account_id":"`+alice+`","currency":"GBP","amount":1000}`)
	if d := decode(t, body); status != http.StatusCreated || d.State != "TRANSFER_STATE_COMPLETED" || d.JournalID == "" {
		t.Fatalf("deposit = %d %s, want 201 completed with a journal", status, body)
	}

	transfer := `{"idempotency_key":"t1","from_account_id":"` + alice + `","to_account_id":"` + bob + `","currency":"GBP","amount":300}`
	status, header, first := post(t, b.base+"/transfers", transfer)
	tr := decode(t, first)
	if status != http.StatusCreated || tr.State != "TRANSFER_STATE_COMPLETED" || header.Get("Location") != "/transfers/"+tr.ID {
		t.Fatalf("transfer = %d %s, want 201 completed with its Location", status, first)
	}
	if again, _, b := post(t, b.base+"/transfers", transfer); again != http.StatusOK || decode(t, b).ID != tr.ID {
		t.Errorf("retry = %d %s, want 200 with the same transfer", again, b)
	}

	tooMuch := `{"idempotency_key":"t2","from_account_id":"` + alice + `","to_account_id":"` + bob + `","currency":"GBP","amount":5000}`
	status, _, body = post(t, b.base+"/transfers", tooMuch)
	if f := decode(t, body); status != http.StatusCreated || f.State != "TRANSFER_STATE_FAILED" || f.FailureReason != "precondition_failed.insufficient_funds" {
		t.Errorf("overdraft = %d %s, want 201 failed for insufficient funds", status, body)
	}

	for id, want := range map[string]string{alice: "700", bob: "300"} {
		_, got := kittest.Get(t, b.ledgerURL+"/accounts/"+id+"/balance")
		if !strings.Contains(got, `"amount":"`+want+`"`) {
			t.Errorf("ledger balance of %s = %s, want %s", id, got, want)
		}
	}
	if status, got := kittest.Get(t, b.base+"/transfers/"+tr.ID); status != http.StatusOK || decode(t, got).JournalID != tr.JournalID {
		t.Errorf("GET = %d %s, want the completed transfer", status, got)
	}
	if strings.Contains(b.logs.String(), dbtest.Password) {
		t.Error("logs leak the database password")
	}
}

// TestADepositIsOneTrace: one deposit, sent with a traceparent, is one trace
// across all three services, each hop under the one that called it:
// transaction → ledger → account.
func TestADepositIsOneTrace(t *testing.T) {
	exporter := kittest.RecordSpans(t)

	b := startBank(t)
	alice := b.open(t)
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	header := http.Header{"Traceparent": {"00-" + traceID + "-00f067aa0ba902b7-01"}}
	if status, _, body := kittest.Post(t, b.base+"/deposits", `{"idempotency_key":"d1","account_id":"`+alice+`","currency":"GBP","amount":1000}`, header); status != http.StatusCreated {
		t.Fatalf("deposit = %d %s", status, body)
	}

	byName := map[string]tracetest.SpanStub{}
	for _, s := range exporter.GetSpans() {
		if s.SpanContext.TraceID().String() == traceID {
			byName[s.Name] = s
		}
	}
	host := func(base string) string { // a client span is named by host name, without the port
		u, err := url.Parse(base)
		if err != nil {
			t.Fatal(err)
		}
		return u.Hostname()
	}
	chain := []string{ // each span's parent is the one before it
		"POST /deposits",
		"POST " + host(b.ledgerURL),
		"POST /journals",
		"GET " + host(b.accountURL),
		"GET /accounts/:id",
	}
	for _, name := range chain {
		if _, ok := byName[name]; !ok {
			t.Fatalf("no %q span in trace %s; got %v", name, traceID, names(byName))
		}
	}
	for i := 1; i < len(chain); i++ {
		parent, child := byName[chain[i-1]], byName[chain[i]]
		if child.Parent.SpanID() != parent.SpanContext.SpanID() {
			t.Errorf("%q isn't under %q", chain[i], chain[i-1])
		}
	}
}

func names(m map[string]tracetest.SpanStub) []string {
	var out []string
	for n := range m {
		out = append(out, n)
	}
	return out
}
