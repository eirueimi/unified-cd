package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
	run, err := pg.CreateRun(t.Context(), "j", nil, spec, nil, nil, "", "")
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

// varsListErrorStore makes ListVars fail the way a database blip does — the
// store call itself erroring — for the first failures calls, then delegates to
// the real store like every other method. It is the only way to exercise the
// TRANSIENT half of the claim handler's classification: the deterministic half
// is reachable through a stored row, this one is not reachable at all without
// injection.
type varsListErrorStore struct {
	store.Store
	remaining atomic.Int32
	calls     atomic.Int32
}

func (s *varsListErrorStore) ListVars(ctx context.Context) ([]store.VarsRecord, error) {
	s.calls.Add(1)
	if s.remaining.Add(-1) >= 0 {
		return nil, errors.New("read tcp 127.0.0.1:5432: connection reset by peer")
	}
	return s.Store.ListVars(ctx)
}

// TRANSIENT: the store call itself failed, so retrying can succeed. The run
// must go back on the queue — not stranded Running (a 500 would leave it there
// with a live heartbeating agent, which ListStuckRuns never reaps) and not
// Failed, which would punish a user's run for a database blip in the
// milliseconds since the claim.
func TestAgentAPI_Claim_RequeuesRunWhenVarsListErrors(t *testing.T) {
	s, pg := newTestServer(t)

	_, err := pg.UpsertVars(t.Context(), "org-defaults", []byte(`{"vars":{"REGISTRY":"ghcr.io/myorg"}}`))
	require.NoError(t, err)

	spec := []byte(`{"steps":[{"name":"s","run":"echo x"}]}`)
	_, err = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", spec)
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "j", nil, spec, nil, nil, "", "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(t.Context(), 10)
	require.NoError(t, err)

	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	// Fail exactly the first ListVars, so the first claim hits the transient
	// path and the second proves the run really is claimable again.
	failing := &varsListErrorStore{Store: pg}
	failing.remaining.Store(1)
	s.store = failing

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=200ms", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got api.ClaimResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Empty(t, got.RunID, "the agent must not be handed a claim assembled without its vars")
	require.Positive(t, failing.calls.Load(), "the test never exercised the injected failure")

	after, err := pg.GetRun(t.Context(), run.ID)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, api.RunQueued, after.Status,
		"a transient vars-load failure must requeue the run, not strand it Running or fail it")
	assert.Empty(t, after.ClaimedBy, "a requeued run must be claimable by any agent again")

	// And it is genuinely claimable again, with everything it needs.
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=2s", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	rec2 := httptest.NewRecorder()
	s.Router().ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

	var got2 api.ClaimResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &got2))
	assert.Equal(t, run.ID, got2.RunID, "the requeued run must be claimable again")
	assert.Equal(t, map[string]string{"REGISTRY": "ghcr.io/myorg"}, got2.Vars)
	require.Len(t, got2.Stages, 1)
	require.NotNil(t, got2.Stages[0].Step)
	assert.Equal(t, "echo x", got2.Stages[0].Step.Run, "the requeued run kept its spec")
}

// DETERMINISTIC: a stored manifest that will not decode fails the same way on
// every claim, and mergedGlobalVars reads every manifest on every claim — so
// requeueing would put the WHOLE FLEET into a silent retry loop where no run
// ever fails and nothing looks broken. One run must fail loudly instead, with
// the offending manifest named in its own log.
//
// The row is manufactured here (valid JSON, wrong shape) because the real write
// paths marshal a validated dsl.VarsSpec and cannot produce it — a migration, a
// manual database edit or a future writer could.
func TestAgentAPI_Claim_FailsRunWhenAVarsManifestIsCorrupt(t *testing.T) {
	s, pg := newTestServer(t)

	_, err := pg.UpsertVars(t.Context(), "corrupt-defaults", []byte(`{"vars":"not-a-map"}`))
	require.NoError(t, err)

	spec := []byte(`{"steps":[{"name":"s","run":"echo x"}]}`)
	_, err = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", spec)
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "j", nil, spec, nil, nil, "", "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(t.Context(), 10)
	require.NoError(t, err)

	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=200ms", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got api.ClaimResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Empty(t, got.RunID, "claim response must be empty so the agent just keeps polling")

	after, err := pg.GetRun(t.Context(), run.ID)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, api.RunFailed, after.Status,
		"a corrupt stored manifest is deterministic: fail this run visibly rather than requeue every claim in the fleet forever")

	lines, err := pg.TailLogs(t.Context(), run.ID, 0, 10)
	require.NoError(t, err)
	require.NotEmpty(t, lines, "the failure reason must be on the run, not only in the controller log")
	var joined string
	for _, l := range lines {
		joined += l.Line + "\n"
	}
	assert.Contains(t, joined, "corrupt-defaults",
		"the message must name the offending manifest so an operator can find the row")
}

func claimWithImages(t *testing.T, spec dsl.Spec, params map[string]string, globals map[string]string) (api.ClaimResponse, error) {
	t.Helper()
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	return buildClaimResponse(&store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "j", Params: params}, Spec: b}, globals)
}

// Image fields are expanded at claim time from the merged vars (job spec.vars
// over globals) and the run's params, in podTemplate containers,
// initContainers, override containers and every scope image (steps, parallel
// sub-steps and finally).
func TestBuildClaimResponse_ExpandsImages(t *testing.T) {
	spec := dsl.Spec{
		Vars: map[string]string{"GO_IMAGE": "golang:job"},
		PodTemplate: &dsl.PodTemplate{
			Spec: map[string]any{
				"containers":     []any{map[string]any{"name": "build", "image": "{{ .Vars.GO_IMAGE }}"}},
				"initContainers": []any{map[string]any{"name": "init", "image": "{{ .Vars.REG }}/init:{{ .Params.TAG }}"}},
			},
			Override: &dsl.PodSpecPatch{Containers: []map[string]any{{"name": "side", "image": "{{ .Vars.REG }}/side"}}},
		},
		Steps: []dsl.StepEntry{
			{Name: "u", ScopeID: "s1", ScopeImage: "{{ .Vars.REG }}/tool:{{ .Params.TAG }}", Run: "true"},
			{Parallel: []dsl.Step{{Name: "p", ScopeID: "s2", ScopeImage: "{{ .Vars.GO_IMAGE }}", Run: "true"}}},
		},
		Finally: []dsl.StepEntry{{Name: "f", ScopeID: "s3", ScopeImage: "plain:1", Run: "true"}},
	}
	globals := map[string]string{"GO_IMAGE": "golang:global", "REG": "reg.io"}
	resp, err := claimWithImages(t, spec, map[string]string{"TAG": "v1"}, globals)
	require.NoError(t, err)

	cs := resp.PodTemplate.Spec["containers"].([]any)
	assert.Equal(t, "golang:job", cs[0].(map[string]any)["image"])
	assert.Equal(t, "reg.io/init:v1", resp.PodTemplate.Spec["initContainers"].([]any)[0].(map[string]any)["image"])
	assert.Equal(t, "reg.io/side", resp.PodTemplate.Override.Containers[0]["image"])
	assert.Equal(t, "reg.io/tool:v1", resp.Stages[0].Step.ScopeImage)
	assert.Equal(t, "golang:job", resp.Stages[1].Parallel[0].ScopeImage)
	assert.Equal(t, "plain:1", resp.Finally[0].Step.ScopeImage)
	assert.Equal(t, map[string]string{"GO_IMAGE": "golang:global", "REG": "reg.io"}, globals)
}

// The spec is parsed fresh per claim, so nothing the caller holds is mutated;
// this pins that the expansion works on copies by claiming the same bytes twice
// with different vars.
func TestBuildClaimResponse_ExpandsImagesWithoutLeaking(t *testing.T) {
	spec := dsl.Spec{
		PodTemplate: &dsl.PodTemplate{Spec: map[string]any{
			"containers": []any{map[string]any{"name": "build", "image": "{{ .Vars.I }}"}},
		}},
		Steps: []dsl.StepEntry{{Name: "s", Run: "true"}},
	}
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	c := &store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "j"}, Spec: b}
	r1, err := buildClaimResponse(c, map[string]string{"I": "a:1"})
	require.NoError(t, err)
	r2, err := buildClaimResponse(c, map[string]string{"I": "b:2"})
	require.NoError(t, err)
	assert.Equal(t, "a:1", r1.PodTemplate.Spec["containers"].([]any)[0].(map[string]any)["image"])
	assert.Equal(t, "b:2", r2.PodTemplate.Spec["containers"].([]any)[0].(map[string]any)["image"])
	assert.Equal(t, "{{ .Vars.I }}", spec.PodTemplate.Spec["containers"].([]any)[0].(map[string]any)["image"])
}

func TestBuildClaimResponse_ImageErrors(t *testing.T) {
	pod := dsl.Spec{
		PodTemplate: &dsl.PodTemplate{Spec: map[string]any{
			"containers": []any{map[string]any{"name": "build", "image": "{{ .Vars.GO_IMAGE }}"}},
		}},
		Steps: []dsl.StepEntry{{Name: "s", Run: "true"}},
	}
	_, err := claimWithImages(t, pod, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `podTemplate container "build" image "{{ .Vars.GO_IMAGE }}"`)
	assert.Contains(t, err.Error(), "GO_IMAGE")

	scope := dsl.Spec{Steps: []dsl.StepEntry{{Name: "u", ScopeID: "s1", ScopeImage: "{{ .Secrets.X }}", Run: "true"}}}
	_, err = claimWithImages(t, scope, nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `step "u"`)
}

// Through the claim endpoint an undefined var fails the run deterministically
// with the reason in its log, instead of requeueing it.
func TestAgentAPI_Claim_FailsRunWhenImageVarUndefined(t *testing.T) {
	s, pg := newTestServer(t)

	spec := []byte(`{"podTemplate":{"spec":{"containers":[{"name":"build","image":"{{ .Vars.NOPE }}"}]}},"steps":[{"name":"s","run":"echo x"}]}`)
	_, err := pg.UpsertJob(t.Context(), "j", "unified-cd/v1", spec)
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "j", nil, spec, nil, nil, "", "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(t.Context(), 10)
	require.NoError(t, err)

	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=200ms", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got api.ClaimResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Empty(t, got.RunID)

	after, err := pg.GetRun(t.Context(), run.ID)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, api.RunFailed, after.Status)

	lines, err := pg.TailLogs(t.Context(), run.ID, 0, 10)
	require.NoError(t, err)
	var joined string
	for _, l := range lines {
		joined += l.Line + "\n"
	}
	assert.Contains(t, joined, "NOPE")
}
