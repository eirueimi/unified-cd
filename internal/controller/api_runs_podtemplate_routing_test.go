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
// workspace.pvc (which the host degrades to a bind mount) is NOT auto-pinned
// to a Kubernetes agent: with agentSelector [kind:docker] the effective
// selector stays {kind:docker}, so a docker agent that does NOT carry the
// "kubernetes" label can claim it, and dsl.RequiredCaps infers "container"
// (not "pod") for it, so a docker agent that only advertises the "container"
// capability (not "pod") can claim it too. Under the old blanket
// "podTemplate ⇒ require kubernetes" label rule the selector was {kind:docker,
// kubernetes} and no docker-only agent could ever claim it; that pin is gone,
// replaced entirely by the required_caps/capabilities match.
func TestAPI_TriggerRun_HostRunnablePodTemplate_NotPinnedToKubernetes(t *testing.T) {
	s, pg := newTestServer(t)
	spec := `{"agentSelector":["kind:docker"],` +
		`"podTemplate":{"workspace":{"mountPath":"/workspace","pvc":{"storageClassName":"standard","storageRequest":"5Gi","accessMode":"ReadWriteOnce"}},` +
		`"spec":{"containers":[{"name":"job","image":"python:3.12-slim"},{"name":"ruff","image":"python:3.12-slim"}]}},` +
		`"steps":[{"name":"s","run":"echo x"}]}`
	_, _ = pg.UpsertJob(t.Context(), "podjob-host", "unified-cd/v1", []byte(spec))

	// Trigger two separate runs of the same job: one claimed by a legacy
	// (never-registered) agent, one by an agent that explicitly advertises
	// capabilities, so each ClaimNextRun call has its own Queued run to pick up.
	triggerAndQueue(t, s, "podjob-host")
	triggerAndQueue(t, s, "podjob-host")
	_, _ = pg.TransitionPendingToQueued(t.Context(), 10)

	// A legacy agent (never registered, so capabilities is NULL) is unaffected
	// by the capability check and is claimable purely on labels, confirming no
	// "kubernetes" label was appended to the run's agentSelector.
	claimed, err := pg.ClaimNextRun(t.Context(), "docker-agent", []string{"kind:docker", "pool:default"})
	require.NoError(t, err)
	require.NotNil(t, claimed, "a docker agent (no kubernetes label) must be able to claim a host-runnable podTemplate job")

	// A registered agent that explicitly advertises "container" but not "pod"
	// must also be able to claim it, confirming RequiredCaps inferred
	// "container" (not "pod") for a host-runnable podTemplate.
	require.NoError(t, pg.UpsertAgent(t.Context(), "docker-agent-caps", "h1", "linux", "", []string{"kind:docker"}, []string{"native", "container"}, nil))
	claimedByCaps, err := pg.ClaimNextRun(t.Context(), "docker-agent-caps", []string{"kind:docker"})
	require.NoError(t, err)
	require.NotNil(t, claimedByCaps, "a docker agent advertising container (not pod) capability must claim a host-runnable podTemplate job")
}

// TestAPI_TriggerRun_KubernetesOnlyPodTemplate_RequiresPodCapability verifies
// the other half: a podTemplate that uses a host-unsupported field (here a
// container command:) makes dsl.RequiredCaps infer "pod". Routing this to
// Kubernetes now happens via the required_caps/capabilities match rather than
// an auto-appended "kubernetes" agentSelector label (that pin was removed): an
// agent that explicitly advertises capabilities without "pod" cannot claim it,
// while an agent advertising "pod" can — even without any "kubernetes" label.
func TestAPI_TriggerRun_KubernetesOnlyPodTemplate_RequiresPodCapability(t *testing.T) {
	s, pg := newTestServer(t)
	spec := `{"agentSelector":["kind:docker"],` +
		`"podTemplate":{"spec":{"containers":[{"name":"job","image":"busybox","command":["sleep","1"]}]}},` +
		`"steps":[{"name":"s","run":"echo x"}]}`
	_, _ = pg.UpsertJob(t.Context(), "podjob-k8sonly", "unified-cd/v1", []byte(spec))

	triggerAndQueue(t, s, "podjob-k8sonly")
	_, _ = pg.TransitionPendingToQueued(t.Context(), 10)

	require.NoError(t, pg.UpsertAgent(t.Context(), "docker-agent", "h1", "linux", "", []string{"kind:docker"}, []string{"native", "container"}, nil))
	require.NoError(t, pg.UpsertAgent(t.Context(), "k8s-agent", "h2", "linux", "", []string{"kind:docker"}, []string{"pod", "container"}, nil))

	claimedDocker, err := pg.ClaimNextRun(t.Context(), "docker-agent", []string{"kind:docker"})
	require.NoError(t, err)
	assert.Nil(t, claimedDocker, "an agent advertising capabilities without \"pod\" must not claim a kubernetes-only podTemplate run")

	claimedK8s, err := pg.ClaimNextRun(t.Context(), "k8s-agent", []string{"kind:docker"})
	require.NoError(t, err)
	require.NotNil(t, claimedK8s, "an agent advertising \"pod\" must claim the run even without a kubernetes label")
}
