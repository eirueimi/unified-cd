package k8sagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// secretsPM is a minimal podManager fake: one generated run pod, no scope pods
// needed for these tests.
type secretsPM struct {
	mu   sync.Mutex
	name string
}

func (f *secretsPM) CreatePod(_ context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := pod.DeepCopy()
	if out.Name == "" {
		out.Name = pod.GenerateName + "generated"
	}
	f.name = out.Name
	return out, nil
}
func (f *secretsPM) WaitForPodRunning(_ context.Context, _ string) error { return nil }
func (f *secretsPM) DeletePod(_ context.Context, _ string) error        { return nil }
func (f *secretsPM) ListPods(_ context.Context, _ string) (*corev1.PodList, error) {
	return &corev1.PodList{}, nil
}

// secretsExec is a stepExecutor fake that writes a caller-supplied line to
// stdout and stderr for every ExecStep call, so tests can assert what
// actually reaches the fake server after masking.
type secretsExec struct {
	mu         sync.Mutex
	stdoutLine string
	stderrLine string
	// stderrDelay, if set, sleeps before writing the stderr line — used by the
	// auto-flush test to prove the line reaches the server before ExecStep
	// (i.e. the step) returns.
	stderrDelay time.Duration
	scripts     []string
}

func (f *secretsExec) ExecStep(_ context.Context, _, _, script string, _ []string, _ []string, stdout, stderr io.Writer) (int, error) {
	f.mu.Lock()
	f.scripts = append(f.scripts, script)
	f.mu.Unlock()
	if f.stdoutLine != "" {
		_, _ = stdout.Write([]byte(f.stdoutLine + "\n"))
	}
	if f.stderrLine != "" {
		_, _ = stderr.Write([]byte(f.stderrLine + "\n"))
		if f.stderrDelay > 0 {
			// Hold the step "in progress" after writing the marker, so a test
			// can prove the marker was shipped by auto-flush mid-step rather
			// than only at step end (when ExecStep finally returns).
			time.Sleep(f.stderrDelay)
		}
	}
	return 0, nil
}
func (f *secretsExec) ExecStepArgv(_ context.Context, _, _ string, _ []string, _, _ io.Writer) (int, error) {
	return 0, nil
}

// secretsHarness wires a fake controller HTTP server recording: step reports
// (by name), all stdout/stderr log lines shipped (in received order, with
// receive timestamps for the auto-flush timing test), and the secrets/fetch
// request received (if any).
type secretsHarness struct {
	mu             sync.Mutex
	statuses       map[string]string
	logLines       []string
	logLineTimes   []time.Time
	fetchedNames   []string
	secretsToServe map[string]string
}

func newSecretsHarness() *secretsHarness {
	return &secretsHarness{statuses: map[string]string{}}
}

func (h *secretsHarness) newServer(t *testing.T, agentID, runID string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil && req.StepName != "" {
			h.mu.Lock()
			h.statuses[req.StepName] = req.Status
			h.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// stdout: logLineWriter -> AppendLog (single-line POST)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, r *http.Request) {
		var req api.LogAppendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			h.mu.Lock()
			h.logLines = append(h.logLines, req.Line)
			h.logLineTimes = append(h.logLineTimes, time.Now())
			h.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// stderr: LogPusher -> AppendLogBulk
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		var reqs []api.LogAppendRequest
		if err := json.NewDecoder(r.Body).Decode(&reqs); err == nil {
			h.mu.Lock()
			for _, req := range reqs {
				if req.Line != "" {
					h.logLines = append(h.logLines, req.Line)
					h.logLineTimes = append(h.logLineTimes, time.Now())
				}
			}
			h.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/outputs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/secrets/fetch", func(w http.ResponseWriter, r *http.Request) {
		var req api.AgentFetchSecretsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.mu.Lock()
		h.fetchedNames = append(h.fetchedNames, req.Names...)
		toServe := h.secretsToServe
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.AgentFetchSecretsResponse{Secrets: toServe})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (h *secretsHarness) snapshot() (map[string]string, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	statuses := make(map[string]string, len(h.statuses))
	for k, v := range h.statuses {
		statuses[k] = v
	}
	lines := append([]string(nil), h.logLines...)
	return statuses, lines
}

// TestExecuteRun_SecretsResolveInStepTemplate proves {{ .Secrets.X }} resolves
// in a step's run template on the k8s agent (RED before Fix 1: the k8s agent
// never fetches secrets, so this expands to empty and the step's captured
// script has no secret value in it).
func TestExecuteRun_SecretsResolveInStepTemplate(t *testing.T) {
	const agentID = "k8s-secrets-1"
	const runID = "run-secrets-1"

	h := newSecretsHarness()
	h.secretsToServe = map[string]string{"API_TOKEN": "supersecretvalue"}
	srv := h.newServer(t, agentID, runID)

	pm := &secretsPM{}
	ex := &secretsExec{}
	agentClient := agentlib.NewClient(srv.URL, "tok")

	cfg := Config{AgentID: agentID, Namespace: "ci", PodImage: "ubuntu:22.04"}
	a := &K8sAgent{cfg: cfg, client: agentClient, pm: pm, exec: ex}

	claim := api.ClaimResponse{
		RunID:         runID,
		JobName:       "secrets-test",
		SecretsNeeded: []string{"API_TOKEN"},
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "use-secret", Run: "echo {{ .Secrets.API_TOKEN }}"}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.executeRun(ctx, claim)

	require.Contains(t, h.fetchedNames, "API_TOKEN", "expected the k8s agent to call FetchSecrets for SecretsNeeded")

	ex.mu.Lock()
	defer ex.mu.Unlock()
	require.Len(t, ex.scripts, 1)
	assert.Contains(t, ex.scripts[0], "supersecretvalue", "expected {{ .Secrets.API_TOKEN }} to resolve in the step's run template")
}

// TestExecuteRun_SecretValueMaskedInStdoutAndStderr proves a secret value
// appearing in a step's stdout AND stderr is masked (***) in the lines
// received by the fake server (RED before Fix 2: the k8s agent ships logs
// unmasked).
func TestExecuteRun_SecretValueMaskedInStdoutAndStderr(t *testing.T) {
	const agentID = "k8s-secrets-2"
	const runID = "run-secrets-2"

	h := newSecretsHarness()
	h.secretsToServe = map[string]string{"API_TOKEN": "supersecretvalue"}
	srv := h.newServer(t, agentID, runID)

	pm := &secretsPM{}
	ex := &secretsExec{stdoutLine: "token is supersecretvalue", stderrLine: "err with supersecretvalue"}
	agentClient := agentlib.NewClient(srv.URL, "tok")

	cfg := Config{AgentID: agentID, Namespace: "ci", PodImage: "ubuntu:22.04"}
	a := &K8sAgent{cfg: cfg, client: agentClient, pm: pm, exec: ex}

	claim := api.ClaimResponse{
		RunID:         runID,
		JobName:       "secrets-mask-test",
		SecretsNeeded: []string{"API_TOKEN"},
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "leaky", Run: "echo leaky"}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.executeRun(ctx, claim)

	_, lines := h.snapshot()
	require.NotEmpty(t, lines)
	for _, line := range lines {
		assert.NotContains(t, line, "supersecretvalue", "secret value leaked unmasked in shipped log line: %q", line)
	}
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "***", "expected masked lines to contain the *** placeholder")
}

// TestExecuteRun_StderrFlushesBeforeStepEnds proves stderr lines reach the
// fake server before the step ends (RED before Fix 3: k8s stderr is only
// flushed at step end, via ExecStep's stderrDelay + assert-gap approach
// mirroring the host's stdout-stream time-gap test). The fake ExecStep
// writes a marker to stderr, then sleeps past the (test-shortened) auto-flush
// interval before returning; the test asserts the marker line's receive time
// is measurably earlier than "now" captured immediately after ExecStep
// returns — i.e. it was shipped mid-step, not only at step end.
func TestExecuteRun_StderrFlushesBeforeStepEnds(t *testing.T) {
	prevInterval := stderrAutoFlushInterval
	stderrAutoFlushInterval = 200 * time.Millisecond
	t.Cleanup(func() { stderrAutoFlushInterval = prevInterval })

	const agentID = "k8s-secrets-3"
	const runID = "run-secrets-3"

	h := newSecretsHarness()
	srv := h.newServer(t, agentID, runID)

	pm := &secretsPM{}
	ex := &secretsExec{stderrLine: "mid-step-marker", stderrDelay: 1500 * time.Millisecond}
	agentClient := agentlib.NewClient(srv.URL, "tok")

	cfg := Config{AgentID: agentID, Namespace: "ci", PodImage: "ubuntu:22.04"}
	a := &K8sAgent{cfg: cfg, client: agentClient, pm: pm, exec: ex}

	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "stderr-flush-test",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "slow-stderr", Run: "echo slow"}},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	start := time.Now()
	a.executeRun(ctx, claim)
	stepEnded := time.Now()

	_, lines := h.snapshot()
	h.mu.Lock()
	var markerTime time.Time
	for i, line := range h.logLines {
		if strings.Contains(line, "mid-step-marker") {
			markerTime = h.logLineTimes[i]
			break
		}
	}
	h.mu.Unlock()

	require.False(t, markerTime.IsZero(), "expected the mid-step-marker stderr line to be shipped, got lines: %v", lines)
	gapBeforeStepEnd := stepEnded.Sub(markerTime)
	assert.Greater(t, gapBeforeStepEnd, time.Second,
		"expected the stderr marker to reach the server at least 1s before the step ended (proving mid-step auto-flush), gap was %s", gapBeforeStepEnd)
	assert.Less(t, markerTime.Sub(start), 1*time.Second,
		"expected the auto-flush to ship the marker shortly after it was written, not only at step end")
}
