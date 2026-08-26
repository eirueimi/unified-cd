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
	"github.com/stretchr/testify/require"
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

// TestExecuteRun_ArtifactClaimWithoutSidecarSecretFailsFast is the regression
// test for the operator's first failure: S3 credentials were in the
// CONTROLLER's Secret, log archiving worked, and then artifact steps died
// mid-run with "artifact requires S3 configuration (UNIFIED_S3_*)" — after the
// job had already done all of its work.
//
// The k8s artifact path does not go through the controller at all: the
// injected unified-artifact sidecar talks to S3 directly using the Secret named
// by cfg.SidecarS3SecretName. With that field empty the run is doomed before it
// starts, so it must fail at claim time, without a Pod ever being created —
// the same treatment `native: true` already gets one line away.
func TestExecuteRun_ArtifactClaimWithoutSidecarSecretFailsFast(t *testing.T) {
	const agentID = "k8s-artifact-nosecret"
	const runID = "run-artifact-nosecret"

	ctl := newSidecarS3Controller(t, agentID, runID)
	pm := &fakePM{}
	a := &K8sAgent{
		cfg:    Config{AgentID: agentID, Namespace: "ci", PodImage: "ubuntu:22.04", SidecarS3SecretName: ""},
		client: agentlib.NewClient(ctl.srv.URL, "tok"),
		pm:     pm,
		exec:   &fakeExec{},
	}

	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "build",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "build", Run: "make"}},
			{Step: &api.ClaimStep{
				Index: 1, StageIndex: 1, Name: "publish",
				UploadArtifact: &api.UploadArtifactStep{Name: "app", Path: "bin/app"},
			}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.executeRun(ctx, claim)

	select {
	case status := <-ctl.finishCh:
		require.Equal(t, api.RunFailed, status)
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun was never called: the claim must fail immediately, not run to the artifact step")
	}

	assert.Nil(t, pm.created, "no Pod may be created: the whole point is failing BEFORE the job does its work")
	assert.Equal(t, 0, pm.creations())

	msg := ctl.runLogText()
	assert.Contains(t, msg, "sidecarS3SecretName", "the failure must name the config field the operator has to set")
	assert.Contains(t, msg, `"publish"`, "the failure must name the offending step")
}

// TestExecuteRun_ArtifactInFinallyFailsFast: `finally` stages are scanned too.
// A run whose only artifact transfer is a finally-stage log upload is just as
// doomed, and just as worth catching before the Pod exists.
func TestExecuteRun_ArtifactInFinallyFailsFast(t *testing.T) {
	const agentID = "k8s-artifact-finally"
	const runID = "run-artifact-finally"

	ctl := newSidecarS3Controller(t, agentID, runID)
	pm := &fakePM{}
	a := &K8sAgent{
		cfg:    Config{AgentID: agentID, Namespace: "ci", PodImage: "ubuntu:22.04"},
		client: agentlib.NewClient(ctl.srv.URL, "tok"),
		pm:     pm,
		exec:   &fakeExec{},
	}

	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "build",
		Stages:  []api.ClaimStage{{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "build", Run: "make"}}},
		Finally: []api.ClaimStage{{Parallel: []api.ClaimStep{{
			Index: 1, StageIndex: 1, Name: "collect-logs",
			DownloadArtifact: &api.DownloadArtifactStep{Name: "logs"},
		}}}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.executeRun(ctx, claim)

	select {
	case status := <-ctl.finishCh:
		require.Equal(t, api.RunFailed, status)
	case <-time.After(5 * time.Second):
		t.Fatal("FinishRun was never called for a finally-stage artifact step")
	}
	assert.Equal(t, 0, pm.creations(), "no Pod may be created")
	assert.Contains(t, ctl.runLogText(), `"collect-logs"`)
}

// TestExecuteRun_CacheClaimWithoutSidecarSecretWarnsButProceeds pins the
// deliberate asymmetry: cache is best-effort by design, so an unreachable
// store must NOT fail the run the way an artifact transfer does. The operator
// still gets one loud per-run warning in the run's own log, because the
// alternative — what production actually did — was months of full build times
// behind a UI that said "cache hit".
func TestExecuteRun_CacheClaimWithoutSidecarSecretWarnsButProceeds(t *testing.T) {
	const agentID = "k8s-cache-nosecret"
	const runID = "run-cache-nosecret"

	ctl := newSidecarS3Controller(t, agentID, runID)
	// waitErr stops executeRun right after the warning, at pod acquisition —
	// this test is about the warning and the absence of a FAIL-FAST, not about
	// driving a whole claim.
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
			{Step: &api.ClaimStep{
				Index: 0, StageIndex: 0, Name: "restore-deps",
				Cache: &dsl.CacheStep{Key: "go-{{ .Params.sha }}", Path: "vendor"},
			}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.executeRun(ctx, claim)

	msg := ctl.runLogText()
	assert.Contains(t, msg, "sidecarS3SecretName", "a cache-only claim must still warn loudly, once per run")
	assert.Contains(t, msg, "cache step", "the warning must say what it is about")
	// The claim proceeded to pod acquisition rather than being rejected up
	// front: cache never fails a run.
	assert.Positive(t, pm.creations(), "a cache-only claim must NOT be fail-fasted; it proceeds to pod acquisition")
}

// TestScanTransferSteps covers the claim walk itself: stages and finally, plain
// steps and parallel members, with cache counted separately from artifacts
// because the two have opposite policies.
func TestScanTransferSteps(t *testing.T) {
	c := api.ClaimResponse{
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Name: "plain", Run: "make"}},
			{Step: &api.ClaimStep{Name: "up", UploadArtifact: &api.UploadArtifactStep{Name: "a"}}},
			{Parallel: []api.ClaimStep{
				{Name: "par-down", DownloadArtifact: &api.DownloadArtifactStep{Name: "b"}},
				{Name: "par-cache", Cache: &dsl.CacheStep{Key: "k", Path: "p"}},
			}},
			// A call: step spawns a CHILD run with its own claim, which runs this
			// check for itself — nothing to recurse into here.
			{Step: &api.ClaimStep{Name: "child", Call: &api.ClaimCallStep{Job: "other"}}},
		},
		Finally: []api.ClaimStage{
			{Step: &api.ClaimStep{Name: "fin-up", UploadArtifact: &api.UploadArtifactStep{Name: "c"}}},
			{Step: &api.ClaimStep{Name: "fin-cache", Cache: &dsl.CacheStep{Key: "k2", Path: "p2"}}},
		},
	}

	artifacts, caches := scanTransferSteps(c)
	assert.Equal(t, []string{`"up"`, `"par-down"`, `"fin-up"`}, artifacts)
	assert.Equal(t, 2, caches)

	none, zero := scanTransferSteps(api.ClaimResponse{
		Stages: []api.ClaimStage{{Step: &api.ClaimStep{Name: "plain", Run: "make"}}},
	})
	assert.Empty(t, none)
	assert.Zero(t, zero)
}
