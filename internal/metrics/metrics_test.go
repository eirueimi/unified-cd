package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerLabel(t *testing.T) {
	assert.Equal(t, "webhook", triggerLabel("webhook:push"))
	assert.Equal(t, "schedule", triggerLabel("schedule:nightly"))
	assert.Equal(t, "api", triggerLabel("api"))
	assert.Equal(t, "api", triggerLabel("some-user@example.com")) // principal names fold into api
}

func TestRecorders(t *testing.T) {
	m := New()
	m.RunCreated("webhook:push")
	m.RunFinished("Failed")
	m.StepCompleted("Succeeded")
	m.StepDuration("Succeeded", 12.5)
	m.WebhookEvent("push", "accepted")
	m.HTTPRequest("GET", "/api/v1/runs/{id}", 200, 0.05)

	assert.Equal(t, 1.0, testutil.ToFloat64(m.runsCreated.WithLabelValues("webhook")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.runsFinished.WithLabelValues("Failed")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.stepsCompleted.WithLabelValues("Succeeded")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.webhookEvents.WithLabelValues("push", "accepted")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.httpRequests.WithLabelValues("GET", "/api/v1/runs/{id}", "200")))
}

func TestHTTPRequestFoldsMethodLabel(t *testing.T) {
	m := New()
	m.HTTPRequest("GET", "/api/v1/runs/{id}", 200, 0.05)
	m.HTTPRequest("FOO123", "/api/v1/runs/{id}", 200, 0.05)

	assert.Equal(t, 1.0, testutil.ToFloat64(m.httpRequests.WithLabelValues("GET", "/api/v1/runs/{id}", "200")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.httpRequests.WithLabelValues("other", "/api/v1/runs/{id}", "200")))
}

func TestHandlerServesTextFormat(t *testing.T) {
	m := New()
	m.RunCreated("api")
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	require.Equal(t, 200, rec.Code)
	assert.True(t, strings.Contains(rec.Body.String(), "unifiedcd_runs_created_total"))
}

func TestTwoInstancesDoNotCollide(t *testing.T) {
	// Per-instance registries: constructing two must not panic on duplicate registration.
	_ = New()
	_ = New()
}

func TestSetBuildInfoExportsExactlyOneVersionSeries(t *testing.T) {
	m := New()
	m.SetBuildInfo("v1.2.3")

	assert.Equal(t, 1.0, testutil.ToFloat64(m.buildInfo.WithLabelValues("v1.2.3")))
	require.Equal(t, 1, testutil.CollectAndCount(m.buildInfo))

	// A second call replaces the series rather than accumulating one per
	// version: the process only ever has one build.
	m.SetBuildInfo("v1.3.0")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.buildInfo.WithLabelValues("v1.3.0")))
	require.Equal(t, 1, testutil.CollectAndCount(m.buildInfo))

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `unifiedcd_build_info{version="v1.3.0"} 1`)
}

func TestAgentAuthEventsUseBoundedLabels(t *testing.T) {
	m := New()
	m.AgentAuthEvent("uce", "success", "anything")
	m.AgentAuthEvent("unknown-provider", "failure", "unrecognized")

	assert.Equal(t, 1.0, testutil.ToFloat64(m.agentAuthEvents.WithLabelValues("one-time-token", "success", "other")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.agentAuthEvents.WithLabelValues("other", "failure", "other")))
}
