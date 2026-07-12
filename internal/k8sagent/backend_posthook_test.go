package k8sagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestK8sBackend_RunPostHook_ExecsIntoGivenContainer locks in the fix for the
// post-refactor regression where the shared orchestrator (agentlib.RunClaim)
// always drained post: hooks with an empty container string, losing the
// named-container routing a runsIn.container step's post hook needs (the
// pre-refactor k8s orchestrate loop routed a hook into the same container the
// step ran in). This drives the REAL k8sBackend.RunPostHook (not the parity
// fake) with a non-zero container and a zero scope, and asserts the exec
// lands in that container rather than the pod's default ("").
func TestK8sBackend_RunPostHook_ExecsIntoGivenContainer(t *testing.T) {
	ex := &fakeExec{exit: 0}
	a := &K8sAgent{exec: ex}
	b := newK8sBackend(a, "run-1", "pod-default", "/workspace")

	err := b.RunPostHook(context.Background(), agentlib.ScopeHandle{}, "build", "cleanup.sh", nil, io.Discard, io.Discard)
	require.NoError(t, err)

	assert.Equal(t, "pod-default", ex.gotPod, "non-scoped post hook must target the default pod")
	assert.Equal(t, "build", ex.gotContainer, "post hook must exec into the given container when non-empty and scope is zero")
	assert.Equal(t, "cleanup.sh", ex.gotScript)
}

// TestK8sBackend_RunPostHook_DefaultContainerWhenEmpty is the companion case:
// a plain step (no runsIn.container) queues its post hook with container ==
// "", and RunPostHook must still exec into the pod's default container (not
// panic / not require a non-empty container).
func TestK8sBackend_RunPostHook_DefaultContainerWhenEmpty(t *testing.T) {
	ex := &fakeExec{exit: 0}
	a := &K8sAgent{exec: ex}
	b := newK8sBackend(a, "run-1", "pod-default", "/workspace")

	err := b.RunPostHook(context.Background(), agentlib.ScopeHandle{}, "", "cleanup.sh", nil, io.Discard, io.Discard)
	require.NoError(t, err)

	assert.Equal(t, "pod-default", ex.gotPod)
	assert.Equal(t, "", ex.gotContainer, "empty container means the pod's default container")
}

// TestK8sBackend_RunPostHook_StreamsOutputToGivenWriters is the regression
// test for the bug where RunPostHook always exec'd with io.Discard, io.Discard
// (internal/k8sagent/backend.go), throwing away a post hook's stdout/stderr
// even though production code (fakeExec here stands in for the real
// k8s exec.ExecStep, exactly like TestK8sBackend_RunPostHook_ExecsIntoGivenContainer
// above) writes real output to whatever writers it's given. This drives the
// REAL k8sBackend.RunPostHook directly with real StepLogWriters (pointed at a
// fake controller HTTP server, mirroring secrets_masking_k8s_test.go's
// pattern) and asserts the post hook's stdout reaches the owning step's
// shipped logs bulk endpoint — AND that a secret value in that output is
// masked, proving StepLogWriters' SetMasker installation applies to
// post-hook writers exactly as it does to main-step writers.
func TestK8sBackend_RunPostHook_StreamsOutputToGivenWriters(t *testing.T) {
	const agentID = "posthook-stream-agent"
	const runID = "run-posthook-stream"
	const secretValue = "s3cr3t-k8s-post-value"

	var mu sync.Mutex
	var stepZeroLines []string

	mux := http.NewServeMux()
	// stdout: k8sBackend.StepLogWriters' logLineWriter ships one line at a
	// time via the single-line AppendLog endpoint (see internal/k8sagent/
	// agent.go's logLineWriter.Write); stderr ships via the bulk endpoint (a
	// LogPusher, mirroring secrets_masking_k8s_test.go's harness). This test's
	// post hook only writes to stdout, but both are wired for realism.
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, r *http.Request) {
		var req api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		if req.Line != "" {
			stepZeroLines = append(stepZeroLines, req.Line)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/2/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		var reqs []api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&reqs)
		mu.Lock()
		for _, req := range reqs {
			if req.Line != "" {
				stepZeroLines = append(stepZeroLines, req.Line)
			}
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := agentlib.NewClient(srv.URL, "tok")
	ex := &fakeExec{exit: 0, stdout: "POST_HOOK_MARKER\n" + secretValue + "\n"}
	a := &K8sAgent{cfg: Config{AgentID: agentID}, client: client, exec: ex}
	b := newK8sBackend(a, runID, "pod-default", "/workspace")
	b.SetMasker(secrets.NewMasker([]string{secretValue}))

	// stepIndex 2: an arbitrary non-zero owning-step index, proving
	// RunPostHook's output is attributed to WHATEVER step index the caller
	// (the orchestrator's hookStack drain) opened writers for, not hardcoded
	// to 0.
	stdout, stderr, finish := b.StepLogWriters(context.Background(), 2)
	err := b.RunPostHook(context.Background(), agentlib.ScopeHandle{}, "", "cleanup.sh", nil, stdout, stderr)
	require.NoError(t, err)
	finish(context.Background())

	mu.Lock()
	joined := strings.Join(stepZeroLines, "\n")
	mu.Unlock()

	assert.Contains(t, joined, "POST_HOOK_MARKER", "expected the post hook's real stdout to reach step 2's shipped logs")
	assert.NotContains(t, joined, secretValue, "post hook output leaked an unmasked secret value")
	assert.Contains(t, joined, "***", "expected the secret-bearing post-hook line to be masked")
}
