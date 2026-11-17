package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/monzo/terrors"
	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/money"
	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// fakeLedger keeps journals in memory. Post applies the same rules as the
// real Store (it calls Validate) and treats a reused key as a retry. The
// Store's own rules, like overdrafts and wrong-key reuse, are tested against
// Postgres in store_integration_test.go.
type fakeLedger struct {
	journals map[uuid.UUID]Journal
	byKey    map[string]uuid.UUID
	balances map[uuid.UUID]Balance
	posts    int      // calls to Post
	refusals []string // calls to CountRefusal, as "<currency label> <code>"
}

func (f *fakeLedger) CountRefusal(currency money.Currency, err error) {
	var terr *terrors.Error
	errors.As(err, &terr)
	f.refusals = append(f.refusals, currencyLabel(currency)+" "+terr.Code)
}

func newFake() *fakeLedger {
	return &fakeLedger{journals: map[uuid.UUID]Journal{}, byKey: map[string]uuid.UUID{}, balances: map[uuid.UUID]Balance{}}
}

func (f *fakeLedger) Post(_ context.Context, req NewJournal) (Journal, bool, error) {
	f.posts++
	req, err := req.Validate()
	if err != nil {
		return Journal{}, false, err
	}
	if id, ok := f.byKey[req.IdempotencyKey]; ok {
		return f.journals[id], false, nil
	}
	j := Journal{
		ID: uuid.New(), IdempotencyKey: req.IdempotencyKey, Currency: req.Entries[0].Amount.Currency(),
		Entries: req.Entries, CreatedAt: time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC),
	}
	f.journals[j.ID], f.byKey[j.IdempotencyKey] = j, j.ID
	return j, true, nil
}

func (f *fakeLedger) Journal(_ context.Context, id uuid.UUID) (Journal, error) {
	j, ok := f.journals[id]
	if !ok {
		return Journal{}, errJournalNotFound()
	}
	return j, nil
}

func (f *fakeLedger) Balance(_ context.Context, id uuid.UUID) (Balance, error) {
	b, ok := f.balances[id]
	if !ok {
		return Balance{}, errAccountNotFound()
	}
	return b, nil
}

// serve sends one request to the ledger API as it runs in production, behind
// kit.Handler's filters.
func serve(t *testing.T, ledger Ledger, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	router := typhon.Router{}
	RegisterRoutes(&router, ledger)
	h := kit.Handler(slog.New(slog.DiscardHandler), obs.NewRegistry(), &router, new(atomic.Bool), nil)
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body isn't JSON: %v: %s", err, rec.Body)
	}
	return e.Code
}

// journalBody is a journal as JSON callers see it. Amounts are strings:
// proto3 JSON writes every 64-bit integer that way, as JavaScript numbers
// can't hold them exactly.
type journalBody struct {
	ID             string `json:"id"`
	IdempotencyKey string `json:"idempotency_key"`
	Currency       string `json:"currency"`
	Entries        []struct {
		AccountID string `json:"account_id"`
		Amount    string `json:"amount"`
	} `json:"entries"`
	CreatedAt string `json:"created_at"`
}

const transferJSON = `{"idempotency_key":"t1","currency":"GBP","entries":[` +
	`{"account_id":"b0b00000-0000-4000-8000-000000000002","amount":300},` +
	`{"account_id":"a11ce000-0000-4000-8000-000000000001","amount":"-300"}]}`

func TestPostJournal(t *testing.T) {
	ledger := newFake()
	rec := serve(t, ledger, http.MethodPost, "/journals", transferJSON)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
	var got journalBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if rec.Header().Get("Location") != "/journals/"+got.ID {
		t.Errorf("Location = %q, want /journals/%s", rec.Header().Get("Location"), got.ID)
	}
	// Sorted by account ID, and amounts as strings, whichever form came in.
	if got.IdempotencyKey != "t1" || got.Currency != "GBP" || got.CreatedAt != "2026-11-17T10:00:00Z" || len(got.Entries) != 2 ||
		got.Entries[0].AccountID != alice.String() || got.Entries[0].Amount != "-300" ||
		got.Entries[1].AccountID != bob.String() || got.Entries[1].Amount != "300" {
		t.Errorf("journal = %+v", got)
	}

	// The same request again is a retry: the first journal, as a 200.
	again := serve(t, ledger, http.MethodPost, "/journals", transferJSON)
	if again.Code != http.StatusOK || again.Header().Get("Location") != "" || !strings.Contains(again.Body.String(), got.ID) {
		t.Errorf("retry = %d, Location %q, %s; want 200 with the first journal", again.Code, again.Header().Get("Location"), again.Body)
	}
}

func TestPostJournalRefusesBadInput(t *testing.T) {
	entries := `"entries":[{"account_id":"b0b00000-0000-4000-8000-000000000002","amount":300},{"account_id":"a11ce000-0000-4000-8000-000000000001","amount":-300}]`
	// counted is what CountRefusal should get for a request the handler
	// refuses itself; empty means it reaches Post, which counts its own.
	tests := []struct {
		name, body, wantCode, counted string
	}{
		{"no currency", `{"idempotency_key":"t1",` + entries + `}`, "bad_request.currency", "unknown bad_request.currency"},
		{"unsupported currency", `{"idempotency_key":"t1","currency":"XYZ",` + entries + `}`, "bad_request.currency", "unknown bad_request.currency"},
		{"no key", `{"currency":"GBP",` + entries + `}`, "bad_request.idempotency_key", ""},
		{"upper-case account ID", `{"idempotency_key":"t1","currency":"GBP","entries":[` +
			`{"account_id":"B0B00000-0000-4000-8000-000000000002","amount":300},{"account_id":"a11ce000-0000-4000-8000-000000000001","amount":-300}]}`,
			"bad_request.entries", "GBP bad_request.entries"},
		{"doesn't add up", `{"idempotency_key":"t1","currency":"GBP","entries":[` +
			`{"account_id":"b0b00000-0000-4000-8000-000000000002","amount":300},{"account_id":"a11ce000-0000-4000-8000-000000000001","amount":-299}]}`,
			"bad_request.entries", ""},
		{"amount with a fraction", `{"idempotency_key":"t1","currency":"GBP","entries":[` +
			`{"account_id":"b0b00000-0000-4000-8000-000000000002","amount":3.5},{"account_id":"a11ce000-0000-4000-8000-000000000001","amount":-3.5}]}`,
			"bad_request.invalid_json", "unknown bad_request.invalid_json"},
		{"unknown field", `{"idempotency_key":"t1","currency":"GBP","memo":"hi",` + entries + `}`, "bad_request.invalid_json", "unknown bad_request.invalid_json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ledger := newFake()
			rec := serve(t, ledger, http.MethodPost, "/journals", tt.body)
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != tt.wantCode {
				t.Errorf("= %d %s, want 400 %s", rec.Code, rec.Body, tt.wantCode)
			}
			// Every refusal is counted exactly once: by the handler if it
			// stopped the request, otherwise by Post.
			if tt.counted == "" {
				if len(ledger.refusals) != 0 || ledger.posts != 1 {
					t.Errorf("refusals %v, posts %d; want Post to count it, once", ledger.refusals, ledger.posts)
				}
			} else if len(ledger.refusals) != 1 || ledger.refusals[0] != tt.counted || ledger.posts != 0 {
				t.Errorf("refusals %v, posts %d; want [%s] and no post", ledger.refusals, ledger.posts, tt.counted)
			}
		})
	}
}

func TestGetJournal(t *testing.T) {
	ledger := newFake()
	created := serve(t, ledger, http.MethodPost, "/journals", transferJSON)
	var posted journalBody
	if err := json.Unmarshal(created.Body.Bytes(), &posted); err != nil {
		t.Fatal(err)
	}

	rec := serve(t, ledger, http.MethodGet, "/journals/"+posted.ID, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"idempotency_key":"t1"`) {
		t.Errorf("GET = %d %s, want the journal", rec.Code, rec.Body)
	}
	for _, path := range []string{"/journals/" + uuid.New().String(), "/journals/" + strings.ToUpper(posted.ID), "/journals/nope"} {
		if rec := serve(t, ledger, http.MethodGet, path, ""); rec.Code != http.StatusNotFound || errorCode(t, rec) != "not_found.journal" {
			t.Errorf("GET %s = %d %s, want 404 not_found.journal", path, rec.Code, rec.Body)
		}
	}
}

func TestGetBalance(t *testing.T) {
	ledger := newFake()
	ledger.balances[alice] = Balance{AccountID: alice, Kind: KindCustomer, Amount: money.New(700, money.GBP())}

	rec := serve(t, ledger, http.MethodGet, "/accounts/"+alice.String()+"/balance", "")
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"account_id": alice.String(), "currency": "GBP", "amount": "700"}
	if rec.Code != http.StatusOK || len(got) != len(want) || got["account_id"] != want["account_id"] || got["currency"] != want["currency"] || got["amount"] != want["amount"] {
		t.Errorf("balance = %d %v, want %v", rec.Code, got, want)
	}
	for _, path := range []string{"/accounts/" + bob.String() + "/balance", "/accounts/nope/balance"} {
		if rec := serve(t, ledger, http.MethodGet, path, ""); rec.Code != http.StatusNotFound || errorCode(t, rec) != "not_found.account" {
			t.Errorf("GET %s = %d %s, want 404 not_found.account", path, rec.Code, rec.Body)
		}
	}
}
