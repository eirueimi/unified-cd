package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTriggerRun_PersistsRequiredCaps verifies that handleTriggerRun infers
// dsl.RequiredCaps(spec) and passes it into CreateRun, so a triggered run is
// only claimable by an agent whose advertised capabilities are a superset of
// what the run needs (enforced by ClaimNextRun's `required_caps <@
// capabilities` match, already covered at the store layer). The API surface
// exposes no direct read of required_caps (api.Run does not carry it), so
// each case is verified the same way api_runs_podtemplate_routing_test.go
// verifies agent_selector: by registering agents with specific capabilities
// and observing which ones can/can't claim the resulting run.
func TestTriggerRun_PersistsRequiredCaps(t *testing.T) {
	t.Run("native job requires native", func(t *testing.T) {
		s, pg := newTestServer(t)
		spec := `{"native":true,"steps":[{"name":"s","run":"echo x"}]}`
		_, _ = pg.UpsertJob(t.Context(), "nativejob", "unified-cd/v1", []byte(spec))
		triggerAndQueue(t, s, "nativejob")
		_, _ = pg.TransitionPendingToQueued(t.Context(), 10)

		require.NoError(t, pg.UpsertAgent(t.Context(), "container-only", "h1", "linux", "", []string{}, []string{"container"}, nil))
		require.NoError(t, pg.UpsertAgent(t.Context(), "native-agent", "h2", "linux", "", []string{}, []string{"native"}, nil))

		claimed, err := pg.ClaimNextRun(t.Context(), "container-only", nil)
		require.NoError(t, err)
		assert.Nil(t, claimed, "a container-only agent must not claim a native-required run")

		claimed, err = pg.ClaimNextRun(t.Context(), "native-agent", nil)
		require.NoError(t, err)
		require.NotNil(t, claimed, "a native-capable agent must claim the native-required run")
	})

	t.Run("isolated (no podTemplate) job requires container", func(t *testing.T) {
		s, pg := newTestServer(t)
		spec := `{"steps":[{"name":"s","run":"echo x"}]}`
		_, _ = pg.UpsertJob(t.Context(), "isolatedjob", "unified-cd/v1", []byte(spec))
		triggerAndQueue(t, s, "isolatedjob")
		_, _ = pg.TransitionPendingToQueued(t.Context(), 10)

		require.NoError(t, pg.UpsertAgent(t.Context(), "native-only", "h1", "linux", "", []string{}, []string{"native"}, nil))
		require.NoError(t, pg.UpsertAgent(t.Context(), "container-agent", "h2", "linux", "", []string{}, []string{"native", "container"}, nil))

		claimed, err := pg.ClaimNextRun(t.Context(), "native-only", nil)
		require.NoError(t, err)
		assert.Nil(t, claimed, "a native-only agent must not claim a container-required run")

		claimed, err = pg.ClaimNextRun(t.Context(), "container-agent", nil)
		require.NoError(t, err)
		require.NotNil(t, claimed, "a container-capable agent must claim the container-required run")
	})

	t.Run("host-runnable podTemplate requires container, not pod, and is not pinned to a kubernetes label", func(t *testing.T) {
		s, pg := newTestServer(t)
		spec := `{"agentSelector":["kind:docker"],` +
			`"podTemplate":{"workspace":{"mountPath":"/workspace","pvc":{"storageClassName":"standard","storageRequest":"5Gi","accessMode":"ReadWriteOnce"}},` +
			`"spec":{"containers":[{"name":"job","image":"python:3.12-slim"},{"name":"ruff","image":"python:3.12-slim"}]}},` +
			`"steps":[{"name":"s","run":"echo x"}]}`
		_, _ = pg.UpsertJob(t.Context(), "podjob-host-caps", "unified-cd/v1", []byte(spec))
		triggerAndQueue(t, s, "podjob-host-caps")
		_, _ = pg.TransitionPendingToQueued(t.Context(), 10)

		// A docker agent that carries the job's own agentSelector label but no
		// "kubernetes" label and no "pod" capability must still be able to claim:
		// the old blanket label pin is gone, and the inferred requirement is
		// "container" (satisfied by this agent), not "pod".
		require.NoError(t, pg.UpsertAgent(t.Context(), "docker-agent-caps", "h1", "linux", "", []string{"kind:docker"}, []string{"native", "container"}, nil))

		claimed, err := pg.ClaimNextRun(t.Context(), "docker-agent-caps", []string{"kind:docker"})
		require.NoError(t, err)
		require.NotNil(t, claimed, "a docker agent with container capability (no pod cap, no kubernetes label) must claim a host-runnable podTemplate run")
	})

	t.Run("kubernetes-only podTemplate requires pod", func(t *testing.T) {
		s, pg := newTestServer(t)
		spec := `{"agentSelector":["kind:docker"],` +
			`"podTemplate":{"spec":{"containers":[{"name":"job","image":"busybox","command":["sleep","1"]}]}},` +
			`"steps":[{"name":"s","run":"echo x"}]}`
		_, _ = pg.UpsertJob(t.Context(), "podjob-k8sonly-caps", "unified-cd/v1", []byte(spec))
		triggerAndQueue(t, s, "podjob-k8sonly-caps")
		_, _ = pg.TransitionPendingToQueued(t.Context(), 10)

		require.NoError(t, pg.UpsertAgent(t.Context(), "docker-agent-nocaps", "h1", "linux", "", []string{"kind:docker"}, []string{"native", "container"}, nil))
		require.NoError(t, pg.UpsertAgent(t.Context(), "k8s-agent-caps", "h2", "linux", "", []string{"kind:docker"}, []string{"pod", "container"}, nil))

		claimed, err := pg.ClaimNextRun(t.Context(), "docker-agent-nocaps", []string{"kind:docker"})
		require.NoError(t, err)
		assert.Nil(t, claimed, "an agent without the pod capability must not claim a kubernetes-only podTemplate run")

		claimed, err = pg.ClaimNextRun(t.Context(), "k8s-agent-caps", []string{"kind:docker"})
		require.NoError(t, err)
		require.NotNil(t, claimed, "an agent with the pod capability must claim the kubernetes-only podTemplate run")
	})
}
