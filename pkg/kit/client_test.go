package kit

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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
	// The server never answers, not even when the deadline it was sent passes
	// (a hung service), so the caller's own context has to end the call, and
	// the transport error must come back as an error, not a success.
	hung := make(chan struct{})
	base := otherService(t, func(r *typhon.Router) {
		r.GET("/slow", func(req typhon.Request) typhon.Response { <-hung; return req.Response(nil) })
	})
	t.Cleanup(func() { close(hung) })

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

// TestGetCapsErrorBodies: an error response's body is read by Typhon's
// ErrorFilter, and for one that isn't a terror it becomes the error message.
// It must be capped like any other body, not read whole.
func TestGetCapsErrorBodies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("x", 4*maxResponseBytes))) // a huge error page from a proxy, say
	}))
	t.Cleanup(srv.Close)

	err := Get(t.Context(), srv.URL+"/thing", &apipb.Mixin{})
	if err == nil {
		t.Fatal("want an error for a 502")
	}
	if n := len(err.Error()); n > maxResponseBytes+1024 {
		t.Errorf("error message is %d bytes; want the body capped at about %d", n, maxResponseBytes)
	}
}

func TestGetRefusesATooLargeResponse(t *testing.T) {
	base := otherService(t, func(r *typhon.Router) {
		r.GET("/thing", func(req typhon.Request) typhon.Response {
			return Respond(req, &typepb.Type{Name: strings.Repeat("x", maxResponseBytes)}, http.StatusOK)
		})
	})
	err := Get(t.Context(), base+"/thing", &apipb.Mixin{})
	var terr *terrors.Error
	if !errors.As(err, &terr) || terr.Code != "bad_response.too_large" {
		t.Fatalf("err = %v, want bad_response.too_large", err)
	}
}

// TestGetReusesAPoolOfConnections: bursts of calls reuse a fixed pool of
// connections instead of dialling one each. Typhon's default transport keeps
// only 10 idle and opens as many as asked for, so bursts like these dialled
// hundreds and, under real load, used up the pod's local ports.
func TestGetReusesAPoolOfConnections(t *testing.T) {
	router := typhon.Router{}
	router.GET("/thing", func(req typhon.Request) typhon.Response {
		time.Sleep(5 * time.Millisecond) // long enough for calls to overlap
		return Respond(req, &typepb.Type{Name: "x"}, http.StatusOK)
	})
	var dialled atomic.Int64
	srv := httptest.NewUnstartedServer(Handler(slog.New(slog.DiscardHandler), obs.NewRegistry(), &router, readyNow(), nil))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			dialled.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	const bursts, perBurst = 5, 300
	for range bursts {
		var wg sync.WaitGroup
		errs := make([]error, perBurst)
		for i := range perBurst {
			wg.Go(func() { errs[i] = Get(t.Context(), srv.URL+"/thing", &apipb.Mixin{}) })
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			t.Fatalf("calls failed: %v", err)
		}
	}
	if n := dialled.Load(); n > maxConnsPerHost {
		t.Errorf("%d calls opened %d connections; want at most %d, reused across bursts", bursts*perBurst, n, maxConnsPerHost)
	}
}

func TestPostSendsBinaryAndReadsTheAnswer(t *testing.T) {
	base := otherService(t, func(r *typhon.Router) {
		r.POST("/things", func(req typhon.Request) typhon.Response {
			var in typepb.Type
			if err := Decode(req, &in); err != nil {
				return typhon.Response{Error: err}
			}
			if in.GetName() == "bad" {
				return typhon.Response{Error: terrors.PreconditionFailed("thing", "not that one", nil)}
			}
			// Echo what arrived, with the content type and request ID it came with.
			return Respond(req, &typepb.Type{Name: in.GetName() + " " + req.Header.Get("Content-Type") + " " + RequestIDFrom(req.Context)}, http.StatusCreated)
		})
	})

	var got apipb.Mixin
	if err := Post(handling(t, "req-7"), base+"/things", &typepb.Type{Name: "Ada"}, &got); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if got.GetName() != "Ada application/protobuf req-7" {
		t.Errorf("the other service saw %q, want the body, binary, and the request ID", got.GetName())
	}

	err := Post(t.Context(), base+"/things", &typepb.Type{Name: "bad"}, &apipb.Mixin{})
	var terr *terrors.Error
	if !errors.As(err, &terr) || terr.Code != "precondition_failed.thing" {
		t.Fatalf("err = %v, want the other service's precondition_failed.thing", err)
	}
}

// TestPostIsStrictOnTheOtherSide: a field the other service's message
// doesn't have is refused there, as JSON would be, so binary callers follow
// the same deploy order (proto/AGENTS.md).
func TestPostIsStrictOnTheOtherSide(t *testing.T) {
	base := otherService(t, func(r *typhon.Router) {
		r.POST("/things", func(req typhon.Request) typhon.Response {
			var in apipb.Mixin // has name (1) and root (2), but not syntax (6)
			if err := Decode(req, &in); err != nil {
				return typhon.Response{Error: err}
			}
			return Respond(req, &in, http.StatusOK)
		})
	})
	err := Post(t.Context(), base+"/things", &typepb.Type{Name: "Ada", Syntax: typepb.Syntax_SYNTAX_EDITIONS}, &apipb.Mixin{})
	var terr *terrors.Error
	if !errors.As(err, &terr) || terr.Code != "bad_request.invalid_proto" {
		t.Fatalf("err = %v, want bad_request.invalid_proto from the other service", err)
	}
}
