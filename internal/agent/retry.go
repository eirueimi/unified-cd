package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// RetryInitialWait and RetryMaxWait bound retryUntilSuccess's backoff (starts
// at RetryInitialWait, doubles each attempt, capped at RetryMaxWait). They
// are exported vars, not consts, so tests (in this package and others, e.g.
// the k8s agent) can shorten them instead of sleeping through real backoff
// delays.
var (
	RetryInitialWait = time.Second
	RetryMaxWait     = 30 * time.Second
)

// retryUntilSuccess retries fn with backoff until it succeeds.
// Each attempt is given a 10s timeout. Returns immediately if ctx is cancelled.
// Returns immediately without retrying for permanent errors such as 4xx.
// Backoff: RetryInitialWait -> ... -> RetryMaxWait (cap), doubling each time.
func retryUntilSuccess(ctx context.Context, fn func(ctx context.Context) error) {
	wait := RetryInitialWait
	for {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := fn(callCtx)
		cancel()
		if err == nil {
			return
		}
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode < 500 {
			slog.Error("permanent error, giving up retry", "status", httpErr.StatusCode, "error", httpErr.Body)
			return
		}
		slog.Warn("retrying after failure", "error", err, "nextWait", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait < RetryMaxWait {
			wait *= 2
		}
	}
}

// RetryUntilSuccess retries fn until it returns nil or ctx is done.
//
// It is the exported form of the retry helper the host agent uses
// internally (retryUntilSuccess): each attempt gets a bounded per-call
// timeout (10s), a permanent 4xx-class HTTPError aborts immediately without
// retrying, and any other failure (including 5xx) backs off — starting at
// RetryInitialWait, doubling up to RetryMaxWait — until fn succeeds or
// ctx.Done() fires. Callers outside package agent (e.g. the k8s agent) use
// this to get identical report-until-success semantics for their own
// controller calls.
func RetryUntilSuccess(ctx context.Context, fn func(ctx context.Context) error) {
	retryUntilSuccess(ctx, fn)
}
