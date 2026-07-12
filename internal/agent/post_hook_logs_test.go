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
)

// TestExecuteRun_PostHookOutput_ReachesOwningStepLog is the regression test
// for the bug where a post: hook's stdout/stderr went nowhere: RunPostHook
// used to be called with nil writers on every host path (see
// backend_host.go's prior "sm.exec(ctx, h, script, env, nil, nil)" /
// "b.pod.Exec(ctx, container, script, env, nil, nil)" /
// "RunStepCapture(ctx, script, nil, env, b.workDir)"), so GET /runs/{id}/logs
// only ever showed main-step output and a failing post hook was invisible to
// users (surfaced only as agent-side slog).
//
// This drives a real native claim through executeRun (real bash execution,
// no exec faking — mirroring TestExecuteRun_ScopedStep_PostHookRunsInScopeContainer
// and TestExecuteRun_ParallelPostHooks_ConcurrentAppendIsSafe) with a single
// step whose post: hook echoes a marker AND a secret value, and asserts
// against the fake controller's steps/0/logs/bulk endpoint that:
//  1. the post hook's stdout reaches the log pusher, attributed to the OWNING
//     step's index (0) — the same step index the main step body's own output
//     was shipped under, not a separate pseudo-step;
//  2. a secret value present in the post hook's output is masked (***)
//     exactly like a secret in the main step's own output would be, proving
//     StepLogWriters' masker installation (SetMasker) is applied to post-hook
//     writers too, not just main-step writers.
func TestExecuteRun_PostHookOutput_ReachesOwningStepLog(t *testing.T) {
	const agentID = "posthook-log-agent"
	const runID = "run-posthook-log"
	const secretValue = "s3cr3t-post-hook-value"

	workDir := t.TempDir()

	var mu sync.Mutex
	var stepZeroLines []string
	finishCh := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	// Bulk log endpoint for step index 0 — both the main step body's output
	// AND the post hook's output must land here, since post output is
	// attributed to the owning step's index.
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/secrets/fetch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.AgentFetchSecretsResponse{
			Secrets: map[string]string{"POST_TOKEN": secretValue},
		})
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		select {
		case finishCh <- body.Status:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	resp := api.ClaimResponse{
		Native:        true,
		RunID:         runID,
		JobName:       "posthook-log-test",
		SecretsNeeded: []string{"POST_TOKEN"},
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index:      0,
				StageIndex: 0,
				Name:       "main",
				Run:        "echo main-step-output",
				Post:       &api.PostStep{Run: "echo POST_HOOK_MARKER; echo " + secretValue},
			}},
		},
	}

	a.executeRun(context.Background(), resp, workDir)

	select {
	case status := <-finishCh:
		if status != "Succeeded" {
			t.Fatalf("expected run to finish Succeeded, got %s", status)
		}
	default:
		t.Fatal("FinishRun was not called")
	}

	mu.Lock()
	joined := strings.Join(stepZeroLines, "\n")
	mu.Unlock()

	if !strings.Contains(joined, "main-step-output") {
		t.Fatalf("expected step 0's log to contain the main step's own output; got lines: %v", stepZeroLines)
	}
	if !strings.Contains(joined, "POST_HOOK_MARKER") {
		t.Fatalf("expected the post hook's stdout to reach step 0's log (owning step attribution); got lines: %v", stepZeroLines)
	}
	if strings.Contains(joined, secretValue) {
		t.Fatalf("post hook output leaked an unmasked secret value into the shipped log lines: %v", stepZeroLines)
	}
	if !strings.Contains(joined, "***") {
		t.Fatalf("expected the post hook's secret-bearing line to be masked (***); got lines: %v", stepZeroLines)
	}
}
