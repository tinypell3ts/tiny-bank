package transaction

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"uuid"

	"github.com/monzo/terrors"
	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/funding"
	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/money"
	"github.com/tinypell3ts/tiny-bank/pkg/obs"
	ledgerv1 "github.com/tinypell3ts/tiny-bank/proto/ledger/v1"
)

func TestJournalFor(t *testing.T) {
	tr := Transfer{ID: uuid.New(), Kind: KindTransfer, From: alice, To: bob, Amount: gbp(300)}
	j, err := journalFor(tr)
	if err != nil {
		t.Fatal(err)
	}
	if j.GetIdempotencyKey() != tr.ID.String() || j.GetCurrency() != "GBP" ||
		j.GetEntries()[0].GetAccountId() != alice.String() || j.GetEntries()[0].GetAmount() != -300 ||
		j.GetEntries()[1].GetAccountId() != bob.String() || j.GetEntries()[1].GetAmount() != 300 {
		t.Errorf("journal = %v; want £3 from alice to bob, keyed by the transfer's ID", j)
	}

	deposit := Transfer{ID: uuid.New(), Kind: KindDeposit, To: alice, Amount: money.New(500, money.EUR())}
	j, err = journalFor(deposit)
	eurFunding, _ := funding.Account(money.EUR())
	if err != nil || j.GetEntries()[0].GetAccountId() != eurFunding.String() || j.GetEntries()[1].GetAccountId() != alice.String() {
		t.Errorf("deposit journal = %v, %v; want it from the EUR funding account", j, err)
	}
}

func TestOutcomeOf(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		want       outcome
		wantReason string
	}{
		{"posted", nil, posted, ""},
		{"insufficient funds", terrors.PreconditionFailed("insufficient_funds", "x", nil), refusedForGood, "precondition_failed.insufficient_funds"},
		{"an account that isn't open", terrors.PreconditionFailed("account_not_open", "x", nil), refusedForGood, "precondition_failed.account_not_open"},
		{"an account that doesn't exist", terrors.NotFound("account", "x", nil), refusedForGood, "not_found.account"},
		{"a bad journal", terrors.BadRequest("entries", "x", nil), refusedForGood, "bad_request.entries"},
		{"our key reused for another journal", terrors.PreconditionFailed("idempotency_key", "x", nil), keyReused, ""},
		{"the ledger failing", terrors.InternalService("account_service", "x", nil), unsure, ""},
		{"a timeout", terrors.Timeout("deadline", "x", nil), unsure, ""},
		{"no connection", errors.New("connection refused"), unsure, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, reason := outcomeOf(tt.err); got != tt.want || reason != tt.wantReason {
				t.Errorf("outcomeOf = %v, %q; want %v, %q", got, reason, tt.want, tt.wantReason)
			}
		})
	}
}

// TestLedgerServicePostsBinaryWithTheTransfersKey runs LedgerService against
// a stand-in ledger that decodes the request as the real one does.
func TestLedgerServicePostsBinaryWithTheTransfersKey(t *testing.T) {
	journalID := uuid.New()
	var got atomic.Pointer[ledgerv1.PostJournalRequest]
	router := typhon.Router{}
	router.POST("/journals", func(req typhon.Request) typhon.Response {
		var body ledgerv1.PostJournalRequest
		if err := kit.Decode(req, &body); err != nil {
			return typhon.Response{Error: err}
		}
		got.Store(&body)
		return kit.Respond(req, &ledgerv1.Journal{Id: journalID.String()}, http.StatusCreated)
	})
	var ready atomic.Bool
	ready.Store(true)
	srv := httptest.NewServer(kit.Handler(slog.New(slog.DiscardHandler), obs.NewRegistry(), &router, &ready, nil))
	t.Cleanup(srv.Close)

	tr := Transfer{ID: uuid.New(), Kind: KindTransfer, From: alice, To: bob, Amount: gbp(300)}
	id, err := LedgerService{BaseURL: srv.URL}.Post(t.Context(), tr)
	if err != nil || id != journalID {
		t.Fatalf("Post = %s, %v; want journal %s", id, err, journalID)
	}
	if body := got.Load(); body == nil || body.GetIdempotencyKey() != tr.ID.String() {
		t.Errorf("the ledger got %v, want the transfer's ID as its key", body)
	}
}
