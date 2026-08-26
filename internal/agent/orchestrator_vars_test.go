package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// varsExtraEnv is the helper Step 4 introduces; these tests define its
// contract before it exists.

// A variable named after an agent credential must never reach a step, even
// though extraEnv is otherwise exempt from stepEnvDenied. Apply-time
// validation refuses these, but a run created BEFORE that validation existed
// can still carry one, and this is the backstop for it.
func TestVarsExtraEnv_DropsDeniedNames(t *testing.T) {
	got := varsExtraEnv(map[string]string{
		"REGISTRY":          "ghcr.io/myorg",
		"UNIFIED_TOKEN":     "stolen",
		"UNIFIED_CACHE_KEY": "stolen",
	})
	assert.Contains(t, got, "REGISTRY=ghcr.io/myorg")
	for _, e := range got {
		assert.NotContains(t, e, "UNIFIED_TOKEN=")
		assert.NotContains(t, e, "UNIFIED_CACHE_KEY=")
	}
}

// The output is sorted, so a step's environment does not change from run to
// run for the same inputs — map iteration order would otherwise make it churn.
func TestVarsExtraEnv_Sorted(t *testing.T) {
	got := varsExtraEnv(map[string]string{"B": "2", "A": "1", "C": "3"})
	require.Equal(t, []string{"A=1", "B=2", "C=3"}, got)
}

func TestVarsExtraEnv_Empty(t *testing.T) {
	assert.Empty(t, varsExtraEnv(nil))
	assert.Empty(t, varsExtraEnv(map[string]string{}))
}

// varsTestHarness is a fake controller server serving the endpoints
// executeRun calls for a single-stage native claim, recording every shipped
// log line so a step's own echo can be inspected. Mirrors the compact fake
// used by agent_stdout_stream_test.go, generalized over the step index via
// Go 1.22 {wildcard} mux patterns.
type varsTestHarness struct {
	mu    sync.Mutex
	lines []string
}

func (h *varsTestHarness) record(line string) {
	if line == "" {
		return
	}
	h.mu.Lock()
	h.lines = append(h.lines, line)
	h.mu.Unlock()
}

// output returns every shipped log line joined by newlines.
func (h *varsTestHarness) output() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.lines, "\n")
}

func newVarsTestServer(t *testing.T, agentID string, h *varsTestHarness) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, r *http.Request) {
		var req api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.record(req.Line)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/steps/{idx}/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		var reqs []api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&reqs)
		for _, l := range reqs {
			h.record(l.Line)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/finish", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// runVarsClaim executes a single-step native claim carrying vars and returns
// everything the step logged.
func runVarsClaim(t *testing.T, name string, vars map[string]string, stepEnv map[string]string, script string) string {
	t.Helper()
	agentID := "vars-agent-" + name
	h := &varsTestHarness{}
	srv := newVarsTestServer(t, agentID, h)

	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}
	claim := api.ClaimResponse{
		Native:  true,
		RunID:   "run-" + name,
		JobName: name,
		Vars:    vars,
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index:      0,
				StageIndex: 0,
				Name:       "show",
				Env:        stepEnv,
				Run:        script,
			}},
		},
	}
	a.executeRun(context.Background(), claim, t.TempDir())
	return h.output()
}

// Precedence end to end: step env beats job vars beats global vars. This is
// the ordering in extraEnv, so it is worth asserting through the real path
// rather than only on varsExtraEnv in isolation.
//
// The claim's Vars is already the merged map (globals overlaid by the job's
// spec.vars — see buildClaimResponse and its
// TestBuildClaimResponse_VarsPrecedence), so the global-beaten-by-job half is
// represented here by SHARED arriving with the job's value; the half this
// test owns is the step's own env: beating the merged var of the same name.
func TestOrchestrator_VarsPrecedence(t *testing.T) {
	out := runVarsClaim(t, "precedence",
		map[string]string{
			"GLOBAL_ONLY": "from-global",
			"SHARED":      "from-job",
			"OVERRIDDEN":  "from-job",
		},
		map[string]string{"OVERRIDDEN": "from-step"},
		`echo "global=[$GLOBAL_ONLY] shared=[$SHARED] overridden=[$OVERRIDDEN] tpl=[{{ .Vars.SHARED }}]"`,
	)

	assert.Contains(t, out, "global=[from-global]", "a var the step does not mention must still reach its environment")
	assert.Contains(t, out, "shared=[from-job]", "the merged var value must reach the step environment")
	assert.Contains(t, out, "overridden=[from-step]", "a step's own env: must win over a var of the same name")
	assert.Contains(t, out, "tpl=[from-job]", "{{ .Vars.KEY }} must expand in a step's run:")
}

// The end-to-end half of TestVarsExtraEnv_DropsDeniedNames: even when the
// claim itself carries a var named after an agent credential (a run created
// before apply-time validation existed), the value must not appear in the
// step's environment.
func TestOrchestrator_VarsNeverCarryAgentCredentials(t *testing.T) {
	out := runVarsClaim(t, "denied",
		map[string]string{
			"UNIFIED_TOKEN":     "stolen-token-value",
			"UNIFIED_CACHE_KEY": "stolen-cache-key",
			"REGISTRY":          "ghcr.io/myorg",
		},
		nil,
		`echo "token=[$UNIFIED_TOKEN] cache=[$UNIFIED_CACHE_KEY] registry=[$REGISTRY]"`,
	)

	assert.Contains(t, out, "registry=[ghcr.io/myorg]", "an ordinary var must still reach the step")
	assert.Contains(t, out, "token=[]", "UNIFIED_TOKEN must not reach the step environment")
	assert.Contains(t, out, "cache=[]", "UNIFIED_CACHE_KEY must not reach the step environment")
	assert.NotContains(t, out, "stolen-token-value")
	assert.NotContains(t, out, "stolen-cache-key")
}
