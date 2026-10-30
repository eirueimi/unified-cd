package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
)

// finalizeBeforeUpsertStore finalizes the run right before the step-report write,
// i.e. after the handler's status pre-check has already seen a non-terminal run.
type finalizeBeforeUpsertStore struct {
	store.Store
	runID string
}

func (f *finalizeBeforeUpsertStore) UpsertStepReport(ctx context.Context, runID string, stepIndex, stageIndex int, stepName, variant, status string, exitCode *int, startedAt, endedAt *time.Time, childRunID, callJobName string) error {
	if _, err := f.Store.FinishRun(ctx, f.runID, api.RunFailed); err != nil {
		return err
	}
	return f.Store.UpsertStepReport(ctx, runID, stepIndex, stageIndex, stepName, variant, status, exitCode, startedAt, endedAt, childRunID, callJobName)
}

// TestAgentAPI_ReportStep_FinalizedBetweenCheckAndWrite forces run finalization
// between the handler's validation and the store write and proves the late report
// is rejected atomically (alreadyFinalized response, no step row written).
func TestAgentAPI_ReportStep_FinalizedBetweenCheckAndWrite(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "", "")
	claimRunForTest(t, pg, "a1", run.ID)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)
	s.store = &finalizeBeforeUpsertStore{Store: pg, runID: run.ID}

	body, _ := json.Marshal(api.StepReportRequest{
		RunID: run.ID, StepIndex: 0, Status: "Succeeded", StartedAt: time.Now(), EndedAt: time.Now(),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/steps", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["alreadyFinalized"])

	steps, err := pg.GetRunSteps(t.Context(), run.ID)
	require.NoError(t, err)
	assert.Empty(t, steps, "a report that lost the race to finalization must not write a step row")
	got, err := pg.GetRun(t.Context(), run.ID)
	require.NoError(t, err)
	assert.Equal(t, api.RunFailed, got.Status)
}
