package kit

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/monzo/terrors"
	"github.com/monzo/typhon"
	"google.golang.org/protobuf/proto"
)

const (
	// callTimeout bounds one call to another service, unless the caller's
	// context ends sooner. A stuck service then fails its callers' requests
	// in seconds instead of holding them open until they give up.
	callTimeout = 5 * time.Second
	// maxResponseBytes caps a response body read from another service, so a
	// misbehaving one can't make its caller use up memory.
	maxResponseBytes = 1 << 20
	// maxConnsPerHost is the most connections open to one other service at
	// once, and how many are kept open between calls to be used again.
	maxConnsPerHost = 100
)

// transport is the connection pool for calls to other services. Typhon's
// default keeps only 10 idle connections to a host and opens as many as are
// asked for. Under load that churns: each burst dials hundreds of new
// connections and closes most of them, and every closed one holds a local
// port for a minute (TIME_WAIT), until the pod has none left and calls fail
// with "cannot assign requested address". A load test of the ledger hit
// exactly that. This pool keeps up to maxConnsPerHost open and reuses them;
// a call beyond that waits for a free connection (within callTimeout)
// instead of dialling another.
var transport = &http.Transport{
	DialContext:           (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxConnsPerHost:       maxConnsPerHost,
	MaxIdleConnsPerHost:   maxConnsPerHost,
	MaxIdleConns:          0, // no limit across hosts: MaxIdleConnsPerHost bounds each
	IdleConnTimeout:       90 * time.Second,
	ResponseHeaderTimeout: callTimeout,
}

// client sends requests to other tiny-bank services. It's Typhon's HTTP
// client, on transport's connection pool, with these filters, innermost
// first:
//
//   - wireStatus notes the status of the response that actually came back
//     over the network, for the call's span, before ErrorFilter can turn a
//     transport error into a made-up 500
//   - capBody limits every response body to maxResponseBytes+1, before
//     anything reads it. ErrorFilter reads an error response's whole body
//     (and makes it the error's message if it isn't a terror), so without
//     this a huge error page would be read into memory and logged in full.
//   - ErrorFilter turns an error response back into the terror the other
//     service returned, code and params included (ADR-0007)
//   - ErrorFilter again, because the first one loses transport errors (a
//     timeout, a refused connection). It writes them into the response as a
//     terror, and then its encoder resets rsp.Error, so the call would look
//     like it worked. The second reads that terror back into rsp.Error.
//     TestGetEndsWithTheCallersContext fails without it.
//   - forwardRequestID passes the current request's ID on, so one request's
//     log lines can be followed across every service it touches
//   - forwardTrace passes the current span on as a W3C traceparent, so the
//     other service's span joins this request's trace
var client = typhon.HttpService(transport).
	Filter(wireStatus).
	Filter(capBody).
	Filter(typhon.ErrorFilter).
	Filter(typhon.ErrorFilter).
	Filter(forwardRequestID).
	Filter(forwardTrace)

// wireStatusKey holds, in a call's context, where wireStatus writes the
// status of the response that came back. It's a pointer back to call, not an
// attribute set on the span, because endCallSpan needs to know whether any
// response came back at all to label the error (a span's attributes can't be
// read back).
type wireStatusKey struct{}

// wireStatus records the status of a response that came back over the
// network. On a transport error (no connection, a timeout) nothing came
// back, so it records nothing.
func wireStatus(req typhon.Request, next typhon.Service) typhon.Response {
	rsp := next(req)
	if p, ok := req.Value(wireStatusKey{}).(*int); ok && rsp.Response != nil && rsp.Error == nil {
		*p = rsp.StatusCode
	}
	return rsp
}

// capBody limits the response's body to maxResponseBytes+1 bytes. Get reads
// one byte more than the cap, so it can tell a body that's too large from one
// exactly at it.
func capBody(req typhon.Request, next typhon.Service) typhon.Response {
	rsp := next(req)
	if rsp.Response != nil && rsp.Body != nil {
		rsp.Body = cappedBody{Reader: io.LimitReader(rsp.Body, maxResponseBytes+1), Closer: rsp.Body}
	}
	return rsp
}

// cappedBody reads from a limited reader but closes the real body.
type cappedBody struct {
	io.Reader
	io.Closer
}

// forwardRequestID sets X-Request-Id on an outgoing request to the ID of the
// request being handled, which kit.Handler stored in the context.
func forwardRequestID(req typhon.Request, next typhon.Service) typhon.Response {
	if id := RequestIDFrom(req.Context); id != "" {
		req.Header.Set(RequestIDHeader, id)
	}
	return next(req)
}

// Get fetches url from another tiny-bank service into m, as binary protobuf,
// within callTimeout. Pass the context of the request being handled, so the
// call ends when it does and carries its request ID.
//
// It reads leniently: fields m doesn't have are skipped, so the other service
// can add fields to its responses without breaking this caller. An error
// response comes back as the other service's terror. Find it with errors.As,
// and decide what to tell your own caller: passing it on unchanged tells them
// about a service they didn't call.
func Get(ctx context.Context, url string, m proto.Message) error {
	return call(ctx, http.MethodGet, url, nil, m)
}

// Post sends body to url on another tiny-bank service, as binary protobuf,
// and reads the answer into m, exactly as Get does (timeout, request ID,
// lenient reading, the other service's terror on an error).
//
// A Post that times out or loses its connection is ambiguous: the other
// service may or may not have acted on it. So only Post what's safe to send
// again, such as a journal with an idempotency key, and retry with the same
// key until you get an answer.
func Post(ctx context.Context, url string, body, m proto.Message) error {
	return call(ctx, http.MethodPost, url, body, m)
}

// call makes one call to another service, as Get and Post describe, inside
// a client span (see startCallSpan) that ends with its status and any error.
func call(ctx context.Context, method, url string, body, m proto.Message) (err error) {
	ctx, span := startCallSpan(ctx, method, url)
	status := 0 // set by wireStatus if a response comes back
	ctx = context.WithValue(ctx, wireStatusKey{}, &status)
	defer func() { endCallSpan(span, status, err) }()

	// Share the caller's deadline: give up a little before it, and tell the
	// other service how long it has, so it stops in time to answer too.
	budget, ok := callBudget(ctx)
	if !ok {
		return fmt.Errorf("%s %s: %w", method, url, terrors.Timeout("deadline", "no time left before the caller's deadline", nil))
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	req := typhon.NewRequest(ctx, method, url, nil)
	req.Header.Set("Accept", protobufType)
	req.Header.Set(DeadlineHeader, strconv.FormatInt(max(budget.Milliseconds(), 1), 10))
	if body != nil {
		req.EncodeAsProtobuf(body) // sets Content-Type: application/protobuf
	}
	rsp := req.SendVia(client).Response()
	if rsp.Error != nil {
		return fmt.Errorf("%s %s: %w", method, url, rsp.Error)
	}
	defer func() { _ = rsp.Body.Close() }()

	if mediaType, _, _ := mime.ParseMediaType(rsp.Header.Get("Content-Type")); mediaType != protobufType {
		return fmt.Errorf("%s %s: %w", method, url, terrors.BadResponse("content_type", "response isn't binary protobuf", nil))
	}
	b, err := io.ReadAll(rsp.Body) // capped at maxResponseBytes+1 by capBody
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, url, err)
	}
	if len(b) > maxResponseBytes {
		return fmt.Errorf("%s %s: %w", method, url, terrors.BadResponse("too_large", "response is too large", nil))
	}
	if err := proto.Unmarshal(b, m); err != nil { // lenient: unknown fields are kept and ignored
		return fmt.Errorf("%s %s: %w", method, url, terrors.BadResponse("invalid_proto", "response isn't valid protobuf", nil))
	}
	return nil
}
