package controller

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEnrollmentLimiterBoundsEntriesAndRefills(t *testing.T) {
	now := time.Now()
	limiter := newEnrollmentLimiter(func() time.Time { return now })
	req := httptest.NewRequest("POST", "/api/v1/agents/enroll", nil)
	req.RemoteAddr = "192.0.2.10:4321"

	for range 5 {
		require.True(t, limiter.allow(req, "one-time-token", ""))
	}
	require.False(t, limiter.allow(req, "one-time-token", ""))
	now = now.Add(6 * time.Second)
	require.True(t, limiter.allow(req, "one-time-token", ""))

	for i := 0; i < 4200; i++ {
		req.RemoteAddr = fmt.Sprintf("[2001:db8::%x]:4321", i)
		limiter.allow(req, "one-time-token", "policy")
	}
	require.LessOrEqual(t, limiter.len(), 4096)
}
