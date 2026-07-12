//go:build integration

package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

// TestExecuteRun_ScopedStep_RealRuntimeRoundTrip is a real-Docker/Podman
// round-trip: a scoped step writes a file inside the isolated scope
// container, and a scoped upload-artifact captures it. It is gated behind the
// "integration" build tag AND skips at runtime if no container runtime is
// detected, so it never runs in ordinary `go test ./...` CI.
func TestExecuteRun_ScopedStep_RealRuntimeRoundTrip(t *testing.T) {
	if _, err := crt.Detect(""); err != nil {
		t.Skipf("no container runtime available, skipping: %v", err)
	}

	const agentID = "scoped-integration-agent"
	const runID = "run-scoped-integration"

	var uploadedBody []byte
	var uploadReceived bool
	finishCh := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /api/v1/runs/"+runID+"/artifacts/out", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		uploadedBody = b
		uploadReceived = true
		w.WriteHeader(http.StatusNoContent)
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
	defer srv.Close()

	a := &Agent{
		ID:       agentID,
		Client:   NewClient(srv.URL, "tok"),
		ToolsDir: installShimOrSkip(t),
	}

	resp := api.ClaimResponse{
		Native:  true,
		RunID:   runID,
		JobName: "test-scoped-integration",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{
				Index:      0,
				StageIndex: 0,
				Name:       "write-in-scope",
				ScopeID:    "scope:build",
				ScopeImage: "alpine:3",
				Run:        "echo -n 'hello from scope' > /tmp/out.txt",
			}},
			{Step: &api.ClaimStep{
				Index:      1,
				StageIndex: 1,
				Name:       "upload-from-scope",
				ScopeID:    "scope:build",
				ScopeImage: "alpine:3",
				UploadArtifact: &api.UploadArtifactStep{
					Name: "out",
					Path: "/tmp/out.txt",
				},
			}},
		},
	}

	a.executeRun(context.Background(), resp, t.TempDir())

	select {
	case status := <-finishCh:
		if status != "Succeeded" {
			t.Fatalf("expected Succeeded, got %s", status)
		}
	default:
		t.Fatal("FinishRun was not called")
	}
	if !uploadReceived {
		t.Fatal("upload-artifact never reached the server")
	}
	if len(uploadedBody) == 0 {
		t.Fatal("expected non-empty uploaded artifact body")
	}
}
