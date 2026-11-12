package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
)

func TestLocateNativeShell_Windows(t *testing.T) {
	gitBash := func(string) (string, error) { return `C:\Git\bin\bash.exe`, nil }
	wsl := func(string) (string, error) { return `C:\Windows\System32\bash.exe`, nil }

	p, ok := locateNativeShell("windows", gitBash, alwaysFailExists, noHomeDir)
	assert.True(t, ok)
	assert.Equal(t, `C:\Git\bin\bash.exe`, p)

	_, ok = locateNativeShell("windows", alwaysFailLookPath, alwaysFailExists, noHomeDir)
	assert.False(t, ok, "no Git Bash anywhere: not found")

	_, ok = locateNativeShell("windows", wsl, alwaysFailExists, noHomeDir)
	assert.False(t, ok, "the WSL launcher alone must be treated as not found")
}

func TestLocateNativeShell_Unix(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		lookPath := func(name string) (string, error) {
			assert.Equal(t, "bash", name)
			return "/usr/bin/bash", nil
		}
		p, ok := locateNativeShell(goos, lookPath, alwaysFailExists, noHomeDir)
		assert.True(t, ok, goos)
		assert.Equal(t, "/usr/bin/bash", p, goos)

		_, ok = locateNativeShell(goos, alwaysFailLookPath, alwaysFailExists, noHomeDir)
		assert.False(t, ok, "%s without bash: not found", goos)
	}
}

func TestFindShell_NeverBareBash(t *testing.T) {
	if s := findShell(); s != "" {
		assert.NotEqual(t, "bash", s, "findShell must return the resolved path, never a bare name")
	}
}

func TestFilterNative_NoNativeShell(t *testing.T) {
	caps := []string{"native", "container"}
	assert.Equal(t, []string{"container"}, (&Agent{NoNativeShell: true}).filterNative(caps))
	assert.Empty(t, (&Agent{NoNativeShell: true}).filterNative([]string{"native"}))
}

// No native shell but a container runtime: the agent drops native and still
// advertises container.
func TestCapabilities_NoNativeShellWithRuntime(t *testing.T) {
	a := &Agent{PauseImage: "p", NoNativeShell: true}
	a.resolvedRuntime = &probeFakeRT{}
	a.runtimeOnce.Do(func() {})
	assert.Equal(t, []string{"container"}, a.capabilities())
}

func TestRun_NoNativeShellAndNoRuntimeRefusesToStart(t *testing.T) {
	a := &Agent{ID: "x", RuntimePref: "no-such-runtime", NoNativeShell: true, Client: NewClient("http://127.0.0.1:1", "tok")}
	err := a.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no native shell")
	assert.Contains(t, err.Error(), "no container runtime")
}

func TestRunStep_NoShellFailsClearly(t *testing.T) {
	// Pre-resolve the cached path as "not found" for this test.
	nativeShellOnce.Do(func() {})
	prev := nativeShellPath
	nativeShellPath = ""
	t.Cleanup(func() { nativeShellPath = prev })

	code, err := RunStep(context.Background(), "echo hi", nil, nil, nil, nil, "")
	assert.Equal(t, -1, code)
	require.ErrorIs(t, err, errNoNativeShell)
	assert.Contains(t, err.Error(), "no native shell")
}

func TestExecuteRun_NativeClaimRefusedWithoutNativeShell(t *testing.T) {
	const agentID = "ns-agent"
	const runID = "run-ns"
	var mu sync.Mutex
	var finished string
	var logs []string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/-1/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		var reqs []api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&reqs)
		mu.Lock()
		for _, l := range reqs {
			logs = append(logs, l.Line)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		finished = req.Status
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok"), NoNativeShell: true}
	claim := api.ClaimResponse{
		Native: true, RunID: runID, JobName: "j",
		Stages: []api.ClaimStage{{Step: &api.ClaimStep{Index: 0, Name: "s", Run: "echo hi"}}},
	}
	a.executeRun(context.Background(), claim, t.TempDir())

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, string(api.RunFailed), finished)
	require.NotEmpty(t, logs)
	assert.Contains(t, logs[0], "no native shell")
}

