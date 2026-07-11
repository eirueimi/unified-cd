package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWebhookIngress_PersistsRequiredCaps verifies that handleWebhookIngress
// mirrors handleTriggerRun: it infers dsl.RequiredCaps(spec) for the
// triggered job and passes it into CreateRun, so a webhook-triggered run is
// only claimable by an agent whose advertised capabilities are a superset of
// what the run needs. Before this fix, the webhook path passed nil for
// required_caps (so any agent could claim a native/pod-only job) and instead
// pinned podTemplate-needs-kubernetes jobs to a "kubernetes" agentSelector
// label — that pin is now gone too, replaced by the required_caps/capabilities
// match, exactly as api_runs_capabilities_test.go and
// api_runs_podtemplate_routing_test.go verify for the direct trigger path.
func TestWebhookIngress_PersistsRequiredCaps(t *testing.T) {
	t.Run("native job requires native capability, not a kubernetes label", func(t *testing.T) {
		s, pg := newTestServer(t)
		_, _ = pg.UpsertJob(t.Context(), "nativejob", "unified-cd/v1",
			[]byte(`{"native":true,"steps":[{"name":"s","run":"echo x"}]}`))
		spec, _ := json.Marshal(map[string]any{
			"trigger": map[string]any{"job": "nativejob"},
			"auth":    map[string]any{"type": "none"},
		})
		_, _ = pg.UpsertWebhookReceiver(t.Context(), "native-hook", spec)

		req := httptest.NewRequest(http.MethodPost, "/webhook/native-hook", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		_, _ = pg.TransitionPendingToQueued(t.Context(), 10)

		// A pod-only (or container-only) agent must not be able to claim a
		// native-required run — confirming required_caps was persisted as
		// ["native"], not left nil/empty (which would be a subset of any
		// agent's capabilities and wrongly claimable).
		require.NoError(t, pg.UpsertAgent(t.Context(), "pod-only", "h1", "linux", "", []string{}, []string{"pod"}, nil))
		require.NoError(t, pg.UpsertAgent(t.Context(), "native-agent", "h2", "linux", "", []string{}, []string{"native"}, nil))

		claimed, err := pg.ClaimNextRun(t.Context(), "pod-only", nil)
		require.NoError(t, err)
		assert.Nil(t, claimed, "a pod-only agent must not claim a native-required webhook run")

		// A legacy agent that carries no "kubernetes" label is irrelevant here
		// (native jobs were never pinned to kubernetes), but a registered agent
		// without the native capability must be rejected regardless of labels,
		// confirming routing now goes through capabilities.
		claimed, err = pg.ClaimNextRun(t.Context(), "native-agent", nil)
		require.NoError(t, err)
		require.NotNil(t, claimed, "a native-capable agent must claim the native-required webhook run")
	})

	t.Run("kubernetes-only podTemplate requires pod capability, not a kubernetes label", func(t *testing.T) {
		s, pg := newTestServer(t)
		podSpec := `{"agentSelector":["kind:docker"],` +
			`"podTemplate":{"spec":{"containers":[{"name":"job","image":"busybox","command":["sleep","1"]}]}},` +
			`"steps":[{"name":"s","run":"echo x"}]}`
		_, _ = pg.UpsertJob(t.Context(), "podjob-k8sonly-hook", "unified-cd/v1", []byte(podSpec))
		spec, _ := json.Marshal(map[string]any{
			"trigger": map[string]any{"job": "podjob-k8sonly-hook"},
			"auth":    map[string]any{"type": "none"},
		})
		_, _ = pg.UpsertWebhookReceiver(t.Context(), "pod-hook", spec)

		req := httptest.NewRequest(http.MethodPost, "/webhook/pod-hook", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		_, _ = pg.TransitionPendingToQueued(t.Context(), 10)

		require.NoError(t, pg.UpsertAgent(t.Context(), "docker-agent-nocaps", "h1", "linux", "", []string{"kind:docker"}, []string{"native", "container"}, nil))
		require.NoError(t, pg.UpsertAgent(t.Context(), "k8s-agent-caps", "h2", "linux", "", []string{"kind:docker"}, []string{"pod", "container"}, nil))

		claimed, err := pg.ClaimNextRun(t.Context(), "docker-agent-nocaps", []string{"kind:docker"})
		require.NoError(t, err)
		assert.Nil(t, claimed, "an agent without the pod capability must not claim a kubernetes-only podTemplate webhook run")

		claimed, err = pg.ClaimNextRun(t.Context(), "k8s-agent-caps", []string{"kind:docker"})
		require.NoError(t, err)
		require.NotNil(t, claimed, "an agent with the pod capability must claim the kubernetes-only podTemplate webhook run, even without a kubernetes label")
	})
}
