package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRetryUntilSuccess_SucceedsImmediately(t *testing.T) {
	calls := 0
	retryUntilSuccess(t.Context(), func(_ context.Context) error {
		calls++
		return nil
	})
	assert.Equal(t, 1, calls)
}

func TestRetryUntilSuccess_SucceedsAfterRetries(t *testing.T) {
	calls := 0
	retryUntilSuccess(t.Context(), func(_ context.Context) error {
		calls++
		if calls < 2 {
			return errors.New("temporary")
		}
		return nil
	})
	assert.Equal(t, 2, calls)
}

func TestRetryUntilSuccess_StopsOn4xx(t *testing.T) {
	calls := 0
	retryUntilSuccess(t.Context(), func(_ context.Context) error {
		calls++
		return &HTTPError{StatusCode: 404, Body: "not found"}
	})
	assert.Equal(t, 1, calls) // 4xx should not be retried
}

func TestRetryUntilSuccess_StopsOnRejectedCredentialRequest(t *testing.T) {
	oldInitial, oldMax := RetryInitialWait, RetryMaxWait
	RetryInitialWait, RetryMaxWait = time.Millisecond, time.Millisecond
	t.Cleanup(func() { RetryInitialWait, RetryMaxWait = oldInitial, oldMax })
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	calls := 0
	retryUntilSuccess(ctx, func(_ context.Context) error {
		calls++
		return &credentialRequestError{status: 401}
	})
	assert.Equal(t, 1, calls)
}

func TestRetryUntilSuccess_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	retryUntilSuccess(ctx, func(_ context.Context) error {
		calls++
		return errors.New("always fail")
	})
	assert.GreaterOrEqual(t, calls, 1)
}
