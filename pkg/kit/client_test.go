package kit

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/monzo/terrors"
	"github.com/monzo/typhon"
	"google.golang.org/protobuf/types/known/apipb"
	"google.golang.org/protobuf/types/known/typepb"

	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// otherService runs routes behind kit.Handler on a real HTTP server, as
// another tiny-bank service would be, and returns its base URL.
func otherService(t *testing.T, routes func(*typhon.Router)) string {
	t.Helper()
	router := typhon.Router{}
	routes(&router)
	srv := httptest.NewServer(Handler(slog.New(slog.DiscardHandler), obs.NewRegistry(), &router, readyNow(), nil))
	t.Cleanup(srv.Close)
	return srv.URL
}

// handling returns a context like a handler's: one carrying a request ID.
func handling(t *testing.T, id string) context.Context {
	return context.WithValue(t.Context(), requestIDKey{}, id)
}

func TestGetReadsBinaryAndForwardsTheRequestID(t *testing.T) {
	base := otherService(t, func(r *typhon.Router) {
		r.GET("/thing", func(req typhon.Request) typhon.Response {
			// Echo the request ID the call arrived with, and send a field
			// (syntax, number 6) that the caller's message below doesn't have.
			return Respond(req, &typepb.Type{Name: RequestIDFrom(req.Context), Syntax: typepb.Syntax_SYNTAX_EDITIONS}, http.StatusOK)
		})
	})

	// apipb.Mixin shares field 1 (name) with typepb.Type but has no field 6,
	// like a caller built against an older contract.
	var got apipb.Mixin
	if err := Get(handling(t, "req-42"), base+"/thing", &got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.GetName() != "req-42" {
		t.Errorf("the other service saw request ID %q, want req-42", got.GetName())
	}
}

func TestGetReturnsTheOtherServicesTerror(t *testing.T) {
	base := otherService(t, func(r *typhon.Router) {
		r.GET("/thing", func(req typhon.Request) typhon.Response {
			return typhon.Response{Error: terrors.NotFound("thing", "no such thing", map[string]string{"kind": "widget"})}
		})
	})

	err := Get(t.Context(), base+"/thing", &apipb.Mixin{})
	var terr *terrors.Error
	if !errors.As(err, &terr) || terr.Code != "not_found.thing" || terr.Message != "no such thing" || terr.Params["kind"] != "widget" {
		t.Fatalf("err = %v, want the other service's not_found.thing with its params", err)
	}
}

func TestGetRefusesANonProtobufResponse(t *testing.T) {
	base := otherService(t, func(r *typhon.Router) {
		r.GET("/thing", func(req typhon.Request) typhon.Response { return req.Response(map[string]string{"name": "x"}) })
	})

	err := Get(t.Context(), base+"/thing", &apipb.Mixin{})
	var terr *terrors.Error
	if !errors.As(err, &terr) || terr.Code != "bad_response.content_type" {
		t.Fatalf("err = %v, want bad_response.content_type", err)
	}
}

func TestGetEndsWithTheCallersContext(t *testing.T) {
	base := otherService(t, func(r *typhon.Router) {
		r.GET("/slow", func(req typhon.Request) typhon.Response { <-req.Done(); return req.Response(nil) })
	})

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := Get(ctx, base+"/slow", &apipb.Mixin{})
	var terr *terrors.Error
	if !errors.As(err, &terr) || terr.Code != terrors.ErrInternalService {
		t.Fatalf("err = %v, want Typhon's internal_service for the timeout, not a success", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Get took %v, want it to give up when the caller's context ends", took)
	}
}

func TestGetFailsWhenTheOtherServiceIsDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens there now

	if err := Get(t.Context(), url+"/thing", &apipb.Mixin{}); err == nil {
		t.Fatal("Get to a service that's down succeeded")
	}
}
