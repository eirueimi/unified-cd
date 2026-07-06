package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetricsEndpointWithoutSetMetricsIs404(t *testing.T) {
	s, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestMetricsEndpointServesUnauthenticated(t *testing.T) {
	s, _ := newTestServer(t)
	s.SetMetrics(metrics.New())

	rec := httptest.NewRecorder()
	// No Authorization header on purpose.
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, strings.Contains(rec.Body.String(), "unifiedcd_"))
}

func TestHTTPMiddlewareUsesRoutePattern(t *testing.T) {
	s, _ := newTestServer(t)
	s.SetMetrics(metrics.New())

	// Hit a parameterized route (auth fails with 401 — still recorded).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/some-id", nil)
	s.Router().ServeHTTP(httptest.NewRecorder(), req)

	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	assert.True(t, strings.Contains(body, `route="/api/v1/runs/{id}"`),
		"expected chi route pattern label, got:\n"+body)
	assert.False(t, strings.Contains(body, `route="/api/v1/runs/some-id"`),
		"raw path must never appear as a label")
}
