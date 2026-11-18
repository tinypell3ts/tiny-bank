package transaction

import (
	"context"
	"net/http"
	"uuid"

	"github.com/monzo/typhon"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/money"
	transactionv1 "github.com/tinypell3ts/tiny-bank/proto/transaction/v1"
)

// Transfers is what the HTTP API needs. *Service implements it; it's an
// interface so handler tests can use a fake instead of Postgres and the
// ledger.
type Transfers interface {
	Create(ctx context.Context, req NewTransfer) (Transfer, bool, error)
	Get(ctx context.Context, id uuid.UUID) (Transfer, error)
}

// RegisterRoutes adds the transaction API to router. Its contract is
// proto/transaction/v1/transaction.proto:
//
//	POST /transfers      CreateTransferRequest -> Transfer
//	POST /deposits       CreateDepositRequest  -> Transfer
//	GET  /transfers/:id  Transfer
//
// There is no authentication yet: any caller can move money between any
// accounts. The service must stay internal (ClusterIP, no Ingress) until the
// edge gateway checks who the caller is (Phase 12).
func RegisterRoutes(router *typhon.Router, transfers Transfers) {
	h := handler{transfers: transfers}
	router.POST("/transfers", h.createTransfer)
	router.POST("/deposits", h.createDeposit)
	router.GET("/transfers/:id", h.get)
}

type handler struct {
	transfers Transfers
}

func (h handler) createTransfer(req typhon.Request) typhon.Response {
	var body transactionv1.CreateTransferRequest
	if err := kit.Decode(req, &body); err != nil {
		return typhon.Response{Error: err}
	}
	// Turn untrusted text into typed values here, at the edge. Validate then
	// checks the rules that need them all together.
	from, ok := kit.ParseUUID(body.GetFromAccountId())
	if !ok {
		return typhon.Response{Error: invalid("from_account_id", "from_account_id must be a lower-case UUID with dashes")}
	}
	to, ok := kit.ParseUUID(body.GetToAccountId())
	if !ok {
		return typhon.Response{Error: invalid("to_account_id", "to_account_id must be a lower-case UUID with dashes")}
	}
	amount, err := amountOf(body.GetCurrency(), body.GetAmount())
	if err != nil {
		return typhon.Response{Error: err}
	}
	return h.create(req, NewTransfer{IdempotencyKey: body.GetIdempotencyKey(), Kind: KindTransfer, From: from, To: to, Amount: amount})
}

func (h handler) createDeposit(req typhon.Request) typhon.Response {
	var body transactionv1.CreateDepositRequest
	if err := kit.Decode(req, &body); err != nil {
		return typhon.Response{Error: err}
	}
	to, ok := kit.ParseUUID(body.GetAccountId())
	if !ok {
		return typhon.Response{Error: invalid("account_id", "account_id must be a lower-case UUID with dashes")}
	}
	amount, err := amountOf(body.GetCurrency(), body.GetAmount())
	if err != nil {
		return typhon.Response{Error: err}
	}
	return h.create(req, NewTransfer{IdempotencyKey: body.GetIdempotencyKey(), Kind: KindDeposit, To: to, Amount: amount})
}

func amountOf(currency string, minor int64) (money.Money, error) {
	c, err := money.ParseCurrency(currency)
	if err != nil {
		return money.Money{}, invalid("currency", "currency must be a supported ISO 4217 code, like GBP")
	}
	return money.New(minor, c), nil
}

// create saves nt, with the IDs of the request asking for it (see
// NewTransfer), and answers with the transfer.
func (h handler) create(req typhon.Request, nt NewTransfer) typhon.Response {
	nt.TraceID, nt.RequestID = kit.TraceIDFrom(req.Context), kit.RequestIDFrom(req.Context)
	t, created, err := h.transfers.Create(req.Context, nt)
	if err != nil {
		return typhon.Response{Error: err}
	}
	return answer(req, t, created)
}

// answer is how every saved transfer is answered, so the rules live in one
// place: 202 while it's pending (ask again at the Location), 201 if it was
// created and finished in this request, and 200 for a retry that finds it
// already finished. A failed transfer is still a transfer: its state and
// failure_reason say what happened, so every retry with the same key gets
// an answer of the same shape.
func answer(req typhon.Request, t Transfer, created bool) typhon.Response {
	location := "/transfers/" + t.ID.String()
	switch {
	case t.State == StatePending:
		return kit.RespondAt(req, transferToProto(t), http.StatusAccepted, location)
	case created:
		return kit.RespondCreated(req, transferToProto(t), location)
	default:
		return kit.Respond(req, transferToProto(t), http.StatusOK)
	}
}

func (h handler) get(req typhon.Request) typhon.Response {
	id, ok := kit.ParseUUID(typhon.RouterForRequest(req).Params(req)["id"])
	if !ok {
		return typhon.Response{Error: errNotFound()} // no transfer has an ID that isn't a canonical UUID
	}
	t, err := h.transfers.Get(req.Context, id)
	if err != nil {
		return typhon.Response{Error: err}
	}
	return kit.Respond(req, transferToProto(t), http.StatusOK)
}

var (
	kindToProto = map[Kind]transactionv1.TransferKind{
		KindTransfer: transactionv1.TransferKind_TRANSFER_KIND_TRANSFER,
		KindDeposit:  transactionv1.TransferKind_TRANSFER_KIND_DEPOSIT,
	}
	// A State missing here reaches callers as TRANSFER_STATE_UNSPECIFIED,
	// which the contract tells them to treat as "not finished": a mistake
	// fails safe.
	stateToProto = map[State]transactionv1.TransferState{
		StatePending:   transactionv1.TransferState_TRANSFER_STATE_PENDING,
		StateCompleted: transactionv1.TransferState_TRANSFER_STATE_COMPLETED,
		StateFailed:    transactionv1.TransferState_TRANSFER_STATE_FAILED,
	}
)

// transferToProto is how a transfer looks in responses (proto/transaction/v1).
func transferToProto(t Transfer) *transactionv1.Transfer {
	p := &transactionv1.Transfer{
		Id:             t.ID.String(),
		IdempotencyKey: t.IdempotencyKey,
		Kind:           kindToProto[t.Kind],
		ToAccountId:    t.To.String(),
		Currency:       t.Amount.Currency().Code(),
		Amount:         t.Amount.Amount(),
		State:          stateToProto[t.State],
		FailureReason:  t.FailureReason,
		CreatedAt:      timestamppb.New(t.CreatedAt),
		UpdatedAt:      timestamppb.New(t.UpdatedAt),
	}
	if t.From != uuid.Nil() {
		p.FromAccountId = t.From.String()
	}
	if t.JournalID != uuid.Nil() {
		p.JournalId = t.JournalID.String()
	}
	return p
}
