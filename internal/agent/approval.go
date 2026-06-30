package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
)

// WaitForApproval creates a pending approval, reports the step as WaitingApproval,
// and polls until the approval is decided or its timeout elapses. Returns true
// only on an explicit Approved decision. On Rejected/TimedOut, on deadline
// expiry, or on ctx cancellation it returns false. Poll errors are logged and
// retried (they do not end the wait).
//
// The pending-row create and WaitingApproval report are best-effort. On timeout
// the agent has no decision endpoint (decisions are human-only), so the
// controller-side run_approvals row stays Pending; the caller fails the step,
// which fails the run and runs finally.
func WaitForApproval(ctx context.Context, c *Client, agentID, runID string, step api.ClaimStep, poll time.Duration) bool {
	timeoutMin := 60.0
	msg := ""
	if step.Approval != nil {
		if step.Approval.TimeoutMinutes > 0 {
			timeoutMin = step.Approval.TimeoutMinutes
		}
		msg = step.Approval.Message
	}

	// best-effort: create the pending row
	_ = c.CreateApproval(ctx, agentID, runID, api.CreateApprovalRequest{
		StepIndex:      step.Index,
		StepName:       step.Name,
		Message:        msg,
		TimeoutMinutes: timeoutMin,
	})
	// report the step as waiting for human approval
	_ = c.ReportStep(ctx, agentID, api.StepReportRequest{
		RunID:      runID,
		StepIndex:  step.Index,
		StageIndex: step.StageIndex,
		StepName:   step.Name,
		Status:     "WaitingApproval",
		StartedAt:  time.Now().UTC(),
	})

	deadline := time.Now().Add(time.Duration(timeoutMin*60) * time.Second)
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		a, err := c.GetApproval(ctx, agentID, runID, step.Index)
		if err == nil {
			switch a.Status {
			case "Approved":
				return true
			case "Rejected", "TimedOut":
				return false
			}
		} else {
			// poll errors are transient; log and keep waiting
			slog.Warn("approval poll failed", "runID", runID, "step", step.Name, "error", err)
		}
		if time.Now().After(deadline) {
			slog.Warn("approval timed out", "runID", runID, "step", step.Name)
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}
