package ledger

import (
	"context"
	"fmt"
	"net/http"
	"uuid"

	"github.com/monzo/typhon"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/money"
	ledgerv1 "github.com/tinypell3ts/tiny-bank/proto/ledger/v1"
)

// Ledger is what the HTTP API needs. *Service implements it; it's an
// interface so handler tests can use a fake instead of Postgres and the
// account service.
type Ledger interface {
	Post(ctx context.Context, req NewJournal) (Journal, bool, error)
	Journal(ctx context.Context, id uuid.UUID) (Journal, error)
	Balance(ctx context.Context, id uuid.UUID) (Balance, error)
	// CountRefusal counts a post the handler refused before it reached Post
	// (a body that isn't valid JSON, an unsupported currency, a malformed
	// account ID), so the metrics see every refusal, not only Post's.
	// currency is the zero Currency if the request didn't get that far.
	CountRefusal(currency money.Currency, err error)
}

// RegisterRoutes adds the ledger API to router. Its contract, the request and
// response messages, is proto/ledger/v1/ledger.proto:
//
//	POST /journals              PostJournalRequest -> Journal: move money
//	GET  /journals/:id          Journal: look one up
//	GET  /accounts/:id/balance  Balance: how much an account holds
//
// Handlers return terrors. kit.Handler turns them into responses (a terror's
// code and message reach the caller), and logs and hides anything internal.
//
// There is no authentication yet: any caller can move money between any open
// accounts (Service.Post checks each one with the account service). The
// service must stay internal (ClusterIP, no Ingress).
// TODO(phase-12): only the transaction service may post.
func RegisterRoutes(router *typhon.Router, ledger Ledger) {
	h := handler{ledger: ledger}
	router.POST("/journals", h.post)
	router.GET("/journals/:id", h.journal)
	router.GET("/accounts/:id/balance", h.balance)
}

type handler struct {
	ledger Ledger
}

func (h handler) post(req typhon.Request) typhon.Response {
	// refuse answers a request turned away before Post, counting it as Post
	// counts its own refusals.
	refuse := func(currency money.Currency, err error) typhon.Response {
		h.ledger.CountRefusal(currency, err)
		return typhon.Response{Error: err}
	}

	var body ledgerv1.PostJournalRequest
	if err := kit.Decode(req, &body); err != nil {
		return refuse(money.Currency{}, err)
	}

	// Turn untrusted text into typed values here, at the edge. Validate then
	// checks the rules that need the whole journal.
	currency, err := money.ParseCurrency(body.GetCurrency())
	if err != nil {
		return refuse(money.Currency{}, invalid("currency", "currency must be a supported ISO 4217 code, like GBP"))
	}
	entries := make([]Entry, len(body.GetEntries()))
	for i, e := range body.GetEntries() {
		id, ok := kit.ParseUUID(e.GetAccountId())
		if !ok {
			return refuse(currency, invalid("entries",
				fmt.Sprintf("entry %d's account_id must be a lower-case UUID with dashes", i+1)))
		}
		entries[i] = Entry{AccountID: id, Amount: money.New(e.GetAmount(), currency)}
	}

	j, created, err := h.ledger.Post(req.Context, NewJournal{IdempotencyKey: body.GetIdempotencyKey(), Entries: entries})
	if err != nil {
		return typhon.Response{Error: err}
	}
	if !created {
		// A retry: the same answer as the first time, but nothing new was
		// created, so 200 rather than 201.
		return kit.Respond(req, journalToProto(j), http.StatusOK)
	}
	return kit.RespondCreated(req, journalToProto(j), "/journals/"+j.ID.String())
}

func (h handler) journal(req typhon.Request) typhon.Response {
	id, ok := kit.ParseUUID(typhon.RouterForRequest(req).Params(req)["id"])
	if !ok {
		// No journal has an ID that isn't a canonical UUID.
		return typhon.Response{Error: errJournalNotFound()}
	}
	j, err := h.ledger.Journal(req.Context, id)
	if err != nil {
		return typhon.Response{Error: err}
	}
	return kit.Respond(req, journalToProto(j), http.StatusOK)
}

func (h handler) balance(req typhon.Request) typhon.Response {
	id, ok := kit.ParseUUID(typhon.RouterForRequest(req).Params(req)["id"])
	if !ok {
		return typhon.Response{Error: errAccountNotFound()}
	}
	b, err := h.ledger.Balance(req.Context, id)
	if err != nil {
		return typhon.Response{Error: err}
	}
	return kit.Respond(req, &ledgerv1.Balance{
		AccountId: b.AccountID.String(),
		Currency:  b.Amount.Currency().Code(),
		Amount:    b.Amount.Amount(),
	}, http.StatusOK)
}

// journalToProto is how a journal looks in responses (proto/ledger/v1).
func journalToProto(j Journal) *ledgerv1.Journal {
	entries := make([]*ledgerv1.Entry, len(j.Entries))
	for i, e := range j.Entries {
		entries[i] = &ledgerv1.Entry{AccountId: e.AccountID.String(), Amount: e.Amount.Amount()}
	}
	return &ledgerv1.Journal{
		Id:             j.ID.String(),
		IdempotencyKey: j.IdempotencyKey,
		Currency:       j.Currency.Code(),
		Entries:        entries,
		CreatedAt:      timestamppb.New(j.CreatedAt),
	}
}
