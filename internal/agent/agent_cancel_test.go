package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
)

func TestExecuteRun_CancelPropagation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("TODO: retryUntilSuccess with context.WithoutCancel keeps retrying after test server closes; Windows socket cleanup slower than Linux")
	}
	finishStatus := make(chan string, 1)
	var getRunCalls atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/run-cancel/finish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case finishStatus <- body["status"]:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/run-cancel/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/runs/run-cancel", func(w http.ResponseWriter, r *http.Request) {
		n := getRunCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		status := api.RunRunning
		if n >= 2 {
			status = api.RunCancelled
		}
		json.NewEncoder(w).Encode(api.Run{ID: "run-cancel", Status: status}) //nolint:errcheck
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{
		ID:     "a1",
		Client: NewClient(srv.URL, "tok"),
	}

	claim := api.ClaimResponse{
		RunID:   "run-cancel",
		JobName: "test",
		Stages:  []api.ClaimStage{{Step: &api.ClaimStep{Name: "s1", Index: 0, Run: "sleep 30"}}},
	}

	workDir := t.TempDir()
	start := time.Now()
	a.executeRun(context.Background(), claim, workDir)

	elapsed := time.Since(start)
	assert.Less(t, elapsed, 15*time.Second, "executeRun should complete within 15s when cancelled (sleep 30 was interrupted)")

	select {
	case status := <-finishStatus:
		assert.Equal(t, string(api.RunCancelled), status, "FinishRun should be called with Cancelled")
	case <-time.After(3 * time.Second):
		t.Fatal("FinishRun was not called within 3 seconds of executeRun returning")
	}
}
