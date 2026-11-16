package account

import (
	"context"
	"net/http"
	"uuid"

	"github.com/monzo/typhon"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/money"
	accountv1 "github.com/tinypell3ts/tiny-bank/proto/account/v1"
)

// Accounts is what the HTTP API needs from storage. *Store implements it; it's
// an interface so handler tests can use a fake instead of Postgres.
type Accounts interface {
	Create(ctx context.Context, req NewRequest) (Account, error)
	Get(ctx context.Context, id uuid.UUID) (Account, error)
}

// RegisterRoutes adds the account API to router. Its contract, the request
// and response messages, is proto/account/v1/account.proto:
//
//	POST /accounts      CreateAccountRequest -> Account: open an account
//	GET  /accounts/:id  Account: look one up
//
// Handlers return terrors. kit.Handler turns them into responses (a terror's
// code and message reach the caller), and logs and hides anything internal.
//
// This is an internal API, called by other services (and, later, staff
// tools), so the caller names the customer with customer_id. Customers won't
// call it directly. Onboarding (Phase 11.5) creates the customer first, then
// opens their first account here. The customer-facing endpoint at the edge
// gateway (Phase 12) takes the customer ID from the signed-in caller's token,
// never from the request body. See ADR-0006.
//
// There is no authentication yet: any caller can open an account for any
// customer and read any account by ID. The service must stay internal
// (ClusterIP, no Ingress) until the edge gateway checks who the caller is in
// Phase 12. TODO(phase-12): check the caller is the customer they act for.
func RegisterRoutes(router *typhon.Router, accounts Accounts) {
	h := handler{accounts: accounts}
	router.POST("/accounts", h.create)
	router.GET("/accounts/:id", h.get)
}

type handler struct {
	accounts Accounts
}

// statusToProto is how each Status appears in the contract. A new Status needs
// a value here and in AccountStatus. One that's missing reaches callers as
// ACCOUNT_STATUS_UNSPECIFIED, which the contract tells them to treat as
// "not open", so a mistake fails safe.
var statusToProto = map[Status]accountv1.AccountStatus{
	StatusOpen: accountv1.AccountStatus_ACCOUNT_STATUS_OPEN,
}

// toProto is how an account looks in responses (proto/account/v1).
func toProto(a Account) *accountv1.Account {
	return &accountv1.Account{
		Id:         a.ID.String(),
		CustomerId: a.CustomerID.String(),
		HolderName: a.HolderName,
		Currency:   a.Currency.Code(),
		Status:     statusToProto[a.Status],
		CreatedAt:  timestamppb.New(a.CreatedAt),
	}
}

func (h handler) create(req typhon.Request) typhon.Response {
	var body accountv1.CreateAccountRequest
	if err := kit.Decode(req, &body); err != nil {
		return typhon.Response{Error: err}
	}

	// Turn untrusted text into typed values here, at the edge, so everything
	// after this point gets a well-formed UUID and a supported currency.
	customerID, ok := parseID(body.GetCustomerId())
	if !ok {
		return typhon.Response{Error: invalid("customer_id",
			"customer_id must be a lower-case UUID with dashes, like c0ffee00-0000-4000-8000-000000000001")}
	}
	currency, err := money.ParseCurrency(body.GetCurrency())
	if err != nil {
		return typhon.Response{Error: invalid("currency", "currency must be a supported ISO 4217 code, like GBP")}
	}

	acc, err := h.accounts.Create(req.Context, NewRequest{CustomerID: customerID, HolderName: body.GetHolderName(), Currency: currency})
	if err != nil {
		return typhon.Response{Error: err}
	}
	return kit.RespondCreated(req, toProto(acc), "/accounts/"+acc.ID.String())
}

func (h handler) get(req typhon.Request) typhon.Response {
	id, ok := parseID(typhon.RouterForRequest(req).Params(req)["id"])
	if !ok {
		// No account has an ID that isn't a canonical UUID, so this is the same
		// "not found" as a missing account, not a bad request.
		return typhon.Response{Error: errNotFound()}
	}
	acc, err := h.accounts.Get(req.Context, id)
	if err != nil {
		return typhon.Response{Error: err}
	}
	return kit.Respond(req, toProto(acc), http.StatusOK)
}

// parseID accepts only the canonical form of a UUID, lower-case with dashes,
// as the API returns it. uuid.Parse also accepts braces, a urn:uuid: prefix,
// no dashes and upper case; allowing those would let one account be reached
// under many URLs, which anything keyed on the path could be fooled by.
func parseID(raw string) (uuid.UUID, bool) {
	id, err := uuid.Parse(raw)
	return id, err == nil && id.String() == raw
}
