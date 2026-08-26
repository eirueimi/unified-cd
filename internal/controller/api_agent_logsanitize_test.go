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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests reuse claimRunForAgentLogBulkTest (api_agent_logbulk_test.go)
// to set up a live (claimed, unarchived) run: it is not specific to the
// bulk endpoint despite the name, and every test below needs the same
// "run exists, is live, and is owned by the posting agent" precondition.

// TestAgentLogBulk_NULByteLine_Lands is the fix's core regression test: a
// bulk batch containing a line with an embedded NUL byte must no longer
// lose the run's entire share of the batch. Before this change, PostgreSQL
// rejected the whole per-run INSERT with SQLSTATE 22021, AppendLogs
// returned an error, the handler 500'd, and (per LogPusher's oldest-first
// retry) the run's log delivery wedged on that batch until drop-oldest
// eviction discarded it. Now the offending byte is sanitized before it
// reaches the store, so the request succeeds and every line -- including
// the previously-poison one, with the NUL replaced by U+FFFD -- lands, in
// order, with the good lines around it untouched.
func TestAgentLogBulk_NULByteLine_Lands(t *testing.T) {
	s, st := newTestServer(t)
	runID := claimRunForAgentLogBulkTest(t, st, "a1")

	lines := []api.LogAppendRequest{
		{RunID: runID, StepIndex: 0, Stream: "stdout", Line: "before"},
		{RunID: runID, StepIndex: 0, Stream: "stdout", Line: "b\x00d"},
		{RunID: runID, StepIndex: 0, Stream: "stdout", Line: "after"},
	}
	token := issueAgentAccessForTest(t, st, "a1", nil, nil)
	body, _ := json.Marshal(lines)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/runs/"+runID+"/steps/0/logs/bulk", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	got, err := st.TailLogs(context.Background(), runID, 0, 100)
	require.NoError(t, err)
	require.Len(t, got, 3, "all three lines must land, including the one that contained the NUL byte")
	assert.Equal(t, "before", got[0].Line)
	assert.Equal(t, "b�d", got[1].Line, "the NUL byte must be replaced, not the whole line dropped")
	assert.Equal(t, "after", got[2].Line)
}

// TestAgentLogBulk_RawInvalidUTF8OnWire_Lands covers PostgreSQL's other,
// independent rejection reason: a byte sequence that is not valid UTF-8 at
// all (as opposed to an embedded NUL, which IS valid UTF-8 but still
// rejected -- see TestAgentLogBulk_NULByteLine_Lands).
//
// This test does NOT exercise sanitizeAgentText's own invalid-UTF-8 branch
// -- that is deliberately proven separately, and only at the unit level, by
// TestSanitizeAgentText_InvalidUTF8. Constructing the request body with
// encoding/json (as every other test in this file does via json.Marshal)
// cannot reach that branch at all: json.Marshal itself already replaces any
// invalid UTF-8 byte in a Go string with U+FFFD while encoding it, before
// the request ever leaves this test, let alone the agent. To actually put
// an invalid byte on the wire this test hand-assembles the JSON body,
// splicing a raw 0xFF byte into the line field where json.Marshal would
// have laundered it away.
//
// What that proves: json.Decode on the controller side ALSO replaces
// invalid UTF-8 with U+FFFD while parsing the request body -- independently
// of sanitizeAgentText, which never sees the raw byte because by the time
// it runs, req.Line already decoded as valid UTF-8 (confirmed below: this
// test passes identically whether or not sanitizeAgentText's invalid-UTF-8
// handling is present, because Go's own JSON decoder got there first). So
// for every endpoint in this file -- all of which decode JSON request
// bodies -- raw invalid UTF-8 from an agent cannot reach PostgreSQL's
// rejection path at all; only NUL can (NUL is valid UTF-8, so JSON leaves
// it alone). sanitizeAgentText's invalid-UTF-8 handling is still correct
// and still cheap to keep -- for any future caller that does not decode
// through encoding/json -- but it is not what makes this particular request
// succeed.
func TestAgentLogBulk_RawInvalidUTF8OnWire_Lands(t *testing.T) {
	s, st := newTestServer(t)
	runID := claimRunForAgentLogBulkTest(t, st, "a1")

	var body bytes.Buffer
	body.WriteString(`[{"runId":"` + runID + `","stepIndex":0,"stream":"stdout","line":"before"},`)
	body.WriteString(`{"runId":"` + runID + `","stepIndex":0,"stream":"stdout","line":"bad`)
	body.WriteByte(0xff) // raw invalid UTF-8 byte, spliced in unescaped
	body.WriteString(`byte"},`)
	body.WriteString(`{"runId":"` + runID + `","stepIndex":0,"stream":"stdout","line":"after"}]`)

	token := issueAgentAccessForTest(t, st, "a1", nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/runs/"+runID+"/steps/0/logs/bulk", bytes.NewReader(body.Bytes()))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	got, err := st.TailLogs(context.Background(), runID, 0, 100)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "before", got[0].Line)
	assert.Equal(t, "bad�byte", got[1].Line)
	assert.Equal(t, "after", got[2].Line)
}

// TestAgentLogAppend_NULByteLine_Lands covers the single-line endpoint
// alongside the bulk one: it was never subject to the batch's
// all-or-nothing failure, but it hit the same PostgreSQL rejection for the
// one poisoned line, and the same fix applies.
func TestAgentLogAppend_NULByteLine_Lands(t *testing.T) {
	s, st := newTestServer(t)
	runID := claimRunForAgentLogBulkTest(t, st, "a1")

	token := issueAgentAccessForTest(t, st, "a1", nil, nil)
	body, _ := json.Marshal(api.LogAppendRequest{RunID: runID, StepIndex: 0, Stream: "stdout", Line: "b\x00d"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/logs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	got, err := st.TailLogs(context.Background(), runID, 0, 100)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "b�d", got[0].Line)
}

// TestAgentSetStepOutputs_NULByteValue_Lands covers the other agent-write
// path exposed to the same PostgreSQL rejection: a captured step output
// value (unlike its key, which comes from the job's own outputs:
// declaration) can carry arbitrary bytes and must be sanitized the same way
// a log line is.
func TestAgentSetStepOutputs_NULByteValue_Lands(t *testing.T) {
	s, st := newTestServer(t)
	runID := claimRunForAgentLogBulkTest(t, st, "a1")

	token := issueAgentAccessForTest(t, st, "a1", nil, nil)
	body, _ := json.Marshal(api.SetOutputsRequest{Outputs: map[string]string{"result": "b\x00d"}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/runs/"+runID+"/steps/0/outputs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	outputs, err := st.GetStepOutputs(context.Background(), runID, 0)
	require.NoError(t, err)
	assert.Equal(t, "b�d", outputs["result"])
}

// TestAgentStepReport_NULByteInStepNameAndVariant_Lands covers the
// step-report path: unlike the log line and output value above, StepName,
// Variant, and CallJobName were originally excluded from sanitization on the
// grounds that they come from the job DSL rather than captured runtime
// output. That does not hold structurally: this endpoint sits behind the
// same agent authentication as the log/output handlers, and Variant in
// particular carries step.MatrixKey, built from matrix dimension values
// that internal/dsl/matrix.go's EvalMatrix only rejects for containing "/"
// -- unlike a step name, which is constrained by stepNameRe to a Go
// identifier and so can never contain a NUL. Left unsanitized, a NUL here
// does not just fail a single write: handleAgentStepReport's caller wraps
// ReportStep in retryUntilSuccess on the if:-skip and call:-completion
// paths (internal/agent/orchestrator.go), which treats a permanent 500 the
// same as a transient failure and retries forever -- the same wedge shape
// this branch exists to remove, relocated from logs to step status.
func TestAgentStepReport_NULByteInStepNameAndVariant_Lands(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(context.Background(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(context.Background(), "j", nil, []byte(`{}`), nil, nil, "")
	claimRunForTest(t, pg, "a1", run.ID)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	body, _ := json.Marshal(api.StepReportRequest{
		RunID:     run.ID,
		StepIndex: 0,
		StepName:  "b\x00d",
		Variant:   "li\x00nux",
		Status:    "Running",
		StartedAt: time.Now(),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/steps", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	steps, err := pg.GetRunSteps(context.Background(), run.ID)
	require.NoError(t, err)
	require.Len(t, steps, 1, "the step report must land, not fail the write")
	assert.Equal(t, "b�d", steps[0].Name, "the NUL byte in StepName must be replaced, not fail the whole write")
	assert.Equal(t, "li�nux", steps[0].Variant, "the NUL byte in Variant must be replaced, not fail the whole write")
}
