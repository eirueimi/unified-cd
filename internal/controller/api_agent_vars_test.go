package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Precedence: a job's spec.vars beats a global of the same name, and the
// global keys the job does not mention still arrive.
//
// The global half of the merge (across several Vars manifests, in ListVars's
// sorted order) happens in the claim handler; buildClaimResponse receives that
// already-merged map and overlays the job's own spec.vars on top, which is the
// nearest-scope-wins half asserted here.
func TestBuildClaimResponse_VarsPrecedence(t *testing.T) {
	spec := dsl.Spec{
		Vars:  map[string]string{"SHARED": "job", "APP_NAME": "myapp"},
		Steps: []dsl.StepEntry{{Name: "s", Run: "true"}},
	}
	b, err := json.Marshal(spec)
	require.NoError(t, err)

	globals := map[string]string{"REGISTRY": "global", "SHARED": "global"}
	resp, err := buildClaimResponse(&store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "j"}, Spec: b}, globals)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{
		"REGISTRY": "global",
		"SHARED":   "job",
		"APP_NAME": "myapp",
	}, resp.Vars)

	// The caller's map must not be mutated by the overlay: the handler builds
	// it once per claim, but a shared map would be a nasty aliasing bug.
	assert.Equal(t, map[string]string{"REGISTRY": "global", "SHARED": "global"}, globals)
}

// A job with no vars and no global manifests gets an empty map, not nil-shaped
// JSON the agent has to special-case.
func TestBuildClaimResponse_NoVars(t *testing.T) {
	spec := dsl.Spec{Steps: []dsl.StepEntry{{Name: "s", Run: "true"}}}
	b, err := json.Marshal(spec)
	require.NoError(t, err)

	resp, err := buildClaimResponse(&store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "j"}, Spec: b}, nil)
	require.NoError(t, err)
	require.NotNil(t, resp.Vars)
	assert.Empty(t, resp.Vars)
}

// A global var reaches a run whose job declares no vars of its own.
func TestBuildClaimResponse_GlobalVarsOnly(t *testing.T) {
	spec := dsl.Spec{Steps: []dsl.StepEntry{{Name: "s", Run: "true"}}}
	b, err := json.Marshal(spec)
	require.NoError(t, err)

	resp, err := buildClaimResponse(
		&store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "j"}, Spec: b},
		map[string]string{"REGISTRY": "ghcr.io/myorg"},
	)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"REGISTRY": "ghcr.io/myorg"}, resp.Vars)
}

// End to end through the claim endpoint: the handler loads every stored Vars
// manifest, flattens them in ListVars's sorted order, and the claimed run
// carries the result with the job's own spec.vars on top.
func TestAgentAPI_Claim_MergesStoredVars(t *testing.T) {
	s, pg := newTestServer(t)

	// Two globals both defining COLLIDE: ListVars sorts by name, so the later
	// name wins deterministically rather than by database order. (The
	// apply-time collision check should have prevented this pair ever being
	// stored; this pins what happens if one slips through.)
	_, err := pg.UpsertVars(t.Context(), "a-org-defaults",
		[]byte(`{"vars":{"REGISTRY":"global","SHARED":"global","COLLIDE":"from-a"}}`))
	require.NoError(t, err)
	_, err = pg.UpsertVars(t.Context(), "b-team-defaults",
		[]byte(`{"vars":{"COLLIDE":"from-b"}}`))
	require.NoError(t, err)

	spec := []byte(`{"vars":{"SHARED":"job","APP_NAME":"myapp"},"steps":[{"name":"s","run":"echo x"}]}`)
	_, err = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", spec)
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "j", nil, spec, nil, nil, "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(t.Context(), 10)
	require.NoError(t, err)

	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=2s", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var got api.ClaimResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, run.ID, got.RunID)
	assert.Equal(t, map[string]string{
		"REGISTRY": "global",
		"SHARED":   "job",
		"APP_NAME": "myapp",
		"COLLIDE":  "from-b",
	}, got.Vars)
}
