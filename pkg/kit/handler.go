package kit

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/monzo/terrors"
	"github.com/monzo/typhon"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/tinypell3ts/tiny-bank/pkg/obs"
)

// Handler turns a service's Typhon router into the http.Handler that Serve
// runs. Every request goes through the same filters, outermost first:
//
//   - request ID: reuse the caller's X-Request-Id or make one, and echo it back
//   - access log: one JSON line per request
//   - metrics: RED metrics labelled by route pattern (pkg/obs)
//   - Typhon's ErrorFilter: turns a returned error into a response with the
//     right status, sending the terror's code and message to the caller
//   - clean errors: send only a terror's code, message and params (no stack
//     traces); log anything internal, and hide errors that aren't terrors;
//     answer 499 if the caller has already gone
//   - recover: a panic becomes a 500 instead of crashing the process
//
// GET /metrics is served next to the router, outside the filters, so
// Prometheus scrapes aren't counted as traffic. Every request body is capped
// at MaxBodyBytes with http.MaxBytesReader, which also tells net/http to
// close the connection rather than read the rest of a huge body.
func Handler(logger *slog.Logger, reg *prometheus.Registry, router *typhon.Router) http.Handler {
	metrics := obs.NewHTTPMetrics(reg)
	svc := router.Serve().
		Filter(recoverFilter(logger)).
		Filter(cleanErrors(logger)).
		Filter(typhon.ErrorFilter).
		Filter(metricsFilter(metrics, router)).
		Filter(accessLogFilter(logger)).
		Filter(requestIDFilter)

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", obs.Handler(reg))
	mux.Handle("/", limitBody(typhon.HttpHandler(svc)))
	return mux
}

// Health is the liveness check, served at /healthz: "the process is alive".
// It never checks dependencies such as the database. If it fails, Kubernetes
// restarts the pod, and restarting every pod because a dependency blinked
// makes things worse.
func Health(req typhon.Request) typhon.Response {
	return req.Response(map[string]string{"status": "ok"})
}

// requestIDFilter gives every request an ID and stores it in the request's
// context, where RequestIDFrom finds it. It reuses the caller's X-Request-Id
// if validRequestID accepts it, and otherwise makes a new one.
func requestIDFilter(req typhon.Request, next typhon.Service) typhon.Response {
	id := req.Header.Get(RequestIDHeader)
	if !validRequestID(id) {
		id = rand.Text()
	}
	req.Context = context.WithValue(req.Context, requestIDKey{}, id)
	rsp := next(req)
	if rsp.Response != nil {
		rsp.Header.Set(RequestIDHeader, id)
	}
	return rsp
}

// accessLogFilter writes one structured log line per request.
func accessLogFilter(logger *slog.Logger) typhon.Filter {
	return func(req typhon.Request, next typhon.Service) typhon.Response {
		start := time.Now()
		rsp := next(req)
		logger.LogAttrs(req.Context, slog.LevelInfo, "http request",
			slog.String("request_id", RequestIDFrom(req.Context)),
			slog.String("method", req.Method),
			slog.String("path", req.URL.Path),
			slog.Int("status", statusOf(rsp)),
			slog.Float64("duration_ms", float64(time.Since(start))/float64(time.Millisecond)),
		)
		return rsp
	}
}

// metricsFilter records RED metrics. The route label is the pattern the
// router matched ("/accounts/:id"), or "" (recorded as "unmatched") when none
// did. The router records it on the request it hands the handler, and the
// response carries that request back out. A response built from the
// unrouted request, like the 500 from recoverFilter after a panic, doesn't
// carry it, so only then does it ask the router to match the path again.
func metricsFilter(m *obs.HTTPMetrics, router *typhon.Router) typhon.Filter {
	return func(req typhon.Request, next typhon.Service) typhon.Response {
		done := m.Track()
		rsp := next(req)
		route := ""
		if rsp.Request != nil {
			route, _ = typhon.RequestPatternFromContext(rsp.Request.Context)
		}
		if route == "" {
			route = router.Pattern(req)
		}
		done(req.Method, route, statusOf(rsp))
		return rsp
	}
}

// limitBody caps every request body at MaxBodyBytes (see Handler).
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// cleanErrors makes the error Typhon's ErrorFilter sends to the caller safe
// to send. ErrorFilter serialises a terror as it is, including its stack trace
// (with file paths) and message history, and it treats a terror wrapped with
// fmt.Errorf("...: %w", err) as an unknown internal error. So this filter,
// which runs just inside ErrorFilter:
//
//   - finds the terror inside any wrapping, so its code and message survive
//   - strips the stack and message history before anything is sent
//   - logs any error that isn't a terror (a raw database error, say) with the
//     request ID, and sends a generic internal error instead
//   - logs every internal_service error, so a 500 can be traced from logs
//
// The full error is logged because it's the only record of why a request
// failed. That's safe because secrets can't get into errors in the first
// place: the services' secrets are kit.Secret values, which never print, and
// pgx hides database passwords in its own errors (AGENTS.md).
func cleanErrors(logger *slog.Logger) typhon.Filter {
	return func(req typhon.Request, next typhon.Service) typhon.Response {
		rsp := next(req)
		if rsp.Error == nil {
			return rsp
		}
		// The caller hung up (or the server's shutdown deadline passed): nobody
		// is waiting for this response, so it isn't a server failure. It checks
		// the request's own context, not the error, so a context.Canceled from
		// inside the service while the caller is still waiting stays a 500.
		if req.Err() != nil { // typhon.Request embeds its context.Context
			logger.InfoContext(req.Context, "request cancelled before it finished",
				"request_id", RequestIDFrom(req.Context))
			return typhon.NewResponseWithCode(req, statusClientClosedRequest)
		}
		var terr *terrors.Error
		if !errors.As(rsp.Error, &terr) {
			logger.ErrorContext(req.Context, "request failed",
				"request_id", RequestIDFrom(req.Context),
				"error", rsp.Error.Error(),
			)
			terr = terrors.InternalService("", "internal error", nil)
		} else if terr.PrefixMatches(terrors.ErrInternalService) {
			logger.ErrorContext(req.Context, "request failed",
				"request_id", RequestIDFrom(req.Context),
				"error", rsp.Error.Error(),
				"stack", terr.StackString(),
			)
		}
		clean := *terr
		clean.StackFrames = nil
		clean.MessageChain = nil
		rsp.Error = &clean
		return rsp
	}
}

// recoverFilter turns a panic in a handler into a 500 response, so one bad
// request can't crash the service for everyone. The panic and stack are logged.
func recoverFilter(logger *slog.Logger) typhon.Filter {
	return func(req typhon.Request, next typhon.Service) (rsp typhon.Response) {
		defer func() {
			if p := recover(); p != nil {
				logger.ErrorContext(req.Context, "handler panicked",
					"request_id", RequestIDFrom(req.Context),
					"panic", fmt.Sprint(p),
					"stack", string(debug.Stack()),
				)
				rsp = typhon.NewResponse(req)
				rsp.Error = terrors.InternalService("panic", "internal error", nil)
			}
		}()
		return next(req)
	}
}

// statusClientClosedRequest is nginx's convention for "the caller went away
// first". It's a 4xx, so metrics don't count it as a server error.
const statusClientClosedRequest = 499

// statusOf is the status a response will be sent with: 200 if nothing else
// was set, as net/http does.
func statusOf(rsp typhon.Response) int {
	if rsp.Response == nil || rsp.StatusCode == 0 {
		return http.StatusOK
	}
	return rsp.StatusCode
}
