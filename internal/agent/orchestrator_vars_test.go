package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
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

// stepEnvDenied is exact-case and lists neither PATH nor HOME; apply-time
// validation matches on strings.ToUpper and refuses both. The runtime backstop
// has to refuse what apply-time refuses, or a run created before that
// validation existed can carry a global PATH that replaces the step's own on
// both backends — breaking every step of the job with nothing in the log to
// say why.
func TestVarsExtraEnv_DropsPathHomeAndMatchesCaseInsensitively(t *testing.T) {
	got := varsExtraEnv(map[string]string{
		"PATH":          "/attacker/bin",
		"Path":          "/attacker/bin",
		"home":          "/tmp/elsewhere",
		"unified_token": "stolen",
		"REGISTRY":      "ghcr.io/myorg",
	})
	require.Equal(t, []string{"REGISTRY=ghcr.io/myorg"}, got)
}

// The runtime backstop and apply-time validation must refuse the same names.
// internal/dsl cannot import internal/agent, so this test lives here — the
// package that can import both, and so compare the real structures. Its
// predecessor lived in internal/dsl and compared reservedVarNames against a
// hand-copied literal list, which is exactly the drift it was meant to catch:
// a name added to stepEnvDenied alone was invisible to it.
func TestVarsDenied_AgreesWithApplyTimeValidation(t *testing.T) {
	reserved := dsl.ReservedVarNames()
	require.NotEmpty(t, reserved)

	// Every name apply-time refuses is dropped at runtime too, in any case.
	for name := range reserved {
		assert.True(t, varsDenied[name], "%s is reserved at apply time but missing from varsDenied", name)
		assert.Empty(t, varsExtraEnv(map[string]string{name: "x"}),
			"%s is reserved at apply time and must not reach a step", name)
		lower := strings.ToLower(name)
		assert.Empty(t, varsExtraEnv(map[string]string{lower: "x"}),
			"%s is reserved at apply time (ValidateVarKeys upper-cases before the lookup) and must not reach a step", lower)
	}

	// And every agent credential the runtime drops is refused at apply time,
	// so an author hears about it when they apply rather than watching the
	// value silently vanish at run time.
	for name := range stepEnvDenied {
		assert.Error(t, dsl.ValidateVarKeys(map[string]string{name: "x"}),
			"%s is dropped at runtime but accepted at apply time", name)
	}

	// And every name the orchestrator SYNTHESISES must be refused at apply
	// time and dropped at run time. This leg is the one that was missing: the
	// reserved list covered the agent's credentials and the shell's baseline
	// but not UNIFIED_WORKSPACE/UNIFIED_AGENT_OS, which are written into the
	// same extraEnv slice the variables are appended to — and appended BEFORE
	// them, so a global Vars manifest naming one won the tie and every step of
	// every job silently resolved it to the manifest's value.
	//
	// This iterates the real SynthesizedStepEnv() rather than a literal pair,
	// so a THIRD synthesised name added to the orchestrator fails here instead
	// of repeating the same bug.
	require.NotEmpty(t, SynthesizedStepEnv())
	for _, name := range SynthesizedStepEnv() {
		assert.Error(t, dsl.ValidateVarKeys(map[string]string{name: "x"}),
			"%s is synthesised into every step's env but accepted as a variable at apply time", name)
		assert.True(t, varsDenied[name],
			"%s is synthesised into every step's env but missing from varsDenied", name)
		assert.Empty(t, varsExtraEnv(map[string]string{name: "x"}),
			"%s is synthesised into every step's env and a variable must never overwrite it", name)
		assert.Empty(t, varsExtraEnv(map[string]string{strings.ToLower(name): "x"}),
			"%s must be refused case-insensitively, as ValidateVarKeys does", strings.ToLower(name))
	}
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

// A variable must not be able to shadow the environment the orchestrator
// synthesises for the step itself.
//
// UNIFIED_WORKSPACE and UNIFIED_AGENT_OS go into the SAME extraEnv slice the
// variables are appended to, and go in FIRST — and a later duplicate wins in
// os/exec. So before these two names were reserved, a single global
// `kind: Vars` manifest with `UNIFIED_WORKSPACE: /tmp/attacker-scratch` was
// accepted by apply-time validation and, from the next claim, every step of
// every job on BOTH backends resolved $UNIFIED_WORKSPACE to that directory.
// The docs tell authors to build artifact and cache paths from exactly this
// variable, so the steps keep exiting 0 while reading and writing somewhere
// else, and nothing appears in any log.
//
// This is the end-to-end backstop, for a run created before the reservation
// existed; TestVarsDenied_AgreesWithApplyTimeValidation is the apply-time half.
func TestOrchestrator_VarsCannotShadowSynthesizedStepEnv(t *testing.T) {
	out := runVarsClaim(t, "shadow",
		map[string]string{
			"UNIFIED_WORKSPACE": "/tmp/attacker-scratch",
			"UNIFIED_AGENT_OS":  "plan9",
			"REGISTRY":          "ghcr.io/myorg",
		},
		nil,
		`echo "ws=[$UNIFIED_WORKSPACE] os=[$UNIFIED_AGENT_OS] registry=[$REGISTRY]"`,
	)

	assert.Contains(t, out, "registry=[ghcr.io/myorg]", "an ordinary var must still reach the step")
	assert.NotContains(t, out, "/tmp/attacker-scratch",
		"a var named UNIFIED_WORKSPACE must not replace the workspace the orchestrator set")
	assert.NotContains(t, out, "plan9",
		"a var named UNIFIED_AGENT_OS must not replace the OS the orchestrator reported")
	assert.Contains(t, out, "os=["+runtime.GOOS+"]",
		"the orchestrator's own UNIFIED_AGENT_OS must survive")
	assert.NotContains(t, out, "ws=[]",
		"the orchestrator's own UNIFIED_WORKSPACE must survive (dropping the var must not drop the real value)")
}
