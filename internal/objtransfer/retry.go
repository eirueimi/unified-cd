package objtransfer

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// retryAttempts bounds how many times a throttled (429) or unavailable
	// (503) presign request is attempted. Many sidecars can share one source
	// address (Ingress, load balancer, SNAT) and so one controller-side
	// rate-limit bucket; a short bounded wait lets a burst of them drain
	// instead of failing the step.
	retryAttempts = 6
	// maxRetryDelay caps any single wait, including a larger Retry-After.
	maxRetryDelay = 10 * time.Second
	// baseRetryDelay is the first backoff step when no Retry-After is given.
	baseRetryDelay = 200 * time.Millisecond
	// bindingRetryDelay is the wait between binding-pending retries when the
	// controller sends no Retry-After.
	bindingRetryDelay = time.Second
)

// bindingWait is how long a 409 binding-pending answer (the run's pod is not
// bound yet, so the controller cannot authorize the request) is retried.
// A variable so tests can shorten it.
var bindingWait = 30 * time.Second

// sleep waits between attempts; a variable so tests can run without real
// delays.
var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// parseRetryAfter reads a Retry-After header value (seconds or an HTTP date).
// It returns 0 when the value is absent or unusable.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// backoff is the wait before retry number attempt (0-based): the server's
// Retry-After when given, else exponential from baseRetryDelay, capped at
// maxRetryDelay.
func backoff(attempt int, retryAfter time.Duration) time.Duration {
	d := retryAfter
	if d <= 0 {
		d = baseRetryDelay << min(attempt, 10)
	}
	return min(d, maxRetryDelay)
}
