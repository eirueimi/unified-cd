package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func volAgent(volumes ...string) api.AgentInfo {
	return api.AgentInfo{Capabilities: []string{"native", "container"}, Volumes: volumes}
}

func volSpec(names ...[]string) dsl.Spec {
	spec := dsl.Spec{Native: true}
	for i, n := range names {
		spec.Volumes = append(spec.Volumes, dsl.VolumeMount{
			Name:      dsl.StringOrSlice(n),
			As:        string(rune('a' + i)),
			MountPath: "/mnt/" + string(rune('a'+i)),
		})
	}
	return spec
}

// An unknown volume is deliberately not an apply-time error: the controller
// holds no volume state, so the only honest answer is an unschedulable run
// with a reason that names the volume.
func TestSchedulability_UnknownVolume(t *testing.T) {
	s := EvaluateSchedulability(volSpec([]string{"unity-main"}), []api.AgentInfo{volAgent("cache"), volAgent()})
	assert.False(t, s.Satisfiable)
	assert.Contains(t, s.Reason, "unity-main")
	assert.Contains(t, s.Reason, "no agent declares")
	assert.Equal(t, [][]string{{"unity-main"}}, s.RequiredVolumes)
}

func TestSchedulability_VolumeAdvertised(t *testing.T) {
	s := EvaluateSchedulability(volSpec([]string{"unity-main"}), []api.AgentInfo{volAgent("unity-main")})
	assert.True(t, s.Satisfiable, s.Reason)
	assert.Empty(t, s.Reason)
}

// A candidate set is an OR: any one advertised candidate satisfies it.
func TestSchedulability_CandidateSetSatisfiedByAnyCandidate(t *testing.T) {
	s := EvaluateSchedulability(volSpec([]string{"unity-a", "unity-b"}), []api.AgentInfo{volAgent("unity-b")})
	assert.True(t, s.Satisfiable, s.Reason)

	s = EvaluateSchedulability(volSpec([]string{"unity-a", "unity-b"}), []api.AgentInfo{volAgent("unity-c")})
	assert.False(t, s.Satisfiable)
	assert.Contains(t, s.Reason, "unity-a")
	assert.Contains(t, s.Reason, "unity-b")
	assert.Contains(t, s.Reason, "no agent declares")
}

// The entries are an AND: every set must be satisfied, and by the same agent,
// because a run is claimed by exactly one agent.
func TestSchedulability_EveryVolumeSetMustBeSatisfied(t *testing.T) {
	spec := volSpec([]string{"unity-main"}, []string{"build-cache"})

	// One agent has both.
	s := EvaluateSchedulability(spec, []api.AgentInfo{volAgent("unity-main", "build-cache")})
	assert.True(t, s.Satisfiable, s.Reason)

	// One set is advertised nowhere: the reason names the missing one only.
	s = EvaluateSchedulability(spec, []api.AgentInfo{volAgent("unity-main")})
	assert.False(t, s.Satisfiable)
	assert.Contains(t, s.Reason, "build-cache")
	assert.NotContains(t, s.Reason, "unity-main")

	// Each set is advertised somewhere, but never together on one agent.
	s = EvaluateSchedulability(spec, []api.AgentInfo{volAgent("unity-main"), volAgent("build-cache")})
	assert.False(t, s.Satisfiable)
	assert.Contains(t, s.Reason, "no single agent")
}

// A volume name that is a template resolves only at queue time, so it cannot
// be judged here. Like a templated selector, it is flagged rather than warned.
func TestSchedulability_TemplatedVolumeNameIsDeferred(t *testing.T) {
	s := EvaluateSchedulability(volSpec([]string{"unity-{{ .Params.LIB }}"}), []api.AgentInfo{volAgent()})
	assert.True(t, s.Satisfiable, s.Reason)
	assert.True(t, s.VolumesDependOnParams)
}

// The endpoint is what a user actually sees: a stored job naming a volume that
// no registered agent advertises reports the reason, and the same job becomes
// schedulable once an agent advertises it.
func TestAPI_GetJobSchedulability_UnknownVolume(t *testing.T) {
	s, pg := newTestServer(t)
	require.NoError(t, pg.UpsertAgent(t.Context(), "ag1", "host1", "linux", "dev", nil, []string{"native", "container"}, []string{"cache"}, nil))
	specJSON := []byte(`{"native":true,"volumes":[{"name":["unity-main"],"as":"unity-main","mountPath":"."}],"steps":[{"name":"build","run":"echo hi"}]}`)
	_, err := pg.UpsertJob(t.Context(), "build", "unified-cd/v1", specJSON)
	require.NoError(t, err)

	get := func() Schedulability {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/build/schedulability", nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got Schedulability
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		return got
	}

	got := get()
	assert.False(t, got.Satisfiable)
	assert.Contains(t, got.Reason, "no agent declares")
	assert.Contains(t, got.Reason, "unity-main")
	assert.Equal(t, [][]string{{"unity-main"}}, got.RequiredVolumes)

	require.NoError(t, pg.UpsertAgent(t.Context(), "ag2", "host2", "linux", "dev", nil, []string{"native", "container"}, []string{"unity-main"}, nil))
	got = get()
	assert.True(t, got.Satisfiable, got.Reason)
}

// A job with no volumes is unaffected.
func TestSchedulability_NoVolumes(t *testing.T) {
	s := EvaluateSchedulability(dsl.Spec{Native: true}, []api.AgentInfo{volAgent()})
	assert.True(t, s.Satisfiable)
	assert.Empty(t, s.RequiredVolumes)
	assert.False(t, s.VolumesDependOnParams)
}
