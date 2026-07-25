package k8sagent

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestK8sBackend_EnsureScope_HonorsExpandedEnv locks in the fix for the
// regression where k8sBackend.EnsureScope (via ensureScopePod) ignored its
// env parameter (the orchestrator's already-template-expanded "KEY=VALUE"
// pairs, e.g. internal/agent/orchestrator.go's extraEnv) and instead baked
// the raw, unexpanded step.Env map straight into the scope pod's container
// spec. A scope env value containing a template (e.g. {{ .Params.x }})
// therefore shipped to the scope pod as the literal template string instead
// of its resolved value — the same class of expanded-env bug fixed in
// 9e09c76. This drives the REAL k8sBackend.EnsureScope (not the parity fake,
// whose fake podManager bypasses ensureScopePod's env-merge logic entirely)
// with a fake podManager that records the created pod spec.
func TestK8sBackend_EnsureScope_HonorsExpandedEnv(t *testing.T) {
	pm := &fakePM{}
	a := &K8sAgent{cfg: Config{Namespace: "default"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	step := api.ClaimStep{
		ScopeID:    "scope:build",
		ScopeImage: "golang:1.22",
		Env:        map[string]string{"TOKEN": "{{ .Params.x }}"},
	}

	env := []string{"TOKEN=resolved-value"}

	_, err := b.EnsureScope(context.Background(), step, env)
	require.NoError(t, err)

	require.NotNil(t, pm.created)
	require.Len(t, pm.created.Spec.Containers, 1)

	gotEnv := map[string]string{}
	for _, e := range pm.created.Spec.Containers[0].Env {
		gotEnv[e.Name] = e.Value
	}

	assert.Equal(t, "resolved-value", gotEnv["TOKEN"], "scope pod env must carry the orchestrator's expanded value")
	assert.NotEqual(t, "{{ .Params.x }}", gotEnv["TOKEN"], "scope pod env must NOT carry the raw, unexpanded template string")
}

// TestK8sBackend_EnsureScope_KeepsK8sDefaultsWhenNoOverride verifies
// imageStepEnv's k8s-specific defaults (e.g. UNIFIED_AGENT_OS) still land in
// the scope pod's env when the orchestrator's env slice doesn't override
// them, following the env-merge semantics (env param wins, else fall back
// to imageStepEnv(step)).
func TestK8sBackend_EnsureScope_KeepsK8sDefaultsWhenNoOverride(t *testing.T) {
	pm := &fakePM{}
	a := &K8sAgent{cfg: Config{Namespace: "default"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	step := api.ClaimStep{
		ScopeID:    "scope:build",
		ScopeImage: "golang:1.22",
	}

	_, err := b.EnsureScope(context.Background(), step, nil)
	require.NoError(t, err)

	require.NotNil(t, pm.created)
	require.Len(t, pm.created.Spec.Containers, 1)

	gotEnv := map[string]string{}
	for _, e := range pm.created.Spec.Containers[0].Env {
		gotEnv[e.Name] = e.Value
	}
	assert.Equal(t, "linux", gotEnv["UNIFIED_AGENT_OS"], "imageStepEnv's default must survive when env has no override")
}

// TestK8sBackend_EnsureScopePod_UsesConfiguredPodStartTimeout proves the
// scope-image pod wait (ensureScopePod, backend.go) is bounded by the SAME
// configurable knob as the run pod (Config.PodStartTimeout /
// UNIFIED_K8S_POD_START_TIMEOUT, resolved via Config.PodStartTimeoutDuration),
// not the old hardcoded imagePodStartTimeout constant (5 * time.Minute). A
// distinctive, short configured value must reach the wait context's deadline;
// if the scope-pod path were still hardcoded to 5 minutes, the observed
// deadline would land far outside the tolerance window asserted below.
func TestK8sBackend_EnsureScopePod_UsesConfiguredPodStartTimeout(t *testing.T) {
	const configured = 1500 * time.Millisecond
	pm := &fakePM{}
	a := &K8sAgent{cfg: Config{Namespace: "default", PodStartTimeout: "1500ms"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	step := api.ClaimStep{
		ScopeID:    "scope:build",
		ScopeImage: "golang:1.22",
	}

	start := time.Now()
	_, err := b.EnsureScope(context.Background(), step, nil)
	require.NoError(t, err)

	require.True(t, pm.waitHadDeadline, "scope pod wait must carry a deadline")
	gotTimeout := pm.waitDeadline.Sub(start)

	// Generous tolerance for test scheduling jitter, but tight enough to
	// clearly distinguish "used the configured 1.5s" from "fell back to the
	// hardcoded 5-minute default".
	assert.InDelta(t, configured.Seconds(), gotTimeout.Seconds(), 5.0,
		"scope pod wait deadline must reflect the configured PodStartTimeout (~1.5s), got %s (would be ~5m if still hardcoded)", gotTimeout)
	assert.Less(t, gotTimeout, time.Minute, "scope pod wait must NOT use the old hardcoded 5-minute default when PodStartTimeout is configured short")
}
