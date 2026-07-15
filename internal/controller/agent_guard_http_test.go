package controller

// Matrix: every agent write endpoint must 403 for a non-owning agent (no
// mutation), and three of the F2 endpoints (step outputs, run outputs,
// approval-create) must additionally no-op with alreadyFinalized on
// terminal runs owned by the caller. Sidecar status is ownership-only
// (rejectTerminal=false): the agent's pump reports the final "exited"
// status after FinishRun by design (deferred CloseScopes runs after the
// finish call), so terminal-run writes there are expected, not rejected —
// see TestAgentGuardHTTP_OwnerTerminalRun_SidecarStatusLands. Correct-agent
// live-run behavior is covered by pre-existing endpoint tests, which must
// stay green.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	guardOwnerAgent    = "owner-agent"
	guardIntruderAgent = "intruder-agent"
)

// seedGuardRun creates a job and a run, transitions the run to Queued, and
// claims it with guardOwnerAgent via the store's claim path — the same
// store-level sequence api_agent_reconcile_test.go uses — leaving the run
// Running with ClaimedBy == guardOwnerAgent. When terminal is true it
// additionally finishes the run (Succeeded); MarkRunFinished does not clear
// claimed_by, so the result is a terminal run still owned by guardOwnerAgent.
func seedGuardRun(t *testing.T, pg store.Store, terminal bool) string {
	t.Helper()
	ctx := context.Background()
	specJSON := []byte(`{"steps":[{"name":"s","run":"echo x"}]}`)
	const jobName = "guard-job"
	_, err := pg.UpsertJob(ctx, jobName, "unified-cd/v1", specJSON)
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, jobName, nil, specJSON, nil, nil, "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	claimed, err := pg.ClaimNextRun(ctx, guardOwnerAgent, nil)
	require.NoError(t, err)
	require.Equal(t, run.ID, claimed.ID, "claim must pick up the just-created run")
	if terminal {
		require.NoError(t, pg.MarkRunFinished(ctx, run.ID, api.RunSucceeded))
	}
	return run.ID
}

// agentGuardCase describes one of the eight agent write endpoints for the
// matrix below: how to build its request, and how to prove a rejected
// request did not mutate anything. terminalOK marks the F2 subset (outputs
// x2, sidecars, approval-create) that also rejects terminal runs.
type agentGuardCase struct {
	name       string
	terminalOK bool
	path       func(agentID, runID string) string
	body       func(runID string) []byte
	verifyNoop func(t *testing.T, pg store.Store, runID string)
}

func agentGuardCases() []agentGuardCase {
	return []agentGuardCase{
		{
			name: "step-report",
			path: func(agentID, runID string) string {
				return "/api/v1/agents/" + agentID + "/steps"
			},
			body: func(runID string) []byte {
				b, _ := json.Marshal(api.StepReportRequest{
					RunID: runID, StepIndex: 0, Status: "Running", StartedAt: time.Now(),
				})
				return b
			},
			verifyNoop: func(t *testing.T, pg store.Store, runID string) {
				steps, err := pg.GetRunSteps(context.Background(), runID)
				require.NoError(t, err)
				assert.Empty(t, steps, "step report must not land")
			},
		},
		{
			name: "log-append",
			path: func(agentID, runID string) string {
				return "/api/v1/agents/" + agentID + "/logs"
			},
			body: func(runID string) []byte {
				b, _ := json.Marshal(api.LogAppendRequest{
					RunID: runID, StepIndex: 0, Stream: "stdout", Timestamp: time.Now(), Line: "hello",
				})
				return b
			},
			verifyNoop: func(t *testing.T, pg store.Store, runID string) {
				count, _, _, err := pg.CountLogs(context.Background(), runID, nil)
				require.NoError(t, err)
				assert.Zero(t, count, "log line must not land")
			},
		},
		{
			name: "finish",
			path: func(agentID, runID string) string {
				return "/api/v1/agents/" + agentID + "/runs/" + runID + "/finish"
			},
			body: func(runID string) []byte {
				b, _ := json.Marshal(map[string]string{"status": string(api.RunSucceeded)})
				return b
			},
			verifyNoop: func(t *testing.T, pg store.Store, runID string) {
				got, err := pg.GetRun(context.Background(), runID)
				require.NoError(t, err)
				assert.NotEqual(t, api.RunSucceeded, got.Status, "finish must not land")
			},
		},
		{
			// terminalOK pins the F2 decision that outputs reported after a
			// run is terminal are never recorded — including run/step
			// outputs from `finally:` steps of a cancelled run, since
			// cancellation marks the run terminal before those `finally:`
			// steps execute. Consistent with handleAgentStepReport, which
			// already no-op'd step status writes on terminal runs before
			// this branch (see docs/superpowers/specs/2026-07-15-hardening-
			// f1-f7-design.md, F2 section).
			name:       "set-step-outputs",
			terminalOK: true,
			path: func(agentID, runID string) string {
				return "/api/v1/agents/" + agentID + "/runs/" + runID + "/steps/0/outputs"
			},
			body: func(runID string) []byte {
				b, _ := json.Marshal(api.SetOutputsRequest{Outputs: map[string]string{"k": "v"}})
				return b
			},
			verifyNoop: func(t *testing.T, pg store.Store, runID string) {
				stored, err := pg.GetStepOutputs(context.Background(), runID, 0)
				require.NoError(t, err)
				assert.Empty(t, stored, "step output must not land")
			},
		},
		{
			// See the set-step-outputs case above: same finally-after-cancel
			// rationale applies to run outputs.
			name:       "set-run-outputs",
			terminalOK: true,
			path: func(agentID, runID string) string {
				return "/api/v1/agents/" + agentID + "/runs/" + runID + "/outputs"
			},
			body: func(runID string) []byte {
				b, _ := json.Marshal(api.SetOutputsRequest{Outputs: map[string]string{"result": "ok"}})
				return b
			},
			verifyNoop: func(t *testing.T, pg store.Store, runID string) {
				stored, err := pg.GetRunOutputs(context.Background(), runID)
				require.NoError(t, err)
				assert.Empty(t, stored, "run output must not land")
			},
		},
		{
			name: "log-bulk",
			path: func(agentID, runID string) string {
				return "/api/v1/agents/" + agentID + "/runs/" + runID + "/steps/0/logs/bulk"
			},
			body: func(runID string) []byte {
				lines := []api.LogAppendRequest{
					{RunID: runID, StepIndex: 0, Stream: "stdout", Timestamp: time.Now(), Line: "line1"},
				}
				b, _ := json.Marshal(lines)
				return b
			},
			verifyNoop: func(t *testing.T, pg store.Store, runID string) {
				count, _, _, err := pg.CountLogs(context.Background(), runID, nil)
				require.NoError(t, err)
				assert.Zero(t, count, "bulk log lines must not land")
			},
		},
		{
			// Sidecars are deliberately NOT in the terminalOK (F2) set: both
			// agents stop their sidecar pumps via a deferred CloseScopes that
			// runs AFTER FinishRun, so the final "exited" status report is
			// *expected* to land on an already-terminal run (see
			// TestAgentGuardHTTP_OwnerTerminalRun_SidecarStatusLands below).
			// Only ownership is enforced here.
			name: "sidecars",
			path: func(agentID, runID string) string {
				return "/api/v1/agents/" + agentID + "/runs/" + runID + "/sidecars"
			},
			body: func(runID string) []byte {
				b, _ := json.Marshal(api.SidecarStatusRequest{
					RunID: runID, Name: "mysql", Index: 100, Phase: "running",
				})
				return b
			},
			verifyNoop: func(t *testing.T, pg store.Store, runID string) {
				statuses, err := pg.GetSidecarStatuses(context.Background(), runID)
				require.NoError(t, err)
				assert.Empty(t, statuses, "sidecar status must not land")
			},
		},
		{
			name:       "approval-create",
			terminalOK: true,
			path: func(agentID, runID string) string {
				return "/api/v1/agents/" + agentID + "/runs/" + runID + "/approvals"
			},
			body: func(runID string) []byte {
				b, _ := json.Marshal(api.CreateApprovalRequest{StepIndex: 0, StepName: "gate", Message: "approve?"})
				return b
			},
			verifyNoop: func(t *testing.T, pg store.Store, runID string) {
				_, err := pg.GetApproval(context.Background(), runID, 0)
				assert.Error(t, err, "approval must not land")
			},
		},
	}
}

// TestAgentGuardHTTP_IntruderForbidden verifies that every one of the eight
// agent write endpoints rejects a non-owning agent with 403 and does not
// mutate any state, regardless of whether that endpoint also checks for
// terminal runs.
func TestAgentGuardHTTP_IntruderForbidden(t *testing.T) {
	for _, c := range agentGuardCases() {
		t.Run(c.name, func(t *testing.T) {
			s, pg := newTestServer(t)
			runID := seedGuardRun(t, pg, false)

			req := httptest.NewRequest(http.MethodPost, c.path(guardIntruderAgent, runID), bytes.NewReader(c.body(runID)))
			req.Header.Set("Authorization", "Bearer agent-secret")
			rec := httptest.NewRecorder()
			s.Router().ServeHTTP(rec, req)

			require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "is claimed by another agent")
			c.verifyNoop(t, pg, runID)
		})
	}
}

// TestAgentGuardHTTP_OwnerTerminalNoop verifies the F2 subset (step outputs,
// run outputs, sidecar status, approval-create): when the *owning* agent
// writes to an already-terminal run, the handler responds 200 with
// alreadyFinalized and the write does not land.
func TestAgentGuardHTTP_OwnerTerminalNoop(t *testing.T) {
	for _, c := range agentGuardCases() {
		if !c.terminalOK {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			s, pg := newTestServer(t)
			runID := seedGuardRun(t, pg, true)

			req := httptest.NewRequest(http.MethodPost, c.path(guardOwnerAgent, runID), bytes.NewReader(c.body(runID)))
			req.Header.Set("Authorization", "Bearer agent-secret")
			rec := httptest.NewRecorder()
			s.Router().ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "alreadyFinalized")
			c.verifyNoop(t, pg, runID)
		})
	}
}

// TestAgentGuardHTTP_OwnerTerminalRun_SidecarStatusLands pins the sidecar
// status endpoint's ownership-only guard: the owning agent's final "exited"
// report, which by design arrives after the run has already been marked
// terminal (FinishRun happens before the deferred CloseScopes stops the
// sidecar pump — see handleAgentSidecarStatus), must land rather than be
// rejected as a no-op. Without this, sidecars would show "running" forever
// for every completed run.
func TestAgentGuardHTTP_OwnerTerminalRun_SidecarStatusLands(t *testing.T) {
	s, pg := newTestServer(t)
	runID := seedGuardRun(t, pg, true) // Succeeded, still claimed by guardOwnerAgent

	exitCode := 0
	body, _ := json.Marshal(api.SidecarStatusRequest{
		RunID: runID, Name: "mysql", Index: 0, Phase: "exited", ExitCode: &exitCode,
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/"+guardOwnerAgent+"/runs/"+runID+"/sidecars", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	statuses, err := pg.GetSidecarStatuses(context.Background(), runID)
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, "exited", statuses[0].Phase)
	require.NotNil(t, statuses[0].ExitCode)
	assert.Equal(t, 0, *statuses[0].ExitCode)
}
