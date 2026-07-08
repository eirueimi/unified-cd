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
	"path/filepath"
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
// behavior directly: prefer well-known Git-for-Windows install locations,
// then fall back to bash on PATH — EXCLUDING the Windows 10+ WSL launcher
// (%SystemRoot%\System32\bash.exe). That launcher also matches "bash" on
// PATH but silently runs the script inside the WSL Linux environment, where
// the Go process's cmd.Env additions (e.g. FOO=bar, UNIFIED_AGENT_OS=linux)
// do not cross into WSL's environment — reproducing exactly the observed
// "got= os=" failure (env vars read as empty) instead of a hard error.
// Without this exclusion, exec.LookPath("bash") can resolve to the WSL
// launcher ahead of real Git Bash depending on PATH ordering, which is what
// caused this test to fail.
func parityShell(t *testing.T) string {
	t.Helper()
	candidates := []string{
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files (x86)\Git\bin\bash.exe`,
		`C:\Git\bin\bash.exe`,
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, `AppData\Local\Programs\Git\bin\bash.exe`))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	if path, err := exec.LookPath("bash"); err == nil && !isWSLLauncherK8s(path) {
		return path
	}
	t.Skipf("parity k8s driver needs bash (non-WSL) on PATH to run real step scripts")
	return ""
}

// isWSLLauncherK8s reports whether path is the WSL launcher
// (%SystemRoot%\System32\bash.exe), mirroring internal/agent/runner.go's
// unexported isWSLLauncher so this test driver gets the same protection the
// production host agent has via findShell/locateGitBash.
func isWSLLauncherK8s(path string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(path, "/", `\`))
	return strings.HasSuffix(normalized, `\system32\bash.exe`)
}

// parityRunScript runs a step's script for real via the discovered shell,
// mirroring the UNIFIED_AGENT_OS=linux injection that the shared
// orchestration loop (agentlib.RunClaim, internal/agent/orchestrator.go)
// applies via agentOSForStep/b.DefaultAgentOS() before RunDefault (built via
// k8sBackend) invokes Executor.ExecStep. Honors ctx cancellation via
// exec.CommandContext, and additionally kills the
// whole process tree on cancellation (see parityKillTree) since
// exec.CommandContext alone only signals the direct child (the shell),
// leaving a backgrounded `sleep` grandchild running — the same class of
// problem the host agent's killTree/runTreeKilled solves
// (internal/agent/exec_tree.go, exec_unix.go, exec_windows.go). stdout/stderr
// are the writers orchestrate hands to RunDefault (its own tee/shipping
// wiring via ExecBackend.StepLogWriters), so this function just needs to
// stream the real script's output into them.
func parityRunScript(t *testing.T, shell string, step api.ClaimStep, expandedRun string, stdout, stderr io.Writer) func(ctx context.Context) (int, error) {
	t.Helper()
	return func(ctx context.Context) (int, error) {
		env := append([]string{}, os.Environ()...)
		env = append(env, "UNIFIED_AGENT_OS=linux")
		for k, v := range step.Env {
			env = append(env, k+"="+v)
		}

		cmd := exec.Command(shell, "-lc", expandedRun)
		cmd.Env = env
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		paritySetupProcAttrs(cmd)

		if startErr := cmd.Start(); startErr != nil {
			return -1, startErr
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

		exitCode := 0
		if runErr != nil {
			if ee, ok := runErr.(*exec.ExitError); ok {
				exitCode = ee.ExitCode()
				runErr = nil
			} else {
				exitCode = -1
			}
		}
		return exitCode, runErr
	}
}

// parityK8sBackend is the real-execution ExecBackend used by
// TestParity_K8sAgent: RunDefault (the only exec path any of the 10 parity
// cases exercises) runs the step's script for real via parityRunScript, and
// StepLogWriters ships real output through parityLineShipper (masked),
// mirroring production's log-shipping shape closely enough for the parity
// suite's log-content assertions (env-reaches-script, finally marker, secret
// masking). Cache/artifact/scope methods are minimal recording stubs — none
// of the 10 cases exercises them. postOrder/postMu record RunPostHook script
// invocation order directly (unlike the host driver, this callback IS what
// runs the script, so LIFO order can be observed with no marker-file
// workaround).
type parityK8sBackend struct {
	t       *testing.T
	client  *agentlib.Client
	agentID string
	runID   string
	masker  *secrets.Masker
	shell   string

	postMu    sync.Mutex
	postOrder []string

	// dispatchMu/dispatch records the (container, script) each exec targeted so
	// the isolated-dispatch case can assert which container each step landed in
	// — the k8s twin of the host driver's exec-handle assertion.
	dispatchMu sync.Mutex
	dispatch   []struct{ container, script string }
}

func (b *parityK8sBackend) recordDispatch(container, script string) {
	b.dispatchMu.Lock()
	b.dispatch = append(b.dispatch, struct{ container, script string }{container, script})
	b.dispatchMu.Unlock()
}

func (b *parityK8sBackend) RunDefault(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error) {
	// The real k8sBackend.RunDefault passes execContainer(step) (== step.Container,
	// "" for a default step) to Executor.ExecStep, and execArgv falls back "" ->
	// "job" (internal/k8sagent/executor.go). Mirror that fallback here so the
	// fake faithfully records the container the production exec would have used.
	container := step.Container
	if container == "" {
		container = "job"
	}
	b.recordDispatch(container, script)
	return parityRunScript(b.t, b.shell, step, script, stdout, stderr)(ctx)
}

func (b *parityK8sBackend) RunNamedContainer(ctx context.Context, step api.ClaimStep, container, script string, env []string, stdout, stderr io.Writer) (int, error) {
	b.recordDispatch(container, script)
	return parityRunScript(b.t, b.shell, step, script, stdout, stderr)(ctx)
}

func (b *parityK8sBackend) EnsureScope(ctx context.Context, step api.ClaimStep, env []string) (agentlib.ScopeHandle, error) {
	return agentlib.NewScopeHandle("scope-pod-" + step.ScopeID), nil
}

func (b *parityK8sBackend) RunInScope(ctx context.Context, h agentlib.ScopeHandle, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return parityRunScript(b.t, b.shell, api.ClaimStep{}, script, stdout, stderr)(ctx)
}

func (b *parityK8sBackend) CloseScopes(ctx context.Context) {}

func (b *parityK8sBackend) CacheRestore(ctx context.Context, scope agentlib.ScopeHandle, key string, restoreKeys []string, path string) (bool, error) {
	return false, nil
}

func (b *parityK8sBackend) CacheSave(ctx context.Context, scope agentlib.ScopeHandle, key, path string, ttlDays int) error {
	return nil
}

func (b *parityK8sBackend) UploadArtifact(ctx context.Context, scope agentlib.ScopeHandle, runID, name, path string) error {
	return nil
}

func (b *parityK8sBackend) DownloadArtifact(ctx context.Context, scope agentlib.ScopeHandle, runID, name, destDir string) error {
	return nil
}

// RunPostHook records the post hook's script in invocation order (so
// post-hooks-lifo can assert LIFO order) and, since none of the 10 parity
// cases' post hooks depend on their side effects being observed any other
// way than the marker-file echoes already captured via log shipping in the
// step body, does not itself execute the script.
func (b *parityK8sBackend) RunPostHook(ctx context.Context, scope agentlib.ScopeHandle, container, script string, env []string) error {
	b.postMu.Lock()
	b.postOrder = append(b.postOrder, script)
	b.postMu.Unlock()
	return nil
}

// ResolveArtifactPath is a minimal passthrough: none of the 10 parity cases
// exercises cache/artifact paths, so exact resolution semantics don't matter
// here (unlike fakeK8sBackend/k8sBackend, which have argv-path assertions).
func (b *parityK8sBackend) ResolveArtifactPath(scope agentlib.ScopeHandle, p string) string {
	return p
}

// ResolveCachePath is a minimal passthrough (see ResolveArtifactPath's doc
// comment: none of the 10 parity cases exercises cache/artifact paths).
func (b *parityK8sBackend) ResolveCachePath(scope agentlib.ScopeHandle, p string) string {
	return p
}

// DefaultAgentOS mirrors k8sBackend: every k8s exec path runs inside a Linux pod.
func (b *parityK8sBackend) DefaultAgentOS() string {
	return "linux"
}

func (b *parityK8sBackend) SetMasker(m *secrets.Masker) { b.masker = m }

func (b *parityK8sBackend) StepLogWriters(ctx context.Context, stepIndex int) (stdout, stderr io.Writer, finish func(ctx context.Context)) {
	stdoutShip := &parityLineShipper{client: b.client, agentID: b.agentID, runID: b.runID, stepIdx: stepIndex, stream: "stdout", masker: b.masker}
	stderrShip := &parityLineShipper{client: b.client, agentID: b.agentID, runID: b.runID, stepIdx: stepIndex, stream: "stderr", masker: b.masker}
	finish = func(context.Context) {
		stdoutShip.flushRemainder()
		stderrShip.flushRemainder()
	}
	return stdoutShip, stderrShip, finish
}

func (b *parityK8sBackend) ConcurrencyMode() agentlib.ConcurrencyMode { return agentlib.Sequential }

var _ agentlib.ExecBackend = (*parityK8sBackend)(nil)

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

	// The masker is no longer pre-built here: agentlib.RunClaim fetches
	// secrets itself (via the harness's /secrets/fetch endpoint, serving
	// tc.Secrets — see h.secretsToServe above) and installs the masker on the
	// backend via SetMasker, mirroring the host agent exactly.
	shell := parityShell(t)
	backend := &parityK8sBackend{t: t, client: client, agentID: agentID, runID: runID, shell: shell}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	agentlib.RunClaim(ctx, client, a.cfg.AgentID, claim, backend)
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
		backend.postMu.Lock()
		order := append([]string(nil), backend.postOrder...)
		backend.postMu.Unlock()
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

	if !claim.Native {
		assertK8sIsolatedDispatch(t, backend)
	}
}

// assertK8sIsolatedDispatch verifies each step of the isolated-dispatch case
// was exec'd into the right container NAME: the default "main" step into the
// primary "job" container (RunDefault normalizes "" -> "job", mirroring the
// real Executor.execArgv fallback), and the "side" step into "tools". This is
// the k8s twin of the host driver's exec-handle assertion (assertIsolatedDispatch).
func assertK8sIsolatedDispatch(t *testing.T, b *parityK8sBackend) {
	t.Helper()
	b.dispatchMu.Lock()
	dispatch := append([]struct{ container, script string }(nil), b.dispatch...)
	b.dispatchMu.Unlock()

	find := func(script string) (string, bool) {
		for _, d := range dispatch {
			if d.script == script {
				return d.container, true
			}
		}
		return "", false
	}

	mainC, ok := find("echo from-primary")
	if !ok {
		t.Fatalf("isolated dispatch: no exec recorded for the default \"main\" step (dispatch: %v)", dispatch)
	}
	if mainC != "job" {
		t.Errorf("isolated dispatch: default \"main\" step exec'd container %q, want the primary \"job\" container", mainC)
	}

	sideC, ok := find("echo from-tools")
	if !ok {
		t.Fatalf("isolated dispatch: no exec recorded for the \"side\" step (dispatch: %v)", dispatch)
	}
	if sideC != "tools" {
		t.Errorf("isolated dispatch: \"side\" (container: tools) step exec'd container %q, want the \"tools\" container", sideC)
	}
}

// sanitizeNameK8s makes a case name safe to embed in an agent ID / URL path
// segment (mirrors internal/agent/parity_host_test.go's sanitizeName).
func sanitizeNameK8s(name string) string {
	return strings.ReplaceAll(name, " ", "-")
}
