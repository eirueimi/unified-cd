package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runVolumeClaim executes a single-step native claim that mounts the given
// volumes and returns what the step logged. The manager is returned so a test
// can look inside the volume directories.
func runVolumeClaim(t *testing.T, name string, vars map[string]string, script string, mounts ...dsl.VolumeMount) (string, *VolumeManager) {
	t.Helper()
	agentID := "volenv-agent-" + name
	h := &varsTestHarness{}
	srv := newVarsTestServer(t, agentID, h)

	m, _ := newTestVolumes(t, exclusive("go-cache"), exclusive("go-cache-2"))
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok"), Volumes: m}

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   "run-" + name,
		JobName: name,
		Vars:    vars,
		Stages:  []api.ClaimStage{{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "show", Run: script}}},
	}
	claim.Volumes = mounts
	a.executeRun(context.Background(), claim, t.TempDir())
	return h.output(), m
}

// The point of UNIFIED_VOLUME_<ALIAS>: a step reaches the volume by an
// absolute path whatever mountPath the job wrote, and the name is fixed by the
// alias, not by the volume the run happened to be handed.
func TestOrchestrator_ExposesVolumePathToSteps(t *testing.T) {
	out, m := runVolumeClaim(t, "expose", nil,
		`echo "set=[${UNIFIED_VOLUME_GO_CACHE:+yes}]"; mkdir -p "$UNIFIED_VOLUME_GO_CACHE/build" && echo hi > "$UNIFIED_VOLUME_GO_CACHE/build/probe"`,
		vmount("go-cache", "go-cache", ".ucd-cache/go"))

	assert.Contains(t, out, "set=[yes]", "UNIFIED_VOLUME_GO_CACHE must reach the step")
	data, err := os.ReadFile(filepath.Join(m.PathOf("go-cache"), "build", "probe"))
	require.NoError(t, err, "a write through UNIFIED_VOLUME_GO_CACHE must land in the volume's own directory")
	assert.Contains(t, string(data), "hi")
}

// The variable is named after the alias even when the volume has a different
// name: a job cannot predict which of its candidates it gets.
func TestOrchestrator_VolumeVariableIsNamedByAliasNotVolume(t *testing.T) {
	out, _ := runVolumeClaim(t, "alias", nil,
		`echo "alias=[${UNIFIED_VOLUME_CACHE:+yes}] byname=[${UNIFIED_VOLUME_GO_CACHE_2:+yes}]"`,
		vmount("go-cache-2", "cache", ".ucd-cache/go"))

	assert.Contains(t, out, "alias=[yes]")
	assert.Contains(t, out, "byname=[]", "the resolved volume name must not leak into the variable name")
}

// A global Vars manifest cannot shadow a volume's path: the variables are
// appended after the synthesised entries and a later duplicate wins in
// os/exec, so without the prefix rule this would silently redirect every
// job's cache. The apply-time half is TestReservedVarNames_CoversVolumePrefix.
func TestOrchestrator_VarsCannotShadowVolumeVariables(t *testing.T) {
	out, m := runVolumeClaim(t, "shadow",
		map[string]string{
			"UNIFIED_VOLUME_GO_CACHE": "/tmp/attacker-scratch",
			"unified_volume_other":    "/tmp/attacker-other",
			"REGISTRY":                "ghcr.io/myorg",
		},
		`echo "gc=[$UNIFIED_VOLUME_GO_CACHE] other=[$UNIFIED_VOLUME_OTHER] registry=[$REGISTRY]"`,
		vmount("go-cache", "go-cache", ".ucd-cache/go"))

	assert.Contains(t, out, "registry=[ghcr.io/myorg]", "an ordinary var must still reach the step")
	assert.NotContains(t, out, "attacker", "a var in the UNIFIED_VOLUME_ namespace must not reach a step")
	assert.Contains(t, out, "other=[]", "a volume variable nobody declared is not settable either")
	assert.NotContains(t, out, "gc=[]", "the orchestrator's own value must survive")
	require.NotEmpty(t, m.PathOf("go-cache"))
}

// A step that runs in its own scope container does not have the volumes
// mounted, so it must not be handed a path that does not exist there.
func TestHostBackend_VolumePathsAreNotOfferedToScopedSteps(t *testing.T) {
	b := newHostBackend(&Agent{}, "r1", "job", t.TempDir(), nil)
	b.volumePaths = map[string]string{"go-cache": "/x"}

	assert.Equal(t, map[string]string{"go-cache": "/x"}, b.VolumePaths(ScopeHandle{}))
	assert.Empty(t, b.VolumePaths(NewScopeHandle("scope-1")))
}
