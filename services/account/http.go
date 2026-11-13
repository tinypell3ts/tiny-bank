package account

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"
	"uuid"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/money"
)

// Accounts is what the HTTP API needs from storage. *Store implements it; it's
// an interface so handler tests can use a fake instead of Postgres.
type Accounts interface {
	Create(ctx context.Context, req NewRequest) (Account, error)
	Get(ctx context.Context, id uuid.UUID) (Account, error)
}

// maxBodyBytes caps a request body. An account request is under 1 KB, so this
// is generous, and stops a caller tying up memory with a huge body.
const maxBodyBytes = 16 << 10

// RegisterRoutes adds the account API to mux:
//
//	POST /accounts       open an account
//	GET  /accounts/{id}  look one up
//
// Errors are JSON, {"error": "..."}, with a message that's safe to show the
// caller. Anything unexpected is logged with the request ID and answered with
// a generic 500, never the internal error.
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
func RegisterRoutes(mux *http.ServeMux, accounts Accounts, logger *slog.Logger) {
	h := handler{accounts: accounts, logger: logger}
	mux.HandleFunc("POST /accounts", h.create)
	mux.HandleFunc("GET /accounts/{id}", h.get)
}

type handler struct {
	accounts Accounts
	logger   *slog.Logger
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

func (h handler) create(w http.ResponseWriter, r *http.Request) {
	// Requiring JSON means a browser can't send this cross-site as a "simple"
	// text/plain request without a CORS preflight.
	if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}

	var body createRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields() // a typo like "currancy" is an error, not silently ignored
	err := dec.Decode(&body)
	if err == nil {
		// Anything after the object is an error. Keep the reader's own error,
		// so going over the size limit here is still a 413.
		if extra := dec.Decode(&struct{}{}); !errors.Is(extra, io.EOF) {
			err = errors.Join(errors.New("more than one JSON value"), extra)
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body is too large")
			return
		}
		writeError(w, http.StatusBadRequest, "request body must be a JSON object with customer_id, holder_name and currency")
		return
	}

	// Turn untrusted text into typed values here, at the edge, so everything
	// after this point gets a well-formed UUID and a supported currency.
	customerID, ok := parseID(body.CustomerID)
	if !ok {
		writeError(w, http.StatusBadRequest, "customer_id must be a lower-case UUID with dashes, like c0ffee00-0000-4000-8000-000000000001")
		return
	}
	currency, err := money.ParseCurrency(body.Currency)
	if err != nil {
		writeError(w, http.StatusBadRequest, "currency must be a supported ISO 4217 code, like GBP")
		return
	}

	acc, err := h.accounts.Create(r.Context(), NewRequest{CustomerID: customerID, HolderName: body.HolderName, Currency: currency})
	var invalid *ValidationError
	switch {
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, invalid.Reason)
		return
	case err != nil:
		h.failed(w, r, err)
		return
	}

	w.Header().Set("Location", "/accounts/"+acc.ID.String())
	kit.WriteJSON(w, http.StatusCreated, toJSON(acc))
}

func (h handler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		// No account has an ID that isn't a canonical UUID, so this is the same
		// "not found" as a missing account, not a bad request.
		notFound(w)
		return
	}
	acc, err := h.accounts.Get(r.Context(), id)
	switch {
	case errors.Is(err, ErrNotFound):
		notFound(w)
		return
	case err != nil:
		h.failed(w, r, err)
		return
	}
	kit.WriteJSON(w, http.StatusOK, toJSON(acc))
}

// parseID accepts only the canonical form of a UUID, lower-case with dashes,
// as the API returns it. uuid.Parse also accepts braces, a urn:uuid: prefix,
// no dashes and upper case; allowing those would let one account be reached
// under many URLs, which anything keyed on the path could be fooled by.
func parseID(raw string) (uuid.UUID, bool) {
	id, err := uuid.Parse(raw)
	return id, err == nil && id.String() == raw
}

func notFound(w http.ResponseWriter) { writeError(w, http.StatusNotFound, "account not found") }

// statusClientClosedRequest is nginx's convention for "the caller went away
// first". It's a 4xx, so metrics don't count it as a server error.
const statusClientClosedRequest = 499

// failed answers an unexpected error. If the caller went away (the request's
// own context is done), nobody is listening, so it isn't logged as a failure.
// It checks the request, not the error: a context.Canceled from inside the
// service, with the caller still waiting, is a real failure. Everything else
// is logged with the request ID, so it can be found from the caller's
// X-Request-Id, and answered with a message that reveals nothing.
func (h handler) failed(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		h.logger.InfoContext(r.Context(), "client went away", "request_id", kit.RequestIDFrom(r.Context()))
		w.WriteHeader(statusClientClosedRequest)
		return
	}
	h.logger.ErrorContext(r.Context(), "account request failed",
		"request_id", kit.RequestIDFrom(r.Context()),
		"error", err.Error(),
	)
	writeError(w, http.StatusInternalServerError, "something went wrong")
}

func writeError(w http.ResponseWriter, status int, message string) {
	kit.WriteJSON(w, status, map[string]string{"error": message})
}
