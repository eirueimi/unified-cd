package agent

import (
	"context"
	"errors"
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
// How long it waits is bounded solely by ctx: the step's timeoutMinutes when
// set (applied to stepCtx by the orchestrator), or unbounded when unset — a
// call with no timeoutMinutes waits until the child run reaches a terminal
// state or the run is cancelled.
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

	childRun, err := client.CreateChildRun(ctx, agentID, runID, step.Call.Job, expandedParams)
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

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

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

		// The only wait bound is ctx. It carries the step's timeoutMinutes when
		// set (orchestrator.go wraps stepCtx with that deadline); when unset it
		// has no deadline, so the call waits until the child reaches a terminal
		// state. There is deliberately no separate hardcoded cap here. On a
		// timeout or run cancellation the child is not stopped from here — the
		// calling agent does not own the child run — but the controller's
		// parent-finish cascade (cancelDescendantRuns in api_agent.go) cancels
		// it once the parent run finalizes Failed/Cancelled.
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, childRun.ID, fmt.Errorf("call: child run %s did not complete within the step timeout", childRun.ID)
			}
			slog.Warn("call: context done before child finished; child run will be cancelled once the parent run finalizes", "childRunId", childRun.ID, "error", ctx.Err())
			return nil, childRun.ID, ctx.Err()
		case <-ticker.C:
		}
	}
}
