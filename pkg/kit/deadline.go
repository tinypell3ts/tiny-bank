package kit

import (
	"context"
	"strconv"
	"time"

	"github.com/monzo/typhon"
)

const (
	// DeadlineHeader carries how many milliseconds a caller will wait for an
	// answer, like gRPC's grpc-timeout. kit.Get and kit.Post send it, and
	// kit.Handler applies it, so a chain of calls between services shares one
	// deadline instead of each hop starting its own clock.
	DeadlineHeader = "X-Request-Timeout"
	// maxRequestTimeout caps the deadline a caller can ask for, so a header
	// can't keep a request (and what it holds) alive for longer than this.
	maxRequestTimeout = 30 * time.Second
	// deadlineMargin is how much sooner each hop gives up than its caller, so
	// its answer (even "timed out") arrives while the caller is still waiting.
	deadlineMargin = 100 * time.Millisecond
)

// deadlineFilter puts the caller's deadline, from DeadlineHeader, on the
// request's context, capped at maxRequestTimeout. A missing, malformed or
// non-positive header is ignored, so callers outside tiny-bank (curl) work
// as before. When the deadline passes, the request's context ends with
// context.DeadlineExceeded, which cleanErrors answers with a 504 timeout.
func deadlineFilter(req typhon.Request, next typhon.Service) typhon.Response {
	ms, err := strconv.ParseInt(req.Header.Get(DeadlineHeader), 10, 64)
	if err != nil || ms <= 0 {
		return next(req)
	}
	// Cap the milliseconds before turning them into a Duration: a huge value
	// times time.Millisecond would wrap round to a negative duration, which
	// would end the request at once.
	ctx, cancel := context.WithTimeout(req.Context, time.Duration(min(ms, maxRequestTimeout.Milliseconds()))*time.Millisecond)
	defer cancel()
	req.Context = ctx
	return next(req)
}

// callBudget is how long a call to another service may take: callTimeout,
// or less if ctx's deadline (the caller's, minus deadlineMargin) comes
// sooner. ok is false when there's no time left at all.
func callBudget(ctx context.Context) (budget time.Duration, ok bool) {
	budget = callTimeout
	if deadline, has := ctx.Deadline(); has {
		left := time.Until(deadline) - deadlineMargin
		if left <= 0 {
			return 0, false
		}
		budget = min(budget, left)
	}
	return budget, true
}
