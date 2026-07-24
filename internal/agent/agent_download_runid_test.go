package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// downloadRunIDHarness drives a single-step downloadArtifact claim through
// the fake-controller harness. It serves artifact "bin" for exactly one run
// ID (serveRunID) and records how many artifact GETs arrived plus the final
// run status.
func downloadRunIDHarness(t *testing.T, stepRunID, serveRunID string) (finalStatus string, artifactHits *int32, workDir string) {
	t.Helper()
	const agentID = "dl-runid-agent"
	const runID = "run-parent"
	workDir = t.TempDir()

	var hits int32
	finishCh := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/runs/"+serveRunID+"/artifacts/bin", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write(makeAgentTestTarZstd(t, map[string]string{"bin.txt": "child-binary"})) //nolint:errcheck
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case finishCh <- body.Status:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}
	resp := api.ClaimResponse{
		Native:  true,
		RunID:   runID,
		JobName: "test-dl-runid",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0, StageIndex: 0, Name: "fetch",
				DownloadArtifact: &api.DownloadArtifactStep{Name: "bin", RunID: stepRunID},
			}},
		},
	}
	a.executeRun(context.Background(), resp, workDir)

	select {
	case finalStatus = <-finishCh:
	default:
		t.Fatal("FinishRun was not called")
	}
	return finalStatus, &hits, workDir
}

// A literal runId downloads from that run, not the current one.
func TestExecuteRun_DownloadArtifact_RunIDOverride(t *testing.T) {
	status, hits, workDir := downloadRunIDHarness(t, "run-other", "run-other")
	assert.Equal(t, "Succeeded", status)
	assert.EqualValues(t, 1, atomic.LoadInt32(hits))
	got, err := os.ReadFile(filepath.Join(workDir, "bin.txt"))
	require.NoError(t, err)
	assert.Equal(t, "child-binary", string(got))
}

// Empty runId keeps downloading from the current run (regression guard).
func TestExecuteRun_DownloadArtifact_EmptyRunIDUsesCurrentRun(t *testing.T) {
	status, hits, _ := downloadRunIDHarness(t, "", "run-parent")
	assert.Equal(t, "Succeeded", status)
	assert.EqualValues(t, 1, atomic.LoadInt32(hits))
}

// A runId template that fails to expand fails the step without any request.
func TestExecuteRun_DownloadArtifact_BadRunIDTemplate_Fails(t *testing.T) {
	status, hits, _ := downloadRunIDHarness(t, "{{ .Steps.missing.ChildRunID.bogus }}", "run-parent")
	assert.Equal(t, "Failed", status)
	assert.EqualValues(t, 0, atomic.LoadInt32(hits))
}

// An expanded runId that violates ^[A-Za-z0-9_-]{1,64}$ (path traversal,
// URL structure characters, empty expansion) fails the step without any
// request reaching the artifact endpoint.
func TestExecuteRun_DownloadArtifact_InvalidRunIDValue_Fails(t *testing.T) {
	for _, bad := range []string{"../evil", "a/b", "run id", "{{ .Steps.missing.ChildRunID }}"} {
		t.Run(bad, func(t *testing.T) {
			status, hits, _ := downloadRunIDHarness(t, bad, "run-parent")
			assert.Equal(t, "Failed", status)
			assert.EqualValues(t, 0, atomic.LoadInt32(hits))
		})
	}
}
