package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

func TestFilterNative(t *testing.T) {
	caps := []string{"native", "container", "securitycontext"}
	assert.Equal(t, caps, (&Agent{}).filterNative(caps), "default keeps native")
	assert.Equal(t, []string{"container", "securitycontext"}, (&Agent{DisableNative: true}).filterNative(caps))
	assert.Empty(t, (&Agent{DisableNative: true}).filterNative([]string{"native"}))
}

func TestCapabilities_DisableNativeOmitsNative(t *testing.T) {
	// An unknown runtime preference makes runtime detection fail, leaving only
	// the native capability on a default agent.
	a := &Agent{RuntimePref: "no-such-runtime"}
	assert.Equal(t, []string{"native"}, a.capabilities())
	a = &Agent{RuntimePref: "no-such-runtime", DisableNative: true}
	assert.Empty(t, a.capabilities())
}

func TestExecuteRun_NativeClaimRefusedWhenNativeDisabled(t *testing.T) {
	const agentID = "dn-agent"
	const runID = "run-dn"
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

	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok"), DisableNative: true}
	claim := api.ClaimResponse{
		Native: true, RunID: runID, JobName: "j",
		Stages: []api.ClaimStage{{Step: &api.ClaimStep{Index: 0, Name: "s", Run: "echo hi"}}},
	}
	a.executeRun(context.Background(), claim, t.TempDir())

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, string(api.RunFailed), finished)
	require.NotEmpty(t, logs)
	assert.Contains(t, logs[0], "native execution is disabled")
}

func TestRun_RefusesToStartWithNoCapabilities(t *testing.T) {
	a := &Agent{ID: "x", RuntimePref: "no-such-runtime", DisableNative: true, Client: NewClient("http://127.0.0.1:1", "tok")}
	err := a.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no capabilities")
}

// A native claim that reaches a disabled-native agent is failed before the
// workspace is prepared: preparing can wipe a root volume, which a refused run
// must not do.
func TestRunLoop_DisabledNativeClaimFailsBeforeWorkspacePrep(t *testing.T) {
	orig := prepareWorkspaceFn
	defer func() { prepareWorkspaceFn = orig }()
	var prepared atomic.Int32
	prepareWorkspaceFn = func(ctx context.Context, workDir, mode string, vol *volumeWorkspace, clean bool, rtFn func() (crt.ContainerRuntime, error), toolsDir string) error {
		prepared.Add(1)
		return orig(ctx, workDir, mode, vol, clean, rtFn, toolsDir)
	}

	finishCh := make(chan string, 2)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", registerHandler)
	var claims atomic.Int32
	mux.HandleFunc("POST /api/v1/agents/dn2/claim", func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		w.Header().Set("Content-Type", "application/json")
		if claims.Add(1) == 1 {
			json.NewEncoder(w).Encode(claimResp("run-dn2", "echo hi")) //nolint:errcheck
			return
		}
		<-r.Context().Done()
		json.NewEncoder(w).Encode(api.ClaimResponse{}) //nolint:errcheck
	})
	mux.HandleFunc("POST /api/v1/agents/dn2/steps", stepHandler)
	mux.HandleFunc("POST /api/v1/agents/dn2/runs/run-dn2/steps/-1/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/dn2/runs/run-dn2/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Status string }
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case finishCh <- body.Status:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := &Agent{ID: "dn2", Client: NewClient(srv.URL, "tok"), MaxConcurrent: 1, MaxDetachedConcurrent: -1,
		WorkspaceDir: t.TempDir(), DisableNative: true}
	// A stub runtime keeps the agent's capability set non-empty (container only).
	a.runtimeOnce.Do(func() { a.resolvedRuntime = &podFakeRT{} })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	select {
	case st := <-finishCh:
		assert.Equal(t, "Failed", st)
	case <-time.After(10 * time.Second):
		t.Fatal("run was not finished")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return")
	}
	assert.Zero(t, prepared.Load(), "workspace must not be prepared for a refused native claim")
}
