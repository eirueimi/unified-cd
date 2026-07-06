package k8sagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/paritycases"
	"github.com/eirueimi/unified-cd/internal/secrets"
)

// parityK8sHarness mirrors parityHostHarness (internal/agent/parity_host_test.go)
// against the same fake-controller pattern used throughout
// orchestrate_test.go/orchestrate_post_test.go/orchestrate_timeout_test.go/
// secrets_masking_k8s_test.go, generalized over step index via {wildcard} mux
// patterns.
type parityK8sHarness struct {
	mu sync.Mutex

	stepNameByIndex map[int]string
	terminalStatus  map[string]string
	finishStatus    string
	logLines        []paritycases.LogLine
	outputs         map[string]map[string]string
	childRunID      map[string]string

	secretsToServe map[string]string
	fetchedNames   []string

	// postOrder records each post-hook script's text in invocation order (the
	// k8s agent's postExec callback runs the script directly — unlike the
	// host, which discards post-hook stdout/stderr, we can observe the k8s
	// post-hook invocation order directly through the fake postExec below,
	// no marker-file workaround needed).
	postOrder []string
}

func newParityK8sHarness() *parityK8sHarness {
	return &parityK8sHarness{
		stepNameByIndex: map[int]string{},
		terminalStatus:  map[string]string{},
		outputs:         map[string]map[string]string{},
		childRunID:      map[string]string{},
	}
}

func (h *parityK8sHarness) recordStepReport(req api.StepReportRequest) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if req.StepName != "" {
		h.stepNameByIndex[req.StepIndex] = req.StepName
	}
	if isTerminalK8s(req.Status) {
		baseName := req.StepName
		if req.Variant != "" {
			if i := strings.Index(baseName, " ("); i >= 0 {
				baseName = baseName[:i]
			}
		}
		key := paritycases.VariantKey(baseName, req.Variant)
		h.terminalStatus[key] = req.Status
		if req.ChildRunID != "" {
			h.childRunID[baseName] = req.ChildRunID
		}
	}
}

func isTerminalK8s(status string) bool {
	switch status {
	case "Succeeded", "Failed", "Skipped", "Cancelled":
		return true
	default:
		return false
	}
}

func (h *parityK8sHarness) recordLogLine(stepIndex int, stream, line string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	name := h.stepNameByIndex[stepIndex]
	h.logLines = append(h.logLines, paritycases.LogLine{Step: name, Stream: stream, Substring: line})
}

func (h *parityK8sHarness) recordOutputs(stepIndex int, outputs map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	name := h.stepNameByIndex[stepIndex]
	if name == "" {
		name = fmt.Sprintf("step-%d", stepIndex)
	}
	h.outputs[name] = outputs
}

func newParityK8sServer(t *testing.T, agentID string, h *parityK8sHarness) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.recordStepReport(req)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, r *http.Request) {
		var req api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.recordLogLine(req.StepIndex, req.Stream, req.Line)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/steps/{idx}/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		idx, _ := strconv.Atoi(r.PathValue("idx"))
		var reqs []api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&reqs)
		for _, l := range reqs {
			if l.Line != "" {
				h.recordLogLine(idx, l.Stream, l.Line)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/steps/{idx}/outputs", func(w http.ResponseWriter, r *http.Request) {
		idx, _ := strconv.Atoi(r.PathValue("idx"))
		var req api.SetOutputsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.recordOutputs(idx, req.Outputs)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/outputs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.mu.Lock()
		h.finishStatus = body.Status
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/runs/{runId}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("runId")
		st := api.RunRunning
		if id == paritycases.ChildRunIDFixture {
			st = api.RunSucceeded
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Run{ID: id, Status: st})
	})
	mux.HandleFunc("GET /api/v1/runs/{runId}/outputs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.RunOutputs{Outputs: map[string]string{}})
	})
	mux.HandleFunc("POST /api/v1/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Run{ID: paritycases.ChildRunIDFixture, Status: api.RunSucceeded})
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

func (h *parityK8sHarness) observation() paritycases.Observation {
	h.mu.Lock()
	defer h.mu.Unlock()
	statuses := make(map[string]string, len(h.terminalStatus))
	for k, v := range h.terminalStatus {
		statuses[k] = v
	}
	logs := append([]paritycases.LogLine(nil), h.logLines...)
	outputs := make(map[string]map[string]string, len(h.outputs))
	for k, v := range h.outputs {
		outputs[k] = v
	}
	childRunID := make(map[string]string, len(h.childRunID))
	for k, v := range h.childRunID {
		childRunID[k] = v
	}
	return paritycases.Observation{
		StepStatus:  statuses,
		RunFinished: h.finishStatus,
		Logs:        logs,
		Outputs:     outputs,
		ChildRunID:  childRunID,
	}
}

// paritySetupProcAttrs and parityKillTree give the driver's real-bash
// exec.Cmd the same "whole process tree dies on cancel" guarantee the host
// agent provides via internal/agent/exec_unix.go / exec_windows.go (used by
// runTreeKilled) — needed because `bash -lc "sleep 10"` backgrounds sleep as
// a grandchild, and killing only the direct bash.exe/bash child leaves it
// running. This driver reimplements a minimal version directly (rather than
// importing the unexported helpers from internal/agent) using
// platform-neutral primitives: on Windows, `taskkill /T /F` kills the whole
// tree by PID; on Unix, a negative PID signals the whole process group
// (requires Setpgid, set here via syscall — safe to reference unconditionally
// since Go compiles the correct GOOS-specific syscall.SysProcAttr fields).
func paritySetupProcAttrs(cmd *exec.Cmd) {
	setPlatformProcAttrs(cmd)
}

func parityKillTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	killPlatformProcessTree(cmd)
}

// parityShell resolves the shell used to run a step's script for real. The
// host agent's shell-discovery helper (findShell in internal/agent/runner.go)
// is unexported, so it cannot be imported here; this mirrors its production
// fallback behavior (bash on PATH) directly via exec.LookPath, which is
// sufficient for this Windows dev box's Git Bash-on-PATH setup and for any
// Unix CI runner.
func parityShell(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("parity k8s driver needs bash on PATH to run real step scripts: %v", err)
	}
	return path
}

// parityStepExec builds a podStepExec that runs a step's script for real via
// the discovered shell, mirroring execStepEnv's UNIFIED_AGENT_OS=linux
// injection (internal/k8sagent/agent.go) since orchestrate's real stepExec
// (built in executeRun) applies exactly that env before invoking
// Executor.ExecStep. Honors ctx cancellation via exec.CommandContext, and
// additionally kills the whole process tree on cancellation (see
// parityKillTree) since exec.CommandContext alone only signals the direct
// child (the shell), leaving a backgrounded `sleep` grandchild running — the
// same class of problem the host agent's killTree/runTreeKilled solves
// (internal/agent/exec_tree.go, exec_unix.go, exec_windows.go).
//
// Log shipping: production's real stepExec closure (built in executeRun,
// internal/k8sagent/agent.go ~287-337) tees stdout through a logLineWriter
// (single-line AppendLog, masked) and stderr through a LogPusher (bulk,
// masked, auto-flushed). Since this driver calls orchestrate directly with
// its own podStepExec, it replicates that shipping (and masking, via the
// real internal/secrets.Masker) here — otherwise log-content assertions
// (env-reaches-script, finally marker, secret masking) would observe nothing.
func parityStepExec(t *testing.T, client *agentlib.Client, agentID, runID string, masker *secrets.Masker) podStepExec {
	t.Helper()
	shell := parityShell(t)
	return func(ctx context.Context, step api.ClaimStep, expandedRun string) (int, string, error) {
		env := append([]string{}, os.Environ()...)
		env = append(env, "UNIFIED_AGENT_OS=linux")
		for k, v := range step.Env {
			env = append(env, k+"="+v)
		}

		var stdoutBuf strings.Builder
		stdoutShip := &parityLineShipper{client: client, agentID: agentID, runID: runID, stepIdx: step.Index, stream: "stdout", masker: masker}
		stdoutWriter := io.MultiWriter(&stdoutBuf, stdoutShip)
		stderrShip := &parityLineShipper{client: client, agentID: agentID, runID: runID, stepIdx: step.Index, stream: "stderr", masker: masker}

		cmd := exec.Command(shell, "-lc", expandedRun)
		cmd.Env = env
		cmd.Stdout = stdoutWriter
		cmd.Stderr = stderrShip
		paritySetupProcAttrs(cmd)

		if startErr := cmd.Start(); startErr != nil {
			return -1, "", startErr
		}
		waitDone := make(chan error, 1)
		go func() { waitDone <- cmd.Wait() }()

		var runErr error
		select {
		case runErr = <-waitDone:
		case <-ctx.Done():
			parityKillTree(cmd)
			<-waitDone // reap
			runErr = ctx.Err()
		}
		stdoutShip.flushRemainder()
		stderrShip.flushRemainder()

		exitCode := 0
		if runErr != nil {
			if ee, ok := runErr.(*exec.ExitError); ok {
				exitCode = ee.ExitCode()
				runErr = nil
			} else {
				exitCode = -1
			}
		}
		return exitCode, stdoutBuf.String(), runErr
	}
}

// parityLineShipper is a minimal Writer that masks (if masker != nil) and
// ships each newline-delimited line to the fake controller via AppendLog.
// Mirrors k8sagent's unexported logLineWriter (internal/k8sagent/agent.go),
// which this test package cannot reuse directly since it is unexported.
type parityLineShipper struct {
	client  *agentlib.Client
	agentID string
	runID   string
	stepIdx int
	stream  string
	masker  *secrets.Masker
	buf     strings.Builder
}

func (s *parityLineShipper) Write(p []byte) (int, error) {
	s.buf.Write(p)
	full := s.buf.String()
	for {
		idx := strings.IndexByte(full, '\n')
		if idx < 0 {
			break
		}
		line := full[:idx]
		full = full[idx+1:]
		s.ship(line)
	}
	s.buf.Reset()
	s.buf.WriteString(full)
	return len(p), nil
}

// flushRemainder ships any partial (no trailing newline) buffered content as
// a final line, so a script's last unterminated line is not silently dropped.
func (s *parityLineShipper) flushRemainder() {
	if s.buf.Len() == 0 {
		return
	}
	line := s.buf.String()
	s.buf.Reset()
	s.ship(line)
}

func (s *parityLineShipper) ship(line string) {
	if s.masker != nil {
		line = s.masker.Mask(line)
	}
	_ = s.client.AppendLog(context.Background(), s.agentID, api.LogAppendRequest{
		RunID: s.runID, StepIndex: s.stepIdx, Stream: s.stream,
		Timestamp: time.Now().UTC(), Line: line,
	})
}

// TestParity_K8sAgent drives every paritycases.Case through the k8s agent's
// real orchestrate loop (a.orchestrate), with stepExec running real scripts
// (no exec faking) and minimal recording stubs for sidecarExec/postExec/
// ensureScopePod (none of the 10 cases exercises cache/artifact/scope
// behavior).
func TestParity_K8sAgent(t *testing.T) {
	for _, tc := range paritycases.Cases() {
		t.Run(tc.Name, func(t *testing.T) {
			runParityK8sCase(t, tc)
		})
	}
}

func runParityK8sCase(t *testing.T, tc paritycases.Case) {
	t.Helper()

	agentID := "parity-k8s-" + sanitizeNameK8s(tc.Name)
	runID := "run-" + sanitizeNameK8s(tc.Name)
	h := newParityK8sHarness()
	h.secretsToServe = tc.Secrets

	srv := newParityK8sServer(t, agentID, h)
	client := agentlib.NewClient(srv.URL, "tok")

	a := &K8sAgent{
		cfg:    Config{AgentID: agentID},
		client: client,
	}

	claim := tc.Claim()

	// Build the masker exactly as executeRun does (internal/k8sagent/agent.go):
	// NewMasker over the case's secret VALUES when any are declared, else a
	// no-op masker. secretsResolveAndMask is the only case that sets Secrets.
	var masker *secrets.Masker
	if len(tc.Secrets) > 0 {
		vals := make([]string, 0, len(tc.Secrets))
		for _, v := range tc.Secrets {
			vals = append(vals, v)
		}
		masker = secrets.NewMasker(vals)
	} else {
		masker = secrets.NoOpMasker
	}

	stepExec := parityStepExec(t, client, agentID, runID, masker)

	// sidecarExec: recording no-op stub. orchestrate uses sidecarExec only
	// for cache/artifact steps (dispatched via the unified-sidecar binary's
	// argv protocol) — none of the 10 parity cases exercises cache/artifact
	// behavior, so a no-op that always reports success is sufficient here.
	noopSidecarExec := func(_ context.Context, _, _ string, _ []string) (int, error) { return 0, nil }

	// postExec: records (script) invocation order directly, so
	// post-hooks-lifo can assert LIFO order without any marker-file
	// workaround (unlike the host driver — see parity_host_test.go — the k8s
	// postExec callback IS the thing that runs the script, so we can observe
	// it directly).
	var postMu sync.Mutex
	var postOrder []string
	postExec := func(_ context.Context, _, _, script string, _ []string) error {
		postMu.Lock()
		postOrder = append(postOrder, script)
		postMu.Unlock()
		return nil
	}

	// ensureScopePod: none of the 10 cases uses uses-scope steps.
	noopEnsureScopePod := func(_ context.Context, _ api.ClaimStep) (string, error) { return "", nil }

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a.orchestrate(ctx, claim, stepExec, noopSidecarExec, postExec, "/workspace", noopEnsureScopePod, tc.Secrets)
	elapsed := time.Since(start)

	if tc.Name == "step-timeout-fails" {
		if elapsed >= 8*time.Second {
			t.Errorf("step-timeout-fails: expected orchestrate to return well before the step's own sleep would (8s), took %s", elapsed)
		}
	}

	obs := h.observation()
	paritycases.Assert(t, tc.Expect, obs)

	if tc.Name == "call-succeeds-with-link" {
		if got := obs.ChildRunID["callChild"]; got != paritycases.ChildRunIDFixture {
			t.Errorf("call-succeeds-with-link: ChildRunID[%q] = %q, want %q", "callChild", got, paritycases.ChildRunIDFixture)
		}
	}

	if tc.Name == "post-hooks-lifo" {
		postMu.Lock()
		order := append([]string(nil), postOrder...)
		postMu.Unlock()
		wantOrder := []string{
			`echo post-2 >> "$POSTHOOK_MARKER_FILE"`,
			`echo post-1 >> "$POSTHOOK_MARKER_FILE"`,
		}
		if len(order) != len(wantOrder) {
			t.Fatalf("post-hooks-lifo: postExec invocation order = %v, want %v", order, wantOrder)
		}
		for i := range wantOrder {
			if order[i] != wantOrder[i] {
				t.Errorf("post-hooks-lifo: postExec invocation order = %v, want %v (LIFO: step2's post before step1's)", order, wantOrder)
				break
			}
		}
	}
}

// sanitizeNameK8s makes a case name safe to embed in an agent ID / URL path
// segment (mirrors internal/agent/parity_host_test.go's sanitizeName).
func sanitizeNameK8s(name string) string {
	return strings.ReplaceAll(name, " ", "-")
}
