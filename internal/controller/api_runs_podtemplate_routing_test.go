package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// triggerAndQueue triggers a run for jobName and transitions it to Queued so it
// is claimable, returning nothing (the test then probes ClaimNextRun).
func triggerAndQueue(t *testing.T, s *Server, jobName string) {
	t.Helper()
	body, _ := json.Marshal(api.TriggerRunRequest{JobName: jobName})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// TestAPI_TriggerRun_HostRunnablePodTemplate_NotPinnedToKubernetes verifies that
// a podTemplate using only host-honored container fields (name/image) plus a
// workspace.pvc (which the host claim pod degrades to a bind mount) is NOT
// auto-pinned to a Kubernetes agent: with agentSelector [kind:docker] the
// effective selector stays {kind:docker}, so a docker agent that does NOT carry
// the "kubernetes" label can claim it. Under the old blanket
// "podTemplate ⇒ require kubernetes" rule the selector was {kind:docker,
// kubernetes} and no docker-only agent could ever claim it.
func TestAPI_TriggerRun_HostRunnablePodTemplate_NotPinnedToKubernetes(t *testing.T) {
	s, pg := newTestServer(t)
	spec := `{"agentSelector":["kind:docker"],` +
		`"podTemplate":{"workspace":{"mountPath":"/workspace","pvc":{"storageClassName":"standard","storageRequest":"5Gi","accessMode":"ReadWriteOnce"}},` +
		`"spec":{"containers":[{"name":"job","image":"python:3.12-slim"},{"name":"ruff","image":"python:3.12-slim"}]}},` +
		`"steps":[{"name":"s","run":"echo x"}]}`
	_, _ = pg.UpsertJob(t.Context(), "podjob-host", "unified-cd/v1", []byte(spec))

	triggerAndQueue(t, s, "podjob-host")
	_, _ = pg.TransitionPendingToQueued(t.Context(), 10)

	claimed, err := pg.ClaimNextRun(t.Context(), "docker-agent", []string{"kind:docker", "pool:default"})
	require.NoError(t, err)
	require.NotNil(t, claimed, "a docker agent (no kubernetes label) must be able to claim a host-runnable podTemplate job")
}

// TestAPI_TriggerRun_KubernetesOnlyPodTemplate_PinnedToKubernetes verifies the
// other half: a podTemplate that uses a host-unsupported field (here a
// container command:) is still auto-pinned to Kubernetes. A docker-only agent
// cannot claim it, but a kubernetes-labelled agent can.
func TestAPI_TriggerRun_KubernetesOnlyPodTemplate_PinnedToKubernetes(t *testing.T) {
	s, pg := newTestServer(t)
	spec := `{"agentSelector":["kind:docker"],` +
		`"podTemplate":{"spec":{"containers":[{"name":"job","image":"busybox","command":["sleep","1"]}]}},` +
		`"steps":[{"name":"s","run":"echo x"}]}`
	_, _ = pg.UpsertJob(t.Context(), "podjob-k8sonly", "unified-cd/v1", []byte(spec))

	triggerAndQueue(t, s, "podjob-k8sonly")
	_, _ = pg.TransitionPendingToQueued(t.Context(), 10)

	claimedDocker, err := pg.ClaimNextRun(t.Context(), "docker-agent", []string{"kind:docker"})
	require.NoError(t, err)
	assert.Nil(t, claimedDocker, "a k8s-only podTemplate must not be claimable by a docker-only agent")

	claimedK8s, err := pg.ClaimNextRun(t.Context(), "k8s-agent", []string{"kind:docker", "kubernetes"})
	require.NoError(t, err)
	require.NotNil(t, claimedK8s, "an agent carrying both kind:docker and kubernetes must claim the pinned run")
}
