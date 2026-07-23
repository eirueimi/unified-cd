package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tokenSourceFunc func(context.Context) (string, error)

func (f tokenSourceFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

type invalidatingTestTokenSource struct {
	token         string
	tokenCalls    int
	invalidations int
}

func (s *invalidatingTestTokenSource) Token(context.Context) (string, error) {
	s.tokenCalls++
	return s.token, nil
}

func (s *invalidatingTestTokenSource) Invalidate() { s.invalidations++ }

func TestClientDoesNotReplayUnauthorizedPost(t *testing.T) {
	source := &invalidatingTestTokenSource{token: "access-1"}
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewClientWithTokenSource(srv.URL, source, srv.Client())
	err := c.Register(t.Context(), api.AgentRegisterRequest{AgentID: "agent-1"})
	require.Error(t, err)
	assert.Equal(t, 1, requests)
	assert.Equal(t, 1, source.tokenCalls)
	assert.Zero(t, source.invalidations)
}

func TestClientReadsTokenForEveryRequest(t *testing.T) {
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := 0
	c := NewClientWithTokenSource(srv.URL, tokenSourceFunc(func(context.Context) (string, error) {
		n++
		return "access-" + strconv.Itoa(n), nil
	}), srv.Client())

	require.NoError(t, c.Register(t.Context(), api.AgentRegisterRequest{AgentID: "a"}))
	require.NoError(t, c.Heartbeat(t.Context(), "a", []string{}))
	assert.Equal(t, []string{"Bearer access-1", "Bearer access-2"}, tokens)
}

func TestClient_Register(t *testing.T) {
	var got api.AgentRegisterRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer t", r.Header.Get("Authorization"))
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")
	require.NoError(t, c.Register(t.Context(), api.AgentRegisterRequest{AgentID: "a", Hostname: "h", OS: "linux"}))
	assert.Equal(t, "a", got.AgentID)
}

func TestClient_Heartbeat_NonEmptyActiveRuns(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")
	require.NoError(t, c.Heartbeat(t.Context(), "a1", []string{"r1"}))

	var got api.HeartbeatRequest
	require.NoError(t, json.Unmarshal(gotBody, &got))
	assert.Equal(t, []string{"r1"}, got.ActiveRunIDs)
}

func TestClient_Heartbeat_EmptyActiveRuns_SendsBody(t *testing.T) {
	var contentLength int64
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentLength = r.ContentLength
		b, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		gotBody = b
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")
	require.NoError(t, c.Heartbeat(t.Context(), "a1", []string{}))

	// A live agent always sends a JSON body, even for zero active runs, so the
	// controller can tell it apart from a legacy agent that sends none. With no
	// omitempty on ActiveRunIDs, the empty set serializes the field explicitly
	// (`{"activeRunIds":[]}`) and decodes back to a NON-NIL empty slice — the
	// signal the controller relies on, not just a non-zero ContentLength.
	assert.NotEqual(t, int64(0), contentLength)

	var got api.HeartbeatRequest
	require.NoError(t, json.Unmarshal(gotBody, &got))
	assert.NotNil(t, got.ActiveRunIDs)
	assert.Len(t, got.ActiveRunIDs, 0)
	assert.Contains(t, string(gotBody), `"activeRunIds":[]`)
}

func TestClient_Heartbeat_NilActiveRuns_SendsNoBody(t *testing.T) {
	var contentLength int64
	var bodyLen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentLength = r.ContentLength
		b, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		bodyLen = len(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")
	require.NoError(t, c.Heartbeat(t.Context(), "a1", nil))

	assert.Equal(t, int64(0), contentLength)
	assert.Zero(t, bodyLen)
}

func TestClient_Claim_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ClaimResponse{})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")
	resp, err := c.Claim(t.Context(), "a", "1s", nil)
	require.NoError(t, err)
	assert.Empty(t, resp.RunID)
}

func TestClient_AppendLog(t *testing.T) {
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")
	require.NoError(t, c.AppendLog(t.Context(), "a", api.LogAppendRequest{RunID: "r", StepIndex: 0, Stream: "stdout", Line: "x"}))
	assert.Equal(t, 1, count)
}

func TestClient_ReportSidecarStatus(t *testing.T) {
	var gotPath string
	var got api.SidecarStatusRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")
	exitCode := 3
	req := api.SidecarStatusRequest{RunID: "r1", Name: "mysql", Index: 100, Phase: "exited", ExitCode: &exitCode}
	require.NoError(t, c.ReportSidecarStatus(t.Context(), "a1", req))
	assert.Equal(t, "/api/v1/agents/a1/runs/r1/sidecars", gotPath)
	assert.Equal(t, "mysql", got.Name)
	assert.Equal(t, 100, got.Index)
	assert.Equal(t, "exited", got.Phase)
	require.NotNil(t, got.ExitCode)
	assert.Equal(t, 3, *got.ExitCode)
}

func TestClient_CreateChildRun(t *testing.T) {
	var got api.TriggerRunRequest
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(api.Run{ID: "run-123", Status: api.RunPending})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")
	run, err := c.CreateChildRun(t.Context(), "agent-1", "parent-run-1", "hello", map[string]string{"k": "v"})
	require.NoError(t, err)
	assert.Equal(t, "run-123", run.ID)
	assert.Equal(t, "/api/v1/agents/agent-1/runs/parent-run-1/children", gotPath)
	assert.Equal(t, "hello", got.JobName)
	assert.Equal(t, "v", got.Params["k"])
}

// TestClient_CreateChildRun_ParentTerminal verifies that when the parent run is
// already terminal, the controller's alreadyFinalized acknowledgement (no child
// run created) is surfaced as ErrParentRunAlreadyTerminal — not a nil error
// with an empty-ID Run, which would send ExecuteCallStep polling a nonexistent
// run ID.
func TestClient_CreateChildRun_ParentTerminal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mirror respondRunWriteVerdict's runWriteTerminal branch.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"runId": "parent-run-1", "alreadyFinalized": true})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")

	run, err := c.CreateChildRun(t.Context(), "agent-1", "parent-run-1", "hello", nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrParentRunAlreadyTerminal), "want ErrParentRunAlreadyTerminal, got %v", err)
	assert.Empty(t, run.ID, "no child run is created when the parent is terminal")
}

func TestClient_GetRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.Run{ID: "run-123", Status: api.RunSucceeded})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")
	run, err := c.GetRun(t.Context(), "run-123")
	require.NoError(t, err)
	assert.Equal(t, api.RunSucceeded, run.Status)
}

func TestClient_GetRunOutputs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.RunOutputs{RunID: "run-123", Outputs: map[string]string{"k": "v"}})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")
	outputs, err := c.GetRunOutputs(t.Context(), "run-123")
	require.NoError(t, err)
	assert.Equal(t, "v", outputs["k"])
}

func TestClient_SetStepOutputs(t *testing.T) {
	var got api.SetOutputsRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")
	require.NoError(t, c.SetStepOutputs(t.Context(), "a", "run-1", 0, "", map[string]string{"k": "v"}))
	assert.Equal(t, "v", got.Outputs["k"])
}

func TestClient_SetRunOutputs(t *testing.T) {
	var got api.SetOutputsRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")
	require.NoError(t, c.SetRunOutputs(t.Context(), "a", "run-1", map[string]string{"result": "ok"}))
	assert.Equal(t, "ok", got.Outputs["result"])
}

func TestClient_AppendLogBulk(t *testing.T) {
	var got []api.LogAppendRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")
	lines := []api.LogAppendRequest{
		{RunID: "r", StepIndex: 0, Stream: "stdout", Line: "a"},
		{RunID: "r", StepIndex: 0, Stream: "stdout", Line: "b"},
	}
	require.NoError(t, c.AppendLogBulk(t.Context(), "agent-1", "r", 0, lines))
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].Line)
	assert.Equal(t, "b", got[1].Line)
}

func TestClient_FetchSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req api.AgentFetchSecretsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		assert.Equal(t, "run-1", req.RunID)
		_ = json.NewEncoder(w).Encode(api.AgentFetchSecretsResponse{
			Secrets: map[string]string{"AWS_KEY": "AKID1234"},
		})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")
	result, err := c.FetchSecrets(t.Context(), "a1", "run-1", []string{"AWS_KEY"})
	require.NoError(t, err)
	assert.Equal(t, "AKID1234", result["AWS_KEY"])
}

func TestClient_FetchSecrets_Empty(t *testing.T) {
	c := NewClient("http://localhost", "t")
	result, err := c.FetchSecrets(t.Context(), "a1", "run-1", nil)
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestClient_UploadArtifact_StreamsChunked(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("payload"), 0o600))

	var gotLen int64
	var extracted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLen = r.ContentLength // -1 for chunked (no Content-Length)
		dest := t.TempDir()
		if err := artifact.ExtractTarZstd(r.Body, dest); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		b, _ := os.ReadFile(filepath.Join(dest, "a.txt"))
		extracted = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "t")
	require.NoError(t, c.UploadArtifact(t.Context(), "run1", "art1", src))
	assert.Equal(t, int64(-1), gotLen, "body must be chunked (no Content-Length)")
	assert.Equal(t, "payload", extracted)
}

func TestClient_UploadArtifact_HTTPErrorSurfaces(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("x"), 0o600))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "t")
	err := c.UploadArtifact(t.Context(), "run1", "art1", src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "502")
}
