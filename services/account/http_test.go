package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/monzo/typhon"

	"github.com/tinypell3ts/tiny-bank/pkg/kit"
	"github.com/tinypell3ts/tiny-bank/pkg/money"
	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// fakeAccounts keeps accounts in memory. Create applies the same rules as the
// real Store (it calls Validate). Set err to make every call fail.
type fakeAccounts struct {
	byID map[uuid.UUID]Account
	err  error
}

func newFake() *fakeAccounts { return &fakeAccounts{byID: map[uuid.UUID]Account{}} }

func (f *fakeAccounts) Create(_ context.Context, req NewRequest) (Account, error) {
	if f.err != nil {
		return Account{}, f.err
	}
	req, err := req.Validate()
	if err != nil {
		return Account{}, err
	}
	acc := Account{
		ID: uuid.New(), CustomerID: req.CustomerID, HolderName: req.HolderName, Currency: req.Currency,
		Status: StatusOpen, CreatedAt: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC),
	}
	f.byID[acc.ID] = acc
	return acc, nil
}

func (f *fakeAccounts) Get(_ context.Context, id uuid.UUID) (Account, error) {
	if f.err != nil {
		return Account{}, f.err
	}
	acc, ok := f.byID[id]
	if !ok {
		return Account{}, errNotFound()
	}
	return acc, nil
}

// apiHandler is the account API as it runs in production: its routes behind
// kit.Handler's filters. It returns the handler and what it logs.
func apiHandler(accounts Accounts) (http.Handler, *bytes.Buffer) {
	var logs bytes.Buffer
	router := typhon.Router{}
	RegisterRoutes(&router, accounts)
	return kit.Handler(slog.New(slog.NewJSONHandler(&logs, nil)), obs.NewRegistry(), &router), &logs
}

// serve sends one request (JSON for POSTs) and returns the response and logs.
func serve(t *testing.T, accounts Accounts, method, path, body string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	h, logs := apiHandler(accounts)
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, logs.String()
}

// terror is the error body Typhon sends: the terror's code and message.
type terror struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) terror {
	t.Helper()
	var e terror
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body isn't JSON: %v: %s", err, rec.Body)
	}
	return e
}

const custJSON = `"customer_id":"c0ffee00-0000-4000-8000-000000000001"`

func TestCreateAccount(t *testing.T) {
	rec, _ := serve(t, newFake(), http.MethodPost, "/accounts",
		`{`+custJSON+`,"holder_name":"  Ada Lovelace ","currency":"GBP"}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}
	var got accountJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.CustomerID != customer.String() || got.HolderName != "Ada Lovelace" || got.Currency != "GBP" || got.Status != "open" {
		t.Errorf("body = %+v", got)
	}
	if rec.Header().Get("Location") != "/accounts/"+got.ID {
		t.Errorf("Location = %q, want /accounts/%s", rec.Header().Get("Location"), got.ID)
	}
}

func TestCreateAccountRejectsBadRequests(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode string // terror code
		wantMsg  string // substring of the message
	}{
		{"not JSON", `customer_id=1`, "bad_request.invalid_json", "JSON object"},
		{"null", `null`, "bad_request.invalid_json", "JSON object"},
		{"empty body", ``, "bad_request.invalid_json", "JSON object"},
		{"unknown field", `{` + custJSON + `,"holder_name":"Ada","currancy":"GBP"}`, "bad_request.invalid_json", `unknown field "currancy"`},
		{"trailing data", `{` + custJSON + `,"holder_name":"Ada","currency":"GBP"}{"currency":"EUR"}`, "bad_request.invalid_json", "single JSON object"},
		{"trailing junk", `{` + custJSON + `,"holder_name":"Ada","currency":"GBP"} x`, "bad_request.invalid_json", "single JSON object"},
		{"customer ID upper case", `{"customer_id":"C0FFEE00-0000-4000-8000-000000000001","holder_name":"Ada","currency":"GBP"}`, "bad_request.customer_id", "lower-case UUID"},
		{"customer not a UUID", `{"customer_id":"ada","holder_name":"Ada","currency":"GBP"}`, "bad_request.customer_id", "lower-case UUID"},
		{"missing customer", `{"holder_name":"Ada","currency":"GBP"}`, "bad_request.customer_id", "lower-case UUID"},
		{"unsupported currency", `{` + custJSON + `,"holder_name":"Ada","currency":"XYZ"}`, "bad_request.currency", "supported ISO 4217"},
		{"lower-case currency", `{` + custJSON + `,"holder_name":"Ada","currency":"gbp"}`, "bad_request.currency", "supported ISO 4217"},
		{"missing name", `{` + custJSON + `,"currency":"GBP"}`, "bad_request.holder_name", "holder name is required"},
		{"control character in name", `{` + custJSON + `,"holder_name":"Ada\nLovelace","currency":"GBP"}`, "bad_request.holder_name", "control characters"},
		{"too large", `{` + custJSON + `,"holder_name":"` + strings.Repeat("a", kit.MaxBodyBytes) + `","currency":"GBP"}`, "bad_request.too_large", "too large"},
		{"too large after a valid object", `{` + custJSON + `,"holder_name":"Ada","currency":"GBP"}` + strings.Repeat(" ", kit.MaxBodyBytes), "bad_request.too_large", "too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, _ := serve(t, newFake(), http.MethodPost, "/accounts", tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
			}
			got := errorOf(t, rec)
			if got.Code != tt.wantCode || !strings.Contains(got.Message, tt.wantMsg) {
				t.Errorf("error = %+v, want code %q and a message containing %q", got, tt.wantCode, tt.wantMsg)
			}
			// The message is meant for the caller: never Go's internal wording.
			if strings.Contains(rec.Body.String(), "create account:") || strings.Contains(rec.Body.String(), "json:") {
				t.Errorf("error leaks internals: %s", rec.Body)
			}
		})
	}
}

func TestCreateAccountNeedsJSONContentType(t *testing.T) {
	body := `{` + custJSON + `,"holder_name":"Ada","currency":"GBP"}`
	for ct, wantStatus := range map[string]int{
		"":                                  http.StatusBadRequest,
		"text/plain":                        http.StatusBadRequest,
		"application/x-www-form-urlencoded": http.StatusBadRequest,
		"application/json; charset=utf-8":   http.StatusCreated, // parameters are fine
	} {
		h, _ := apiHandler(newFake())
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/accounts", strings.NewReader(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != wantStatus {
			t.Errorf("Content-Type %q: status = %d, want %d", ct, rec.Code, wantStatus)
		}
		if wantStatus == http.StatusBadRequest && errorOf(t, rec).Code != "bad_request.content_type" {
			t.Errorf("Content-Type %q: error = %s", ct, rec.Body)
		}
	}
}

func TestGetAccount(t *testing.T) {
	fake := newFake()
	acc, err := fake.Create(t.Context(), NewRequest{CustomerID: customer, HolderName: "Ada", Currency: money.EUR()})
	if err != nil {
		t.Fatal(err)
	}

	rec, _ := serve(t, fake, http.MethodGet, "/accounts/"+acc.ID.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	want := `{"id":"` + acc.ID.String() + `","customer_id":"` + customer.String() +
		`","holder_name":"Ada","currency":"EUR","status":"open","created_at":"2026-11-13T10:00:00Z"}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Errorf("body =\n%s\nwant\n%s", got, want)
	}
}

func TestGetAccountOnlyAtItsCanonicalURL(t *testing.T) {
	fake := newFake()
	acc, err := fake.Create(t.Context(), NewRequest{CustomerID: customer, HolderName: "Ada", Currency: money.GBP()})
	if err != nil {
		t.Fatal(err)
	}
	canonical := acc.ID.String()
	for _, alias := range []string{
		strings.ToUpper(canonical),
		strings.ReplaceAll(canonical, "-", ""),
		"urn:uuid:" + canonical,
		"%7B" + canonical + "%7D", // {braces}
	} {
		if rec, _ := serve(t, fake, http.MethodGet, "/accounts/"+alias, ""); rec.Code != http.StatusNotFound {
			t.Errorf("GET /accounts/%s = %d, want 404: only the canonical form is the account's URL", alias, rec.Code)
		}
	}
	if rec, _ := serve(t, fake, http.MethodGet, "/accounts/"+canonical, ""); rec.Code != http.StatusOK {
		t.Errorf("GET canonical = %d, want 200", rec.Code)
	}
}

func TestGetAccountNotFound(t *testing.T) {
	// A malformed ID can't match any account, so it's a 404 like a missing one,
	// never a 400 or a 500.
	for _, id := range []string{
		uuid.New().String(),     // well-formed, but no such account
		"not-a-uuid",            // malformed
		"%27%20OR%201%3D1%20--", // "' OR 1=1 --", URL-encoded
	} {
		t.Run(id, func(t *testing.T) {
			rec, _ := serve(t, newFake(), http.MethodGet, "/accounts/"+id, "")
			if got := errorOf(t, rec); rec.Code != http.StatusNotFound || got.Code != "not_found.account" || got.Message != "account not found" {
				t.Errorf("status = %d, error %+v; want 404 not_found.account", rec.Code, got)
			}
		})
	}
}

func TestInternalErrorsAreLoggedNotShown(t *testing.T) {
	broken := &fakeAccounts{err: errors.New("connection reset by 10.0.0.7")}
	for _, req := range []struct{ method, path, body string }{
		{http.MethodPost, "/accounts", `{` + custJSON + `,"holder_name":"Ada","currency":"GBP"}`},
		{http.MethodGet, "/accounts/" + uuid.New().String(), ""},
	} {
		rec, logs := serve(t, broken, req.method, req.path, req.body)
		if rec.Code != http.StatusInternalServerError || errorOf(t, rec).Code != "internal_service" {
			t.Errorf("%s %s: status %d, body %s; want 500 internal_service", req.method, req.path, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "10.0.0.7") || strings.Contains(rec.Body.String(), "connection") {
			t.Errorf("%s %s: response leaks the internal error: %s", req.method, req.path, rec.Body)
		}
		if !strings.Contains(logs, "connection reset by 10.0.0.7") {
			t.Errorf("%s %s: internal error should be logged, got: %s", req.method, req.path, logs)
		}
	}
}

// ctxAccounts answers like a store whose query was cut short: it returns the
// request context's error once that context is done.
type ctxAccounts struct{}

func (ctxAccounts) Create(ctx context.Context, _ NewRequest) (Account, error) {
	<-ctx.Done()
	return Account{}, fmt.Errorf("create account: %w", ctx.Err())
}

func (ctxAccounts) Get(ctx context.Context, _ uuid.UUID) (Account, error) {
	<-ctx.Done()
	return Account{}, fmt.Errorf("get account: %w", ctx.Err())
}

func TestClientGoneIsNotAServerError(t *testing.T) {
	for _, req := range []struct{ method, path, body string }{
		{http.MethodPost, "/accounts", `{` + custJSON + `,"holder_name":"Ada","currency":"GBP"}`},
		{http.MethodGet, "/accounts/" + uuid.New().String(), ""},
	} {
		t.Run(req.method, func(t *testing.T) {
			h, logs := apiHandler(ctxAccounts{})
			// The caller hangs up mid-request: its context is cancelled.
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			r := httptest.NewRequestWithContext(ctx, req.method, req.path, strings.NewReader(req.body))
			r.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)

			if rec.Code != 499 {
				t.Errorf("status = %d, want 499", rec.Code)
			}
			if strings.Contains(logs.String(), `"level":"ERROR"`) {
				t.Errorf("a cancelled request shouldn't log an error:\n%s", logs)
			}
		})
	}
}

func TestCanceledInsideTheServiceIsAServerError(t *testing.T) {
	// The caller is still waiting, but something inside returned
	// context.Canceled. That's a real failure, not a disconnect.
	inside := &fakeAccounts{err: fmt.Errorf("get account: %w", context.Canceled)}
	rec, logs := serve(t, inside, http.MethodGet, "/accounts/"+uuid.New().String(), "")
	if rec.Code != http.StatusInternalServerError || !strings.Contains(logs, `"level":"ERROR"`) {
		t.Errorf("status = %d, logs %s; want 500 and an error log", rec.Code, logs)
	}
}

func TestWrongMethod(t *testing.T) {
	// Typhon's router has no route for DELETE, so it's a 404 (not 405).
	rec, _ := serve(t, newFake(), http.MethodDelete, "/accounts/"+uuid.New().String(), "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
