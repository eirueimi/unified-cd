package metrics

import (
	"context"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
)

// InstrumentedStore decorates a store.Store, counting run and step state
// transitions. Every other method passes through via embedding, so reapers
// and handlers are instrumented with a single wrap in main.go.
//
// Known blind spot: run-finish transitions performed internally by
// *store.Postgres itself — e.g. TransitionPendingToQueued calling
// MarkRunFinished directly when it fails a run on a concurrency template-
// expansion error or an or-lock parameter conflict — call the underlying
// Postgres method, not this decorator, so they bypass it and are not
// counted in unifiedcd_runs_finished_total.
type InstrumentedStore struct {
	store.Store
	m *Metrics
}

func NewInstrumentedStore(s store.Store, m *Metrics) *InstrumentedStore {
	return &InstrumentedStore{Store: s, m: m}
}

func (s *InstrumentedStore) CreateRun(ctx context.Context, jobName string, params map[string]string, spec []byte, agentSelector []string, triggeredBy string) (*api.Run, error) {
	run, err := s.Store.CreateRun(ctx, jobName, params, spec, agentSelector, triggeredBy)
	if err == nil {
		s.m.RunCreated(triggeredBy)
	}
	return run, err
}

func (s *InstrumentedStore) FinishRun(ctx context.Context, runID string, status api.RunStatus) (bool, error) {
	updated, err := s.Store.FinishRun(ctx, runID, status)
	if err == nil && updated {
		s.m.RunFinished(string(status))
	}
	return updated, err
}

// MarkRunFinished routes through FinishRun so a transition is only counted
// when the run actually left a non-terminal state (the underlying CAS
// silently ignores finish-after-finish).
func (s *InstrumentedStore) MarkRunFinished(ctx context.Context, runID string, status api.RunStatus) error {
	_, err := s.FinishRun(ctx, runID, status)
	return err
}

func (s *InstrumentedStore) UpsertStepReport(ctx context.Context, runID string, stepIndex, stageIndex int, stepName, variant, status string, exitCode *int, startedAt, endedAt *time.Time, childRunID, callJobName string) error {
	err := s.Store.UpsertStepReport(ctx, runID, stepIndex, stageIndex, stepName, variant, status, exitCode, startedAt, endedAt, childRunID, callJobName)
	if err == nil && status != "Running" {
		s.m.StepCompleted(status)
		if startedAt != nil && endedAt != nil {
			s.m.StepDuration(status, endedAt.Sub(*startedAt).Seconds())
		}
	}
	return err
}
