package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestClient_CreateRun(t *testing.T) {
	var got api.TriggerRunRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(api.Run{ID: "run-123", Status: api.RunPending})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")
	run, err := c.CreateRun(t.Context(), "hello", map[string]string{"k": "v"})
	require.NoError(t, err)
	assert.Equal(t, "run-123", run.ID)
	assert.Equal(t, "hello", got.JobName)
	assert.Equal(t, "v", got.Params["k"])
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
		_ = json.NewEncoder(w).Encode(api.AgentFetchSecretsResponse{
			Secrets: map[string]string{"AWS_KEY": "AKID1234"},
		})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")
	result, err := c.FetchSecrets(t.Context(), "a1", []string{"AWS_KEY"})
	require.NoError(t, err)
	assert.Equal(t, "AKID1234", result["AWS_KEY"])
}

func TestClient_FetchSecrets_Empty(t *testing.T) {
	c := NewClient("http://localhost", "t")
	result, err := c.FetchSecrets(t.Context(), "a1", nil)
	require.NoError(t, err)
	assert.Empty(t, result)
}
