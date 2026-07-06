package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExecuteRun_StdoutStreamsWhileStepRuns pins that a run: step's stdout
// reaches the server WHILE the step is still executing, not only when it
// exits. stderr has always streamed via LogPusher, but stdout used to be
// captured in memory until step completion (RunStepCapture), so long builds
// (e.g. Unity via `-logFile -`) showed no output in the WebUI for their
// entire duration and then delivered tens of thousands of lines at once.
//
// Discriminator: the step prints a marker and then sleeps 3s. With buffered
// stdout the marker reaches the server milliseconds before the terminal step
// report; with streaming it arrives seconds earlier. Assert the gap > 1s.
func TestExecuteRun_StdoutStreamsWhileStepRuns(t *testing.T) {
	const agentID = "stream-agent"
	const runID = "run-stream"

	prevEvery := logPusherAutoFlushEvery
	logPusherAutoFlushEvery = 100 * time.Millisecond
	t.Cleanup(func() { logPusherAutoFlushEvery = prevEvery })

	var mu sync.Mutex
	var markerAt, terminalAt time.Time

	sawMarker := func(line string) {
		if !strings.Contains(line, "live-marker") {
			return
		}
		mu.Lock()
		if markerAt.IsZero() {
			markerAt = time.Now()
		}
		mu.Unlock()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		if req.Status == "Succeeded" || req.Status == "Failed" {
			mu.Lock()
			if terminalAt.IsZero() {
				terminalAt = time.Now()
			}
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, r *http.Request) {
		var req api.LogAppendRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		sawMarker(req.Line)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		var reqs []api.LogAppendRequest
		json.NewDecoder(r.Body).Decode(&reqs) //nolint:errcheck
		for _, l := range reqs {
			sawMarker(l.Line)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{
		ID:     agentID,
		Client: NewClient(srv.URL, "tok"),
	}
	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "test-stdout-stream",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index:      0,
				StageIndex: 0,
				Name:       "noisy",
				Run:        "echo live-marker; sleep 3",
			}},
		},
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	mu.Lock()
	defer mu.Unlock()
	require.False(t, markerAt.IsZero(), "the stdout marker line never reached the server")
	require.False(t, terminalAt.IsZero(), "the step never reported a terminal status")
	assert.Greater(t, terminalAt.Sub(markerAt), time.Second,
		"stdout must stream while the step runs: marker arrived %v before the terminal report (buffered stdout arrives only milliseconds before it)",
		terminalAt.Sub(markerAt))
}
