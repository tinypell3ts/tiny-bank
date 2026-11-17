package kit

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/monzo/typhon"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/sourcecontextpb"
	"google.golang.org/protobuf/types/known/typepb"

	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// respond runs h behind kit.Handler, as a real service would.
func respond(t *testing.T, h typhon.Service) *httptest.ResponseRecorder {
	t.Helper()
	router := typhon.Router{}
	router.GET("/thing", h)
	handler := Handler(slog.New(slog.DiscardHandler), obs.NewRegistry(), &router, readyNow(), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/thing", nil))
	return rec
}

func TestRespondUsesProtoFieldNames(t *testing.T) {
	thing := &typepb.Type{Name: "Ada", SourceContext: &sourcecontextpb.SourceContext{FileName: "a.proto"}}
	rec := respond(t, func(req typhon.Request) typhon.Response { return Respond(req, thing, http.StatusOK) })

	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, Content-Type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	// protojson varies its spacing on purpose, so compare the parsed JSON.
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body isn't JSON: %v: %s", err, rec.Body)
	}
	want := map[string]any{"name": "Ada", "source_context": map[string]any{"file_name": "a.proto"}}
	if !jsonEqual(got, want) {
		t.Errorf("body = %v, want %v (proto field names, empty fields left out)", got, want)
	}
}

func jsonEqual(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}

func TestRespondCreated(t *testing.T) {
	rec := respond(t, func(req typhon.Request) typhon.Response {
		return RespondCreated(req, &typepb.Type{Name: "Ada"}, "/things/1")
	})
	if rec.Code != http.StatusCreated || rec.Header().Get("Location") != "/things/1" {
		t.Errorf("status %d, Location %q, want 201 /things/1", rec.Code, rec.Header().Get("Location"))
	}
}

func TestRespondErrorIsAServerErrorNotAnEmpty201(t *testing.T) {
	// proto3 text must be valid UTF-8, so this message can't be encoded.
	// Typhon's ErrorFilter only writes an error for a response still at 200,
	// so an error left on a 201 would reach the caller as an empty 201.
	rec := respond(t, func(req typhon.Request) typhon.Response {
		return RespondCreated(req, &typepb.Type{Name: "\xff"}, "/things/1")
	})
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("Location") != "" {
		t.Errorf("status %d, Location %q; want a 500 with no Location", rec.Code, rec.Header().Get("Location"))
	}
}

func TestRespondSendsBinaryWhenAskedFor(t *testing.T) {
	router := typhon.Router{}
	router.GET("/thing", func(req typhon.Request) typhon.Response {
		return Respond(req, &typepb.Type{Name: "Ada"}, http.StatusOK)
	})
	handler := Handler(slog.New(slog.DiscardHandler), obs.NewRegistry(), &router, readyNow(), nil)

	for accept, wantType := range map[string]string{"": "application/json", "application/protobuf": "application/protobuf"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/thing", nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Header().Get("Content-Type") != wantType || rec.Header().Get("Vary") != "Accept" {
			t.Errorf("Accept %q: Content-Type %q, Vary %q; want %s, Accept", accept, rec.Header().Get("Content-Type"), rec.Header().Get("Vary"), wantType)
			continue
		}
		if wantType == "application/protobuf" {
			var got typepb.Type
			if err := proto.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.GetName() != "Ada" {
				t.Errorf("binary body = %v, %v; want Ada", &got, err)
			}
		}
	}
}
