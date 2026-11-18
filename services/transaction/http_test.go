package transaction

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/monzo/typhon"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// fakeTransfers keeps transfers in memory, applying the same rules as the
// Store (Validate, and the same key giving the same transfer back). finish,
// if set, decides what happens to a new transfer straight away, as the
// ledger will once the service calls it; otherwise it stays pending.
type fakeTransfers struct {
	byKey  map[string]Transfer
	byID   map[uuid.UUID]Transfer
	finish func(Transfer) Transfer
}

func newFake(finish func(Transfer) Transfer) *fakeTransfers {
	return &fakeTransfers{byKey: map[string]Transfer{}, byID: map[uuid.UUID]Transfer{}, finish: finish}
}

func (f *fakeTransfers) Create(_ context.Context, req NewTransfer) (Transfer, bool, error) {
	if err := req.Validate(); err != nil {
		return Transfer{}, false, err
	}
	if t, ok := f.byKey[req.IdempotencyKey]; ok {
		if !req.same(t) {
			return Transfer{}, false, errKeyReused()
		}
		return t, false, nil
	}
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	t := Transfer{ID: uuid.New(), IdempotencyKey: req.IdempotencyKey, Kind: req.Kind, From: req.From, To: req.To,
		TraceID: req.TraceID, RequestID: req.RequestID,
		Amount: req.Amount, State: StatePending, CreatedAt: now, UpdatedAt: now}
	if f.finish != nil {
		t = f.finish(t)
	}
	f.byKey[t.IdempotencyKey], f.byID[t.ID] = t, t
	return t, true, nil
}

func (f *fakeTransfers) Get(_ context.Context, id uuid.UUID) (Transfer, error) {
	t, ok := f.byID[id]
	if !ok {
		return Transfer{}, errNotFound()
	}
	return t, nil
}

func completed(t Transfer) Transfer {
	t.State, t.JournalID = StateCompleted, uuid.MustParse("10000000-0000-4000-8000-000000000001")
	return t
}

func failedForFunds(t Transfer) Transfer {
	t.State, t.FailureReason = StateFailed, "precondition_failed.insufficient_funds"
	return t
}

func serve(t *testing.T, transfers Transfers, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	router := typhon.Router{}
	RegisterRoutes(&router, transfers)
	var ready atomic.Bool
	ready.Store(true)
	h := kit.Handler(slog.New(slog.DiscardHandler), obs.NewRegistry(), &router, &ready, nil)
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// transferBody is a transfer as JSON callers see it.
type transferBody struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	FromAccountID  string `json:"from_account_id"`
	ToAccountID    string `json:"to_account_id"`
	Currency       string `json:"currency"`
	Amount         string `json:"amount"`
	State          string `json:"state"`
	JournalID      string `json:"journal_id"`
	FailureReason  string `json:"failure_reason"`
	IdempotencyKey string `json:"idempotency_key"`
}

func decodeTransfer(t *testing.T, rec *httptest.ResponseRecorder) transferBody {
	t.Helper()
	var b transferBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("body isn't a transfer: %v: %s", err, rec.Body)
	}
	return b
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

const transferJSON = `{"idempotency_key":"t1","from_account_id":"a11ce000-0000-4000-8000-000000000001",` +
	`"to_account_id":"b0b00000-0000-4000-8000-000000000002","currency":"GBP","amount":300}`

func TestAPendingTransferIsAccepted(t *testing.T) {
	rec := serve(t, newFake(nil), http.MethodPost, "/transfers", transferJSON)
	got := decodeTransfer(t, rec)
	if rec.Code != http.StatusAccepted || rec.Header().Get("Location") != "/transfers/"+got.ID {
		t.Fatalf("= %d, Location %q; want 202 with the transfer's Location: %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if got.State != "TRANSFER_STATE_PENDING" || got.Kind != "TRANSFER_KIND_TRANSFER" || got.Amount != "300" ||
		got.FromAccountID != alice.String() || got.ToAccountID != bob.String() || got.Currency != "GBP" {
		t.Errorf("transfer = %+v", got)
	}
}

func TestAFinishedTransferIsCreatedThenRepeated(t *testing.T) {
	transfers := newFake(completed)
	rec := serve(t, transfers, http.MethodPost, "/transfers", transferJSON)
	got := decodeTransfer(t, rec)
	if rec.Code != http.StatusCreated || rec.Header().Get("Location") != "/transfers/"+got.ID ||
		got.State != "TRANSFER_STATE_COMPLETED" || got.JournalID == "" {
		t.Fatalf("= %d %s, want 201 completed with its journal and Location", rec.Code, rec.Body)
	}
	// A retry with the same key finds it finished: the same transfer, 200.
	again := serve(t, transfers, http.MethodPost, "/transfers", transferJSON)
	if again.Code != http.StatusOK || again.Header().Get("Location") != "" || decodeTransfer(t, again).ID != got.ID {
		t.Errorf("retry = %d %s, want 200 with the first transfer", again.Code, again.Body)
	}
}

// TestARefusalIsAFailedTransferNotAnError: once saved, the answer is the
// transfer, so the first answer and every retry have the same shape.
func TestARefusalIsAFailedTransferNotAnError(t *testing.T) {
	transfers := newFake(failedForFunds)
	rec := serve(t, transfers, http.MethodPost, "/transfers", transferJSON)
	got := decodeTransfer(t, rec)
	if rec.Code != http.StatusCreated || got.State != "TRANSFER_STATE_FAILED" || got.FailureReason != "precondition_failed.insufficient_funds" {
		t.Fatalf("= %d %s, want 201 with a failed transfer and its reason", rec.Code, rec.Body)
	}
	if again := serve(t, transfers, http.MethodPost, "/transfers", transferJSON); again.Code != http.StatusOK || decodeTransfer(t, again).State != "TRANSFER_STATE_FAILED" {
		t.Errorf("retry = %d %s, want 200 with the same failed transfer", again.Code, again.Body)
	}
}

func TestADepositHasNoFromAccount(t *testing.T) {
	rec := serve(t, newFake(nil), http.MethodPost, "/deposits",
		`{"idempotency_key":"d1","account_id":"a11ce000-0000-4000-8000-000000000001","currency":"GBP","amount":"1000"}`)
	got := decodeTransfer(t, rec)
	if rec.Code != http.StatusAccepted || got.Kind != "TRANSFER_KIND_DEPOSIT" || got.FromAccountID != "" || got.ToAccountID != alice.String() {
		t.Errorf("= %d %+v, want a pending deposit into alice's account, with no from", rec.Code, got)
	}
}

func TestBadRequestsSaveNothing(t *testing.T) {
	tests := []struct{ name, path, body, wantCode string }{
		{"a malformed from", "/transfers", `{"idempotency_key":"t1","from_account_id":"A11CE000-0000-4000-8000-000000000001","to_account_id":"b0b00000-0000-4000-8000-000000000002","currency":"GBP","amount":300}`, "bad_request.from_account_id"},
		{"a malformed to", "/transfers", `{"idempotency_key":"t1","from_account_id":"a11ce000-0000-4000-8000-000000000001","to_account_id":"nope","currency":"GBP","amount":300}`, "bad_request.to_account_id"},
		{"to itself", "/transfers", `{"idempotency_key":"t1","from_account_id":"a11ce000-0000-4000-8000-000000000001","to_account_id":"a11ce000-0000-4000-8000-000000000001","currency":"GBP","amount":300}`, "bad_request.to_account_id"},
		{"an unknown currency", "/transfers", `{"idempotency_key":"t1","from_account_id":"a11ce000-0000-4000-8000-000000000001","to_account_id":"b0b00000-0000-4000-8000-000000000002","currency":"XYZ","amount":300}`, "bad_request.currency"},
		{"a deposit's malformed account", "/deposits", `{"idempotency_key":"d1","account_id":"nope","currency":"GBP","amount":5}`, "bad_request.account_id"},
		{"no amount", "/deposits", `{"idempotency_key":"d1","account_id":"a11ce000-0000-4000-8000-000000000001","currency":"GBP"}`, "bad_request.amount"},
		{"no key", "/deposits", `{"account_id":"a11ce000-0000-4000-8000-000000000001","currency":"GBP","amount":5}`, "bad_request.idempotency_key"},
		{"an unknown field", "/deposits", `{"idempotency_key":"d1","account_id":"a11ce000-0000-4000-8000-000000000001","currency":"GBP","amount":5,"memo":"hi"}`, "bad_request.invalid_json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transfers := newFake(nil)
			rec := serve(t, transfers, http.MethodPost, tt.path, tt.body)
			if rec.Code != http.StatusBadRequest || errorCode(t, rec) != tt.wantCode || len(transfers.byID) != 0 {
				t.Errorf("= %d %s, %d saved; want 400 %s and nothing saved", rec.Code, rec.Body, len(transfers.byID), tt.wantCode)
			}
		})
	}
}

func TestAReusedKeyIsRefused(t *testing.T) {
	transfers := newFake(nil)
	serve(t, transfers, http.MethodPost, "/transfers", transferJSON)
	other := strings.Replace(transferJSON, `"amount":300`, `"amount":301`, 1)
	if rec := serve(t, transfers, http.MethodPost, "/transfers", other); rec.Code != http.StatusPreconditionFailed || errorCode(t, rec) != "precondition_failed.idempotency_key" {
		t.Errorf("= %d %s, want 412 precondition_failed.idempotency_key", rec.Code, rec.Body)
	}
}

func TestGetTransfer(t *testing.T) {
	transfers := newFake(nil)
	id := decodeTransfer(t, serve(t, transfers, http.MethodPost, "/transfers", transferJSON)).ID
	if rec := serve(t, transfers, http.MethodGet, "/transfers/"+id, ""); rec.Code != http.StatusOK || decodeTransfer(t, rec).ID != id {
		t.Errorf("GET = %d %s, want the transfer", rec.Code, rec.Body)
	}
	for _, path := range []string{"/transfers/" + uuid.New().String(), "/transfers/" + strings.ToUpper(id), "/transfers/nope"} {
		if rec := serve(t, transfers, http.MethodGet, path, ""); rec.Code != http.StatusNotFound || errorCode(t, rec) != "not_found.transfer" {
			t.Errorf("GET %s = %d %s, want 404 not_found.transfer", path, rec.Code, rec.Body)
		}
	}
}

// TestATransferIsSavedWithItsRequestsIDs: the handler takes the trace and
// request IDs from the request (kit.Handler put them on its context) and
// hands them to the store.
func TestATransferIsSavedWithItsRequestsIDs(t *testing.T) {
	prev := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{}) // as obs.StartTracing does
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })

	transfers := newFake(nil)
	router := typhon.Router{}
	RegisterRoutes(&router, transfers)
	var ready atomic.Bool
	ready.Store(true)
	h := kit.Handler(slog.New(slog.DiscardHandler), obs.NewRegistry(), &router, &ready, nil)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/transfers", strings.NewReader(transferJSON))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(kit.RequestIDHeader, "req-42")
	req.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	h.ServeHTTP(httptest.NewRecorder(), req)

	saved := transfers.byKey["t1"]
	if saved.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || saved.RequestID != "req-42" {
		t.Errorf("saved trace %q, request %q; want the request's", saved.TraceID, saved.RequestID)
	}
}
