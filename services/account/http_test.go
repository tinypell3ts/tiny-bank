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

	"github.com/tinypell3ts/tiny-bank/pkg/money"
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
		return Account{}, ErrNotFound
	}
	return acc, nil
}

// serve sends one request to the API backed by accounts, and returns the
// response and anything logged.
func serve(t *testing.T, accounts Accounts, method, path, body string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	var logs bytes.Buffer
	mux := http.NewServeMux()
	RegisterRoutes(mux, accounts, slog.New(slog.NewJSONHandler(&logs, nil)))
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec, logs.String()
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body isn't JSON: %v: %s", err, rec.Body)
	}
	return body["error"]
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
		name       string
		body       string
		wantStatus int
		wantError  string
	}{
		{"not JSON", `customer_id=1`, 400, "must be a JSON object"},
		{"empty body", ``, 400, "must be a JSON object"},
		{"unknown field", `{` + custJSON + `,"holder_name":"Ada","currancy":"GBP"}`, 400, "must be a JSON object"},
		{"trailing data", `{` + custJSON + `,"holder_name":"Ada","currency":"GBP"}{"currency":"EUR"}`, 400, "must be a JSON object"},
		{"trailing junk", `{` + custJSON + `,"holder_name":"Ada","currency":"GBP"} x`, 400, "must be a JSON object"},
		{"customer ID upper case", `{"customer_id":"C0FFEE00-0000-4000-8000-000000000001","holder_name":"Ada","currency":"GBP"}`, 400, "customer_id must be a lower-case UUID"},
		{"customer ID without dashes", `{"customer_id":"c0ffee0000004000800000000000001","holder_name":"Ada","currency":"GBP"}`, 400, "customer_id must be a lower-case UUID"},
		{"customer not a UUID", `{"customer_id":"ada","holder_name":"Ada","currency":"GBP"}`, 400, "customer_id must be a lower-case UUID"},
		{"missing customer", `{"holder_name":"Ada","currency":"GBP"}`, 400, "customer_id must be a lower-case UUID"},
		{"unsupported currency", `{` + custJSON + `,"holder_name":"Ada","currency":"XYZ"}`, 400, "currency must be a supported"},
		{"lower-case currency", `{` + custJSON + `,"holder_name":"Ada","currency":"gbp"}`, 400, "currency must be a supported"},
		{"missing name", `{` + custJSON + `,"currency":"GBP"}`, 400, "holder name is required"},
		{"control character in name", `{` + custJSON + `,"holder_name":"Ada\nLovelace","currency":"GBP"}`, 400, "control characters"},
		{"too large", `{` + custJSON + `,"holder_name":"` + strings.Repeat("a", maxBodyBytes) + `","currency":"GBP"}`, 413, "too large"},
		{"too large after a valid object", `{` + custJSON + `,"holder_name":"Ada","currency":"GBP"}` + strings.Repeat(" ", maxBodyBytes), 413, "too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, _ := serve(t, newFake(), http.MethodPost, "/accounts", tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if got := errorOf(t, rec); !strings.Contains(got, tt.wantError) {
				t.Errorf("error = %q, want it to contain %q", got, tt.wantError)
			}
			// The message is meant for the caller: never Go's internal wording.
			if got := errorOf(t, rec); strings.Contains(got, "create account:") || strings.Contains(got, "json:") {
				t.Errorf("error leaks internals: %q", got)
			}
		})
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

func TestCreateAccountNeedsJSONContentType(t *testing.T) {
	body := `{` + custJSON + `,"holder_name":"Ada","currency":"GBP"}`
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
		mux := http.NewServeMux()
		RegisterRoutes(mux, newFake(), slog.New(slog.DiscardHandler))
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/accounts", strings.NewReader(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type %q: status = %d, want 415", ct, rec.Code)
		}
	}
	// Parameters such as charset are fine.
	mux := http.NewServeMux()
	RegisterRoutes(mux, newFake(), slog.New(slog.DiscardHandler))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/accounts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Errorf("application/json; charset=utf-8: status = %d, want 201", rec.Code)
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
			var logs bytes.Buffer
			mux := http.NewServeMux()
			RegisterRoutes(mux, ctxAccounts{}, slog.New(slog.NewJSONHandler(&logs, nil)))

			// The caller hangs up mid-request: its context is cancelled.
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			r := httptest.NewRequestWithContext(ctx, req.method, req.path, strings.NewReader(req.body))
			r.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, r)

			if rec.Code != statusClientClosedRequest {
				t.Errorf("status = %d, want %d", rec.Code, statusClientClosedRequest)
			}
			if strings.Contains(logs.String(), `"level":"ERROR"`) || !strings.Contains(logs.String(), "client went away") {
				t.Errorf("want an info line, not an error, got: %s", logs.String())
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

func TestGetAccountNotFound(t *testing.T) {
	// A malformed ID can't match any account, so it's a 404 like a missing one,
	// never a 400 or a 500 (carried over from the store's old string-ID tests).
	for _, id := range []string{
		uuid.New().String(),     // well-formed, but no such account
		"not-a-uuid",            // malformed
		"%27%20OR%201%3D1%20--", // "' OR 1=1 --", URL-encoded
	} {
		t.Run(id, func(t *testing.T) {
			rec, _ := serve(t, newFake(), http.MethodGet, "/accounts/"+id, "")
			if rec.Code != http.StatusNotFound || errorOf(t, rec) != "account not found" {
				t.Errorf("status = %d, body %s; want 404 account not found", rec.Code, rec.Body)
			}
		})
	}
}

func TestInternalErrorsAreLoggedNotShown(t *testing.T) {
	broken := &fakeAccounts{err: errors.New("connection reset: password=s3cret")}
	for _, req := range []struct{ method, path, body string }{
		{http.MethodPost, "/accounts", `{` + custJSON + `,"holder_name":"Ada","currency":"GBP"}`},
		{http.MethodGet, "/accounts/" + uuid.New().String(), ""},
	} {
		rec, logs := serve(t, broken, req.method, req.path, req.body)
		if rec.Code != http.StatusInternalServerError || errorOf(t, rec) != "something went wrong" {
			t.Errorf("%s %s: status %d, body %s; want 500 something went wrong", req.method, req.path, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "s3cret") || strings.Contains(rec.Body.String(), "connection") {
			t.Errorf("%s %s: response leaks the internal error: %s", req.method, req.path, rec.Body)
		}
		if !strings.Contains(logs, "connection reset") {
			t.Errorf("%s %s: internal error should be logged, got: %s", req.method, req.path, logs)
		}
	}
}

func TestWrongMethod(t *testing.T) {
	rec, _ := serve(t, newFake(), http.MethodDelete, "/accounts/"+uuid.New().String(), "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}
