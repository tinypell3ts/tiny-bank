package kit

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monzo/terrors"
	"github.com/monzo/typhon"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/apipb"
	"google.golang.org/protobuf/types/known/typepb"

	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// deadlineSeen serves /deadline behind kit.Handler and reports, as the
// response's name, how long the handler's context had left.
func deadlineSeen(t *testing.T, header string) string {
	t.Helper()
	router := typhon.Router{}
	router.GET("/deadline", func(req typhon.Request) typhon.Response {
		left := "none"
		if d, ok := req.Deadline(); ok {
			left = strconv.FormatInt(time.Until(d).Milliseconds(), 10)
		}
		return Respond(req, &typepb.Type{Name: left}, http.StatusOK)
	})
	h := Handler(slog.New(slog.DiscardHandler), obs.NewRegistry(), &router, readyNow(), nil)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/deadline", nil)
	req.Header.Set("Accept", protobufType)
	if header != "" {
		req.Header.Set(DeadlineHeader, header)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var got typepb.Type
	if err := proto.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	return got.GetName()
}

func TestHandlerAppliesTheCallersDeadline(t *testing.T) {
	ms := func(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }
	if got := ms(deadlineSeen(t, "2000")); got < 1900 || got > 2000 {
		t.Errorf("X-Request-Timeout: 2000 gave the handler %dms, want about 2000", got)
	}
	if got := ms(deadlineSeen(t, "999999")); got > maxRequestTimeout.Milliseconds() {
		t.Errorf("a huge deadline gave %dms, want it capped at %s", got, maxRequestTimeout)
	}
	for _, ignored := range []string{"", "soon", "-5", "0"} {
		if got := deadlineSeen(t, ignored); got != "none" {
			t.Errorf("X-Request-Timeout %q set a deadline (%s), want it ignored", ignored, got)
		}
	}
}

// waitForDeadline answers when the request's deadline passes, or after 2s
// if it has none, so a test with the deadline broken fails instead of
// hanging.
func waitForDeadline(req typhon.Request) typhon.Response {
	select {
	case <-req.Done():
		return typhon.Response{Error: req.Err()}
	case <-time.After(2 * time.Second):
		return req.Response(nil)
	}
}

// TestAPassedDeadlineIsA504NotA499: the caller is still waiting (a little
// longer than us), so it gets a timeout it can act on.
func TestAPassedDeadlineIsA504NotA499(t *testing.T) {
	router := typhon.Router{}
	router.GET("/slow", waitForDeadline)
	h := Handler(slog.New(slog.DiscardHandler), obs.NewRegistry(), &router, readyNow(), nil)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/slow", nil)
	req.Header.Set(DeadlineHeader, "50")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d %s, want 504 for a passed deadline", rec.Code, rec.Body)
	}
}

// TestDeadlinesNestDownAChain: A calls B, B calls C, and C never answers. B
// must give up first and tell A, before A's own deadline, so A gets B's
// clear "timed out" rather than timing out itself.
func TestDeadlinesNestDownAChain(t *testing.T) {
	c := otherService(t, func(r *typhon.Router) {
		r.GET("/c", waitForDeadline)
	})
	var bAnswered atomic.Bool
	b := otherService(t, func(r *typhon.Router) {
		r.GET("/b", func(req typhon.Request) typhon.Response {
			err := Get(req.Context, c+"/c", &apipb.Mixin{})
			bAnswered.Store(true)
			return typhon.Response{Error: terrors.InternalService("c_failed", "c didn't answer: "+err.Error(), nil)}
		})
	})

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	err := Get(ctx, b+"/b", &apipb.Mixin{})
	var terr *terrors.Error
	if !errors.As(err, &terr) || terr.Code != "internal_service.c_failed" {
		t.Fatalf("A got %v; want B's own answer, internal_service.c_failed, before A's deadline", err)
	}
	if !bAnswered.Load() {
		t.Error("B never got to answer")
	}
}

func TestCallFailsFastWithNoTimeLeft(t *testing.T) {
	var called atomic.Bool
	base := otherService(t, func(r *typhon.Router) {
		r.GET("/thing", func(req typhon.Request) typhon.Response {
			called.Store(true)
			return Respond(req, &typepb.Type{}, http.StatusOK)
		})
	})
	ctx, cancel := context.WithTimeout(t.Context(), deadlineMargin/2)
	defer cancel()
	err := Get(ctx, base+"/thing", &apipb.Mixin{})
	var terr *terrors.Error
	if !errors.As(err, &terr) || terr.Code != "timeout.deadline" || called.Load() {
		t.Errorf("err = %v, called %v; want timeout.deadline without calling the other service", err, called.Load())
	}
}
