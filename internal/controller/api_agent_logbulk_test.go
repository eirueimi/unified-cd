package controller

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

// countingLogBulkStore wraps a real store.Store (so auth, ownership guard,
// and every other call the request path needs still work against Postgres)
// and overrides only the two log-append entry points, to prove which one the
// handler actually uses and how many times.
//
// Unlike fakeQueuedReaperStore (queuedrun_reaper_test.go), which embeds
// store.Store as a nil interface and relies on the reaper calling nothing
// else, this handler goes through the full HTTP stack — agent auth, the
// ownership-guard pass, etc. — so the embedded Store must be a working
// implementation, not nil, or any uncounted call panics.
type countingLogBulkStore struct {
	store.Store

	appendLogsCalls int
	appendLogCalls  int
	lastBatch       []store.LogAppend

	// allDropped makes AppendLogs report every line dropped (seq 0), as it
	// would for a sealed run, without a real per-run seal check.
	allDropped bool
	nextSeq    int64
}

func (f *countingLogBulkStore) AppendLogs(ctx context.Context, lines []store.LogAppend) ([]int64, error) {
	f.appendLogsCalls++
	f.lastBatch = append([]store.LogAppend(nil), lines...)
	seqs := make([]int64, len(lines))
	if !f.allDropped {
		for i := range lines {
			f.nextSeq++
			seqs[i] = f.nextSeq
		}
	}
	return seqs, nil
}

func (f *countingLogBulkStore) AppendLog(ctx context.Context, runID string, stepIndex int, stream string, ts time.Time, line string) (int64, error) {
	f.appendLogCalls++
	f.nextSeq++
	return f.nextSeq, nil
}

// claimRunForAgentLogBulkTest creates a job, a run, and claims it for
// agentID, mirroring sealRun's setup (api_agent_seal_test.go) minus the
// finish/archive steps — the bulk handler's ownership guard only checks
// ClaimedBy, since it calls agentRunGuard with rejectTerminal=false.
func claimRunForAgentLogBulkTest(t *testing.T, st store.Store, agentID string) string {
	t.Helper()
	ctx := context.Background()
	_, err := st.UpsertJob(ctx, "j-bulk", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := st.CreateRun(ctx, "j-bulk", nil, []byte(`{}`), nil, nil, "", "")
	require.NoError(t, err)
	_, err = st.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	claimed, err := st.ClaimNextRun(ctx, agentID, nil)
	require.NoError(t, err)
	require.Equal(t, run.ID, claimed.ID)
	return run.ID
}

// TestHandleAgentLogBulk_OneBatchCall is the specific regression this change
// exists to prevent recurring: the agent batches, and the controller must not
// unbatch. A counting fake proves the handler makes ONE store call for a
// request carrying many lines, not one per line.
func TestHandleAgentLogBulk_OneBatchCall(t *testing.T) {
	s, pg := newTestServer(t)
	fake := &countingLogBulkStore{Store: pg}
	s.store = fake
	agentID := "bulk-agent"
	runID := claimRunForAgentLogBulkTest(t, pg, agentID)
	token := issueAgentAccessForTest(t, pg, agentID, nil, nil)

	const n = 25
	lines := make([]api.LogAppendRequest, n)
	for i := range lines {
		lines[i] = api.LogAppendRequest{RunID: runID, StepIndex: 0, Stream: "stdout", Line: "line"}
	}
	body, _ := json.Marshal(lines)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/runs/"+runID+"/steps/0/logs/bulk", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	assert.Equal(t, 1, fake.appendLogsCalls, "handler must batch, not loop")
	assert.Equal(t, 0, fake.appendLogCalls, "handler must not use the single-line path")
	assert.Len(t, fake.lastBatch, n)
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// TestHandleAgentLogBulk_SealedRunDropped: when the store reports every line
// dropped, the handler still returns 204 and counts the drops.
func TestHandleAgentLogBulk_SealedRunDropped(t *testing.T) {
	s, pg := newTestServer(t)
	fake := &countingLogBulkStore{Store: pg, allDropped: true}
	s.store = fake
	agentID := "bulk-agent-sealed"
	runID := claimRunForAgentLogBulkTest(t, pg, agentID)
	token := issueAgentAccessForTest(t, pg, agentID, nil, nil)

	lines := []api.LogAppendRequest{
		{RunID: runID, StepIndex: 0, Stream: "stdout", Line: "late 1"},
		{RunID: runID, StepIndex: 0, Stream: "stderr", Line: "late 2"},
	}
	body, _ := json.Marshal(lines)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/runs/"+runID+"/steps/0/logs/bulk", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	// nothing is asserted about the log line itself; the point is that an
	// all-dropped batch is not an error
	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, 1, fake.appendLogsCalls)
}

// TestHandleAgentLogBulk_EmptyBatch covers the request body `[]`: the
// handler still returns 204. It is not asserted here whether AppendLogs is
// called for a zero-length batch — that is whatever the handler does
// naturally with an empty decoded slice.
func TestHandleAgentLogBulk_EmptyBatch(t *testing.T) {
	s, pg := newTestServer(t)
	fake := &countingLogBulkStore{Store: pg}
	s.store = fake
	agentID := "bulk-agent-empty"
	runID := claimRunForAgentLogBulkTest(t, pg, agentID)
	token := issueAgentAccessForTest(t, pg, agentID, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/runs/"+runID+"/steps/0/logs/bulk", bytes.NewReader([]byte("[]")))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, 0, fake.appendLogCalls, "an empty batch must never fall through to the single-line path")
	// The handler has no early-return for an empty decoded slice: it still
	// builds a zero-length batch and calls AppendLogs once with it, rather
	// than skipping the store call entirely. Documenting that here so a
	// future change to add an early return is a deliberate choice, not a
	// silent behaviour drift.
	assert.Equal(t, 1, fake.appendLogsCalls)
	assert.Empty(t, fake.lastBatch)
}
