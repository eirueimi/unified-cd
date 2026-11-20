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
	return downloadRunIDHarnessMode(t, stepRunID, serveRunID, false)
}

// stubDirectArtifactRoutes serves a direct-mode artifact download for
// presign requests made on the claimed run: the presign path must name
// claimedRun and sourceRunId must be wantSource ("" for the claimed run's
// own artifact). The presigned URL points back at this server's /blob route.
func stubDirectArtifactRoutes(t *testing.T, mux *http.ServeMux, claimedRun, wantSource, name string, payload []byte, hits *int32) {
	t.Helper()
	mux.HandleFunc("POST /api/v1/runs/{run}/objects/presign", func(w http.ResponseWriter, r *http.Request) {
		var req api.PresignRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		assert.Equal(t, claimedRun, r.PathValue("run"), "presign must be requested on the claimed run")
		assert.Equal(t, wantSource, req.SourceRunID, "sourceRunId must be the artifact's run")
		assert.Equal(t, name, req.Name)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.PresignResponse{
			Mode:    api.ObjectModeDirect,
			Request: &api.PresignedRequest{Method: http.MethodGet, URL: "http://" + r.Host + "/blob/a"},
		})
	})
	mux.HandleFunc("GET /blob/a", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		_, _ = w.Write(payload)
	})
}

func downloadRunIDHarnessMode(t *testing.T, stepRunID, serveRunID string, direct bool) (finalStatus string, artifactHits *int32, workDir string) {
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
	if direct {
		wantSource := serveRunID
		if serveRunID == runID {
			wantSource = ""
		}
		stubDirectArtifactRoutes(t, mux, runID, wantSource, "bin", makeAgentTestTarZstd(t, map[string]string{"bin.txt": "child-binary"}), &hits)
	} else {
		mux.HandleFunc("GET /api/v1/runs/"+serveRunID+"/artifacts/bin", func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			w.Write(makeAgentTestTarZstd(t, map[string]string{"bin.txt": "child-binary"})) //nolint:errcheck
		})
	}
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

	cli := NewClient(srv.URL, "tok")
	if !direct {
		cli.SetObjectTransferController(true)
	}
	a := &Agent{ID: agentID, Client: cli}
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

// The same two cases in direct mode: the presign request goes to the CLAIMED
// run's endpoint and carries the artifact's run as sourceRunId.
func TestExecuteRun_DownloadArtifact_DirectMode_RunIDOverride(t *testing.T) {
	status, hits, workDir := downloadRunIDHarnessMode(t, "run-other", "run-other", true)
	assert.Equal(t, "Succeeded", status)
	assert.EqualValues(t, 1, atomic.LoadInt32(hits))
	got, err := os.ReadFile(filepath.Join(workDir, "bin.txt"))
	require.NoError(t, err)
	assert.Equal(t, "child-binary", string(got))
}

func TestExecuteRun_DownloadArtifact_DirectMode_EmptyRunIDUsesCurrentRun(t *testing.T) {
	status, hits, _ := downloadRunIDHarnessMode(t, "", "run-parent", true)
	assert.Equal(t, "Succeeded", status)
	assert.EqualValues(t, 1, atomic.LoadInt32(hits))
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

// Full flow: a call step records its child run ID, and a later
// downloadArtifact with runId: "{{ .Steps.<call>.ChildRunID }}" downloads
// the child run's artifact.
func TestExecuteRun_CallThenDownloadChildArtifact(t *testing.T) {
	callThenDownloadChildArtifact(t, false)
}

func TestExecuteRun_CallThenDownloadChildArtifact_DirectMode(t *testing.T) {
	callThenDownloadChildArtifact(t, true)
}

func callThenDownloadChildArtifact(t *testing.T, direct bool) {
	const agentID = "call-dl-agent"
	const runID = "run-parent"
	const childRunID = "run-child-42"
	workDir := t.TempDir()

	finishCh := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/children", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.Run{ID: childRunID, Status: api.RunSucceeded}) //nolint:errcheck
	})
	mux.HandleFunc("GET /api/v1/runs/"+childRunID, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.Run{ID: childRunID, Status: api.RunSucceeded}) //nolint:errcheck
	})
	mux.HandleFunc("GET /api/v1/runs/"+childRunID+"/outputs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.RunOutputs{Outputs: map[string]string{}}) //nolint:errcheck
	})
	var dlHits int32
	if direct {
		stubDirectArtifactRoutes(t, mux, runID, childRunID, "app-binary", makeAgentTestTarZstd(t, map[string]string{"app.txt": "from-child"}), &dlHits)
	} else {
		mux.HandleFunc("GET /api/v1/runs/"+childRunID+"/artifacts/app-binary", func(w http.ResponseWriter, r *http.Request) {
			w.Write(makeAgentTestTarZstd(t, map[string]string{"app.txt": "from-child"})) //nolint:errcheck
		})
	}
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

	cli := NewClient(srv.URL, "tok")
	if !direct {
		cli.SetObjectTransferController(true)
	}
	a := &Agent{ID: agentID, Client: cli}
	resp := api.ClaimResponse{
		Native:  true,
		RunID:   runID,
		JobName: "test-call-dl",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index: 0, StageIndex: 0, Name: "build_app",
				Call: &api.ClaimCallStep{Job: "build"},
			}},
			{Step: &api.ClaimStep{
				Index: 1, StageIndex: 1, Name: "fetch",
				DownloadArtifact: &api.DownloadArtifactStep{
					Name:  "app-binary",
					RunID: "{{ .Steps.build_app.ChildRunID }}",
				},
			}},
		},
	}
	a.executeRun(context.Background(), resp, workDir)

	select {
	case status := <-finishCh:
		assert.Equal(t, "Succeeded", status)
	default:
		t.Fatal("FinishRun was not called")
	}
	got, err := os.ReadFile(filepath.Join(workDir, "app.txt"))
	require.NoError(t, err)
	assert.Equal(t, "from-child", string(got))
}
