package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// retryUntilSuccess retries fn with backoff until it succeeds.
// Each attempt is given a 10s timeout. Returns immediately if ctx is cancelled.
// Returns immediately without retrying for permanent errors such as 4xx.
// Backoff: 1s -> 2s -> 4s -> ... -> 30s (cap).
func retryUntilSuccess(ctx context.Context, fn func(ctx context.Context) error) {
	wait := time.Second
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
		if wait < 30*time.Second {
			wait *= 2
		}
	}
}
