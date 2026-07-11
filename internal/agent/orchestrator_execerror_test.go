package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunClaim_InfraExecError_ShippedToStepLog reproduces the class of failure
// seen when a step's exec never runs to completion — e.g. its target container
// image has no shell (a distroless image like ghcr.io/astral-sh/ruff, where the
// injected `sleep infinity` keep-alive and the `bash -lc` exec both fail). In
// that case the backend returns (exitCode, runErr) with a non-nil runErr and
// the command produces no output of its own, so without special handling the
// run shows an opaque failure with ZERO log lines and nothing to debug.
//
// A native claim carrying a `container:` step is the host-backend analogue:
// hostBackend.RunNamedContainer returns an error (no pod to exec into), driving
// the same orchestrator branch (orchestrator.go, `if runErr != nil`). The
// orchestrator must surface runErr to the failing step's own log stream.
func TestRunClaim_InfraExecError_ShippedToStepLog(t *testing.T) {
	const agentID = "infra-exec-error-agent"
	const runID = "run-infra-exec-error"

	h := newGuardHarness()
	srv := newGuardServer(t, agentID, h)

	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   runID,
		JobName: "test-infra-exec-error",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index:     0,
				Name:      "lint",
				Container: "ruff",
				Run:       "ruff check /workspace",
			}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	h.mu.Lock()
	defer h.mu.Unlock()

	assert.Equal(t, "Failed", h.finishStatus, "an infra exec error must fail the run")

	lines := h.logsByStep[0]
	require.NotEmpty(t, lines, "the failing step must ship at least one log line explaining why it failed")
	found := false
	for _, l := range lines {
		if l.Stream == "stderr" && l.StepIndex == 0 && strings.Contains(l.Line, "failed to execute") {
			found = true
		}
	}
	assert.True(t, found, "expected a stderr line on step 0 surfacing the exec error, got: %+v", lines)
}
