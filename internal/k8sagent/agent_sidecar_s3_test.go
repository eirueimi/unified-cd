package k8sagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
)

// sidecarS3Controller is a minimal fake controller for the fail-fast tests: it
// records the run-level (step -1) log lines the agent appends and the terminal
// status it reports, which is everything these tests assert on.
type sidecarS3Controller struct {
	srv *httptest.Server

	mu       sync.Mutex
	runLog   []string
	finishCh chan api.RunStatus
}

func newSidecarS3Controller(t *testing.T, agentID, runID string) *sidecarS3Controller {
	t.Helper()
	c := &sidecarS3Controller{finishCh: make(chan api.RunStatus, 1)}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/-1/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		var lines []api.LogAppendRequest
		if err := json.NewDecoder(r.Body).Decode(&lines); err == nil {
			c.mu.Lock()
			for _, l := range lines {
				c.runLog = append(c.runLog, l.Line)
			}
			c.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		select {
		case c.finishCh <- api.RunStatus(body["status"]):
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// Catch-all so any other bookkeeping call (step reports, other log
	// streams) succeeds rather than erroring the agent into a retry loop.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	c.srv = httptest.NewServer(mux)
	t.Cleanup(c.srv.Close)
	return c
}

func (c *sidecarS3Controller) runLogText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.runLog, "\n")
}

// TestExecuteRun_ArtifactStepsNoLongerNeedSidecarSecret: the sidecar holds no
// store credentials any more, so a claim with an unconditional artifact upload
// step needs no sidecar Secret configuration. The agent must create the Pod
// and must not fail the run at claim time (it used to, for every artifact step,
// when sidecarS3SecretName was unset: the failure mode this pins after the
// upgrade).
func TestExecuteRun_ArtifactStepsNoLongerNeedSidecarSecret(t *testing.T) {
	const agentID = "k8s-artifact-nosecret"
	const runID = "run-artifact-nosecret"

	ctl := newSidecarS3Controller(t, agentID, runID)
	// waitErr stops executeRun at pod acquisition: the test is about the
	// absence of a claim-time failure, not about driving a whole claim.
	pm := &fakePM{waitErr: assert.AnError}
	a := &K8sAgent{
		cfg:    Config{AgentID: agentID, Namespace: "ci", PodImage: "ubuntu:22.04", ShimImage: "shim", PodStartTimeout: "50ms"},
		client: agentlib.NewClient(ctl.srv.URL, "tok"),
		pm:     pm,
		exec:   &fakeExec{},
	}

	prevInitial, prevMax := agentlib.RetryInitialWait, agentlib.RetryMaxWait
	agentlib.RetryInitialWait, agentlib.RetryMaxWait = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { agentlib.RetryInitialWait, agentlib.RetryMaxWait = prevInitial, prevMax })

	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "build",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "build", Run: "make"}},
			{Step: &api.ClaimStep{
				Index: 1, StageIndex: 1, Name: "publish",
				UploadArtifact: &api.UploadArtifactStep{Name: "app", Path: "bin/app"},
			}},
			{Step: &api.ClaimStep{
				Index: 2, StageIndex: 2, Name: "restore-deps",
				Cache: &dsl.CacheStep{Key: "go-{{ .Params.sha }}", Path: "vendor"},
			}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.executeRun(ctx, claim)

	assert.Positive(t, pm.creations(), "an artifact claim without sidecar configuration must proceed to Pod creation")
	msg := ctl.runLogText()
	assert.NotContains(t, msg, "sidecarS3SecretName", "no warning or failure may point at the removed setting")
	assert.NotContains(t, msg, `"publish"`, "the artifact step must not be flagged at claim time")
}

// TestExecuteRun_HeartbeatKickedAfterPodBinding: the controller learns a
// run/Pod binding only through a heartbeat, and the sidecar's first presign
// request is answered 409 binding-pending until it does. Recording a binding
// must therefore send a heartbeat at once, not at the next 15s tick.
func TestExecuteRun_HeartbeatKickedAfterPodBinding(t *testing.T) {
	const agentID = "k8s-hb-kick"
	const runID = "run-hb-kick"

	var mu sync.Mutex
	var beats []api.HeartbeatRequest
	boundBeat := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var req api.HeartbeatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		beats = append(beats, req)
		mu.Unlock()
		if _, ok := req.PodBindings[runID]; ok {
			select {
			case boundBeat <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	gate := make(chan struct{})
	pm := &fakePM{blockFirstWait: gate, waitStarted: make(chan struct{})}
	a := &K8sAgent{
		cfg:         Config{AgentID: agentID, Namespace: "ci", PodImage: "ubuntu:22.04", ShimImage: "shim", PodStartTimeout: "30s"},
		client:      agentlib.NewClient(srv.URL, "tok"),
		pm:          pm,
		exec:        &fakeExec{},
		podBindings: newPodBindingSet(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The production interval: only the kick can deliver the binding in time.
	hbDone := agentlib.StartHeartbeatKicked(ctx, a.client, agentID, agentlib.DefaultHeartbeatInterval,
		a.buildHeartbeat(func() []string { return []string{runID} }), nil, a.podBindings.Kicks())

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		a.executeRun(ctx, api.ClaimResponse{
			RunID: runID, JobName: "build",
			Stages: []api.ClaimStage{{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "build", Run: "make"}}},
		})
	}()

	select {
	case <-boundBeat:
	case <-time.After(time.Second):
		mu.Lock()
		n := len(beats)
		mu.Unlock()
		t.Fatalf("no heartbeat carrying the binding within 1s of the Pod being created (%d heartbeats seen)", n)
	}
	close(gate)
	cancel()
	<-hbDone
	<-runDone
}
