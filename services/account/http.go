package account

import (
	"context"
	"net/http"
	"time"
	"uuid"

	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/money"
)

// Accounts is what the HTTP API needs from storage. *Store implements it; it's
// an interface so handler tests can use a fake instead of Postgres.
type Accounts interface {
	Create(ctx context.Context, req NewRequest) (Account, error)
	Get(ctx context.Context, id uuid.UUID) (Account, error)
}

// RegisterRoutes adds the account API to router:
//
//	POST /accounts      open an account
//	GET  /accounts/:id  look one up
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

// createRequest is the JSON body of POST /accounts.
type createRequest struct {
	CustomerID string `json:"customer_id"`
	HolderName string `json:"holder_name"`
	Currency   string `json:"currency"`
}

// accountJSON is how an account looks in responses.
type accountJSON struct {
	ID         string    `json:"id"`
	CustomerID string    `json:"customer_id"`
	HolderName string    `json:"holder_name"`
	Currency   string    `json:"currency"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"` // RFC 3339, e.g. 2026-11-13T10:00:00Z
}

func toJSON(a Account) accountJSON {
	return accountJSON{
		ID:         a.ID.String(),
		CustomerID: a.CustomerID.String(),
		HolderName: a.HolderName,
		Currency:   a.Currency.Code(),
		Status:     string(a.Status),
		CreatedAt:  a.CreatedAt.UTC(),
	}
}

func (h handler) create(req typhon.Request) typhon.Response {
	var body createRequest
	if err := kit.DecodeJSON(req, &body); err != nil {
		return typhon.Response{Error: err}
	}

	// Turn untrusted text into typed values here, at the edge, so everything
	// after this point gets a well-formed UUID and a supported currency.
	customerID, ok := parseID(body.CustomerID)
	if !ok {
		return typhon.Response{Error: invalid("customer_id",
			"customer_id must be a lower-case UUID with dashes, like c0ffee00-0000-4000-8000-000000000001")}
	}
	currency, err := money.ParseCurrency(body.Currency)
	if err != nil {
		return typhon.Response{Error: invalid("currency", "currency must be a supported ISO 4217 code, like GBP")}
	}

	acc, err := h.accounts.Create(req.Context, NewRequest{CustomerID: customerID, HolderName: body.HolderName, Currency: currency})
	if err != nil {
		return typhon.Response{Error: err}
	}
	rsp := req.ResponseWithCode(toJSON(acc), http.StatusCreated)
	rsp.Header.Set("Location", "/accounts/"+acc.ID.String())
	return rsp
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
	return req.Response(toJSON(acc))
}

// parseID accepts only the canonical form of a UUID, lower-case with dashes,
// as the API returns it. uuid.Parse also accepts braces, a urn:uuid: prefix,
// no dashes and upper case; allowing those would let one account be reached
// under many URLs, which anything keyed on the path could be fooled by.
func parseID(raw string) (uuid.UUID, bool) {
	id, err := uuid.Parse(raw)
	return id, err == nil && id.String() == raw
}
