package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// ExecuteCallStep launches a child Run for a `call:` step and polls until it
// completes. It is backend-agnostic (used by both the host agent and the k8s
// agent) so that where the child actually runs is decided by the child job's
// own scheduling, not by which agent executed the call.
//
// Returns the child Run's outputs and the child Run's ID (so the caller can
// report it on the step's terminal StepReport for caller→child linking in the
// WebUI). childRunID is "" only if the child run was never created (param
// template failure, or the create request itself failed); on every other
// path (success, failure, cancellation, timeout) it is returned alongside
// the error so the link is preserved even for failed calls.
//
// runID is the PARENT run's ID, used to publish the child link on a
// non-terminal step report as soon as the child is created (see below).
func ExecuteCallStep(ctx context.Context, client *Client, agentID, runID string, step api.ClaimStep, tplData dsl.TemplateData) (outputs map[string]string, childRunID string, err error) {
	// Expand templates in the call parameters.
	// Stdout is not exposed to prevent previous step output from leaking into child job parameters.
	// Expansion errors fail the step: these values become the child run's
	// inputs, and silently forwarding a raw unexpanded template (e.g. a
	// literal "{{ .RunID }}") hides the mistake until it surfaces in the
	// child job or an external webhook. Matches the cache-step precedent.
	callCtx := dsl.TemplateData{Params: tplData.Params, Steps: tplData.Steps}
	expandedParams := map[string]string{}
	for k, v := range step.Call.Params {
		expanded, err := dsl.ExpandTemplate(v, callCtx)
		if err != nil {
			return nil, "", fmt.Errorf("call param %q template: %w", k, err)
		}
		expandedParams[k] = expanded
	}

	childRun, err := client.CreateRun(ctx, step.Call.Job, expandedParams)
	if err != nil {
		return nil, "", fmt.Errorf("create child run for job %q: %w", step.Call.Job, err)
	}
	slog.Info("call: child run created", "childRunId", childRun.ID, "job", step.Call.Job)

	// Publish the caller→child link immediately on a non-terminal report so the
	// WebUI can navigate to the child while it is still running (long child jobs
	// are exactly when the link matters). StartedAt/EndedAt stay zero: the
	// controller maps zero times to NULL and the UPSERT's COALESCE preserves the
	// values from the initial Running report. The terminal report re-sends the
	// link, so a report lost here self-heals; failure to send is non-fatal.
	_ = client.ReportStep(ctx, agentID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex,
		StepName: step.DisplayName(), Variant: step.MatrixKey, Status: "Running",
		ChildRunID: childRun.ID, CallJobName: step.Call.Job,
	})

	const maxWait = 30 * time.Minute
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	deadline := time.Now().Add(maxWait)

	for {
		run, err := client.GetRun(ctx, childRun.ID)
		if err != nil {
			slog.Warn("call: poll child run failed", "childRunId", childRun.ID, "error", err)
		} else {
			switch run.Status {
			case api.RunSucceeded:
				outputs, oErr := client.GetRunOutputs(ctx, childRun.ID)
				if oErr != nil {
					slog.Warn("call: get child outputs failed", "childRunId", childRun.ID, "error", oErr)
					outputs = map[string]string{}
				}
				return outputs, childRun.ID, nil
			case api.RunFailed, api.RunCancelled:
				return nil, childRun.ID, fmt.Errorf("call: child run %s finished with status %s", childRun.ID, run.Status)
			}
		}

		if time.Now().After(deadline) {
			return nil, childRun.ID, fmt.Errorf("call: child run %s timed out after %s", childRun.ID, maxWait)
		}
		select {
		case <-ctx.Done():
			// child run orphaned; log for visibility
			slog.Warn("call: parent context cancelled, child run may be orphaned", "childRunId", childRun.ID)
			return nil, childRun.ID, ctx.Err()
		case <-ticker.C:
		}
	}
}
