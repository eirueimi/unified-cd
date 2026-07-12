package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/paritycases"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

// shellFakeRT is a crt.ContainerRuntime for the isolated-dispatch parity case.
// It backs the host driver's claim pod: Create records each CreateSpec and
// returns sequential handles ("c0", "c1", ...), Exec runs the script through
// the SAME local shell the native host path uses (RunStep, rooted at the
// driver's workDir) so real echo output flows into the captured logs while the
// (handle, script) pair is recorded for dispatch assertions, and Remove
// records teardown. It deliberately does NOT isolate anything — it exists only
// to let the real claimPodManager/hostBackend dispatch logic run against a
// recordable, script-executing runtime, mirroring podFakeRT (claim_pod_test.go)
// but executing scripts (podFakeRT discards them).
type shellFakeRT struct {
	workDir string

	mu      sync.Mutex
	created []crt.CreateSpec
	execs   []struct{ handle, script string }
	removed []string
}

func newShellFakeRT(workDir string) *shellFakeRT { return &shellFakeRT{workDir: workDir} }

func (f *shellFakeRT) Name() string                       { return "shell-fake" }
func (f *shellFakeRT) Available() bool                    { return true }
func (f *shellFakeRT) Pull(context.Context, string) error { return nil }
func (f *shellFakeRT) Run(context.Context, crt.RunSpec, io.Writer, io.Writer) (int, error) {
	return 0, nil
}

func (f *shellFakeRT) Create(_ context.Context, s crt.CreateSpec) (crt.ContainerHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "c" + strconv.Itoa(len(f.created))
	f.created = append(f.created, s)
	return crt.ContainerHandle{ID: id}, nil
}

func (f *shellFakeRT) Exec(ctx context.Context, h crt.ContainerHandle, s crt.ExecSpec, stdout, stderr io.Writer) (int, error) {
	f.mu.Lock()
	f.execs = append(f.execs, struct{ handle, script string }{h.ID, s.Script})
	f.mu.Unlock()
	// Run the script through the same local shell the native host path uses so
	// echo output flows into the captured logs. RunStep tolerates nil writers
	// (post hooks pass nil), matching the native path exactly.
	return RunStep(ctx, s.Script, stdout, stderr, s.Env, f.workDir)
}

func (f *shellFakeRT) CopyIn(context.Context, crt.ContainerHandle, string, string) error  { return nil }
func (f *shellFakeRT) CopyOut(context.Context, crt.ContainerHandle, string, string) error { return nil }

func (f *shellFakeRT) Remove(_ context.Context, h crt.ContainerHandle) error {
	f.mu.Lock()
	f.removed = append(f.removed, h.ID)
	f.mu.Unlock()
	return nil
}

// handleByCreateIndex returns the handle id assigned to the i-th Create call
// (Create returns "c<index>", so this is just "c<i>", but going through the
// recorded slice keeps the mapping honest if the id scheme ever changes).
func (f *shellFakeRT) handleByCreateIndex(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.created) {
		return ""
	}
	return "c" + strconv.Itoa(i)
}

// execHandleForScript returns the handle the given script was exec'd into.
func (f *shellFakeRT) execHandleForScript(script string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.execs {
		if e.script == script {
			return e.handle, true
		}
	}
	return "", false
}

func (f *shellFakeRT) execScripts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.execs))
	for _, e := range f.execs {
		out = append(out, e.handle+":"+e.script)
	}
	return out
}

// parityHostHarness is a fake controller server mirroring the patterns used
// throughout agent_callrun_test.go / agent_stdout_stream_test.go /
// agent_if_test.go / agent_finally_test.go, generalized to serve ANY step
// index (the 10 parity cases use a handful of different stage layouts) via
// Go 1.22 {wildcard} mux patterns instead of one handler per fixed index.
type parityHostHarness struct {
	mu sync.Mutex

	// stepNameByIndex resolves a StepReportRequest.StepIndex back to the
	// step's display name, populated from every ReportStep body seen (a
	// step's very first report already carries its StepName).
	stepNameByIndex map[int]string

	// terminalStatus is keyed by "name" or "name@variant" (paritycases
	// VariantKey), holding only the LAST terminal (non-Running,
	// non-Skipped-is-terminal-too) status observed — Skipped is itself
	// terminal (no further reports follow for that step).
	terminalStatus map[string]string

	finishStatus string

	// logLines captures every shipped log line (both the single AppendLog
	// endpoint and the bulk endpoint), resolved to (stepName, stream, text).
	logLines []paritycases.LogLine

	// outputs captures SetStepOutputs bodies, keyed by step display name
	// (variant-qualified via DisplayName when MatrixKey is set — the query
	// param carries the raw variant key, but the step name path param plus
	// the fake's stepNameByIndex map already gives us the display name from
	// the ReportStep stream, so we key by the plain step name here since none
	// of the 10 cases needs per-variant outputs).
	outputs map[string]map[string]string

	// childRunID captures ChildRunID from a terminal StepReport, by step name.
	childRunID map[string]string

	secretsToServe map[string]string
	fetchedNames   []string
}

func newParityHostHarness() *parityHostHarness {
	return &parityHostHarness{
		stepNameByIndex: map[int]string{},
		terminalStatus:  map[string]string{},
		outputs:         map[string]map[string]string{},
		childRunID:      map[string]string{},
	}
}

// isTerminal reports whether status is a final per-step status (i.e. not an
// intermediate "Running" report).
func isTerminal(status string) bool {
	switch status {
	case "Succeeded", "Failed", "Skipped", "Cancelled":
		return true
	default:
		return false
	}
}

func (h *parityHostHarness) recordStepReport(req api.StepReportRequest) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if req.StepName != "" {
		h.stepNameByIndex[req.StepIndex] = req.StepName
	}
	if isTerminal(req.Status) {
		// req.StepName is the DisplayName() ("build (a)" for a matrix
		// variant), not the plain step name; recover the plain name by
		// trimming the " (...)" suffix DisplayName appends when Variant != ""
		// so the VariantKey matches paritycases' "name@variant" convention.
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

func (h *parityHostHarness) recordLogLine(stepIndex int, stream, line string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	name := h.stepNameByIndex[stepIndex]
	h.logLines = append(h.logLines, paritycases.LogLine{Step: name, Stream: stream, Substring: line})
}

func (h *parityHostHarness) recordOutputs(stepIndex int, outputs map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	name := h.stepNameByIndex[stepIndex]
	if name == "" {
		name = fmt.Sprintf("step-%d", stepIndex)
	}
	h.outputs[name] = outputs
}

// newParityHostServer stands up an httptest.Server implementing every
// endpoint executeRun can call, generalized over step index/run id via
// {wildcard} path patterns (Go 1.22+ ServeMux).
func newParityHostServer(t *testing.T, agentID string, h *parityHostHarness) *httptest.Server {
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
	// Single-line log endpoint (stdout is streamed via NewLogPusher too, but
	// some paths — e.g. AppendLog direct calls — use the non-bulk endpoint).
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", func(w http.ResponseWriter, r *http.Request) {
		var req api.LogAppendRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		h.recordLogLine(req.StepIndex, req.Stream, req.Line)
		w.WriteHeader(http.StatusNoContent)
	})
	// Bulk log endpoint: /api/v1/agents/{agentID}/runs/{runId}/steps/{idx}/logs/bulk
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
	// GetRun: used by the cancel-poller goroutine (never cancels in these
	// cases) — always report Running so the poller is a no-op.
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
	// CreateRun: for the call-succeeds-with-link case, always returns the
	// fixed child id.
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

// observation converts the harness's recordings into a paritycases.Observation.
func (h *parityHostHarness) observation() paritycases.Observation {
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

// TestParity_HostAgent drives every paritycases.Case through the real host
// agent's executeRun (real bash execution, no exec faking) and asserts the
// shared Expectation via paritycases.Assert.
func TestParity_HostAgent(t *testing.T) {
	for _, tc := range paritycases.Cases() {
		t.Run(tc.Name, func(t *testing.T) {
			runParityHostCase(t, tc)
		})
	}
}

func runParityHostCase(t *testing.T, tc paritycases.Case) {
	t.Helper()

	agentID := "parity-host-" + sanitizeName(tc.Name)
	h := newParityHostHarness()
	h.secretsToServe = tc.Secrets

	srv := newParityHostServer(t, agentID, h)

	a := &Agent{
		ID:     agentID,
		Client: NewClient(srv.URL, "tok"),
		// CacheStore: needed so cache: steps (e.g. cache-empty-key-skips)
		// actually exercise executeCacheStep's cache branch instead of
		// short-circuiting; a real filesystem-backed store rooted in a temp
		// dir is sufficient (mirrors agent_cache_test.go's newCacheTestAgent).
		CacheStore: objectstore.NewLocalObjectStore(t.TempDir()),
	}

	claim := tc.Claim()
	// Native vs isolated is now carried by each Case's claim (Native: true for
	// the host-process cases, unset for the isolated-dispatch case) rather than
	// forced here — see paritycases/scenarios.go. A native claim runs real bash
	// on the host workspace via executeRun's host-process path; the one
	// isolated case (Native == false) is driven below through a claim pod
	// backed by shellFakeRT, whose Exec still runs the script through the same
	// local shell so echo output flows into the captured logs.

	// post-hooks-lifo: the host agent's post: hook drain now streams the
	// script's stdout/stderr into the owning step's shipped log (see
	// paritycases.postHooksLIFO's doc comment), but this case still observes
	// LIFO order out-of-band: each post script appends a line to a real file
	// via $POSTHOOK_MARKER_FILE (inherited from the test process env, since
	// RunStep's cmd.Env = append(os.Environ(), extraEnv...)) — one file's
	// append order is a simpler ordering signal than diffing two log streams.
	// Dedicated coverage of post output reaching the shipped logs lives in
	// post_hook_logs_test.go.
	var markerFile string
	if tc.Name == "post-hooks-lifo" {
		markerFile = filepath.Join(t.TempDir(), "posthook-order.txt")
		t.Setenv("POSTHOOK_MARKER_FILE", markerFile)
	}

	workDir := t.TempDir()

	// fakeRT is non-nil only for the isolated claim; it records the claim pod's
	// exec dispatch so the driver can assert which container each step landed in.
	var fakeRT *shellFakeRT

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if claim.Native {
		// Native host-agent parity: real bash execution on the host workspace,
		// no claim pod. executeRun keeps the host-process path.
		a.executeRun(ctx, claim, workDir)
	} else {
		// Isolated claim: executeRun would demand a real container runtime, so
		// we reproduce its isolated-claim wiring here with a shellFakeRT whose
		// Exec runs each step's script through the SAME local shell (RunStep)
		// the native path uses — so echo output still flows into captured logs,
		// while Create/Exec/Remove are recorded for the dispatch assertions.
		fakeRT = newShellFakeRT(workDir)
		pod := newClaimPodManager(fakeRT, workDir, hostNamedMountPath(claim.PodTemplate), "pause:img", "runner:img", "")
		if err := pod.Start(ctx, claim.PodTemplate); err != nil {
			t.Fatalf("isolated claim: claim pod start failed: %v", err)
		}
		backend := newHostBackend(a, claim.RunID, workDir, pod)
		RunClaim(ctx, a.Client, a.ID, claim, backend)
	}
	elapsed := time.Since(start)

	if tc.Name == "step-timeout-fails" {
		if elapsed >= 8*time.Second {
			t.Errorf("step-timeout-fails: expected executeRun to return well before the step's own sleep would (8s), took %s", elapsed)
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
		assertPostHookLIFOFromMarkerFile(t, markerFile)
	}

	if !claim.Native {
		assertIsolatedDispatch(t, fakeRT)
	}
}

// assertIsolatedDispatch verifies the claim pod dispatched each step into the
// right container: the default "main" step (no container:) into the primary
// "job" container, and the "side" step (container: tools) into "tools". It
// asserts against the recorded Create/Exec handles rather than the container
// name directly, mirroring how the pod resolves a name to a handle
// (claimPodManager.Exec): the "job" container is the LAST created (injected
// after every podTemplate container — see claimContainerDefs), and "tools" is
// created in podTemplate order. This is the host twin of the k8s driver's
// container-name assertion.
func assertIsolatedDispatch(t *testing.T, rt *shellFakeRT) {
	t.Helper()
	// Create order for this case's podTemplate: [pause, tools, job].
	// createByName maps each recorded container back to its handle id so we can
	// match execs to the container the pod would have resolved.
	jobHandle := rt.handleByCreateIndex(2) // pause(0), tools(1), job(2)
	toolsHandle := rt.handleByCreateIndex(1)

	mainHandle, ok := rt.execHandleForScript("echo from-primary")
	if !ok {
		t.Fatalf("isolated dispatch: no exec recorded for the default \"main\" step (execs: %v)", rt.execScripts())
	}
	if mainHandle != jobHandle {
		t.Errorf("isolated dispatch: default \"main\" step exec'd container %q, want the primary \"job\" container %q", mainHandle, jobHandle)
	}

	sideHandle, ok := rt.execHandleForScript("echo from-tools")
	if !ok {
		t.Fatalf("isolated dispatch: no exec recorded for the \"side\" step (execs: %v)", rt.execScripts())
	}
	if sideHandle != toolsHandle {
		t.Errorf("isolated dispatch: \"side\" (container: tools) step exec'd container %q, want the \"tools\" container %q", sideHandle, toolsHandle)
	}
}

// assertPostHookLIFOFromMarkerFile reads the marker file each post: hook
// script appended a line to and asserts post-2 was written before post-1
// (LIFO: step2's post hook, appended to hookStack after step1's, drains
// first — see internal/agent/agent.go's hookStack `for i := len-1; i >= 0`
// drain loop).
func assertPostHookLIFOFromMarkerFile(t *testing.T, markerFile string) {
	t.Helper()
	data, err := os.ReadFile(markerFile)
	if err != nil {
		t.Fatalf("post-hooks-lifo: failed to read marker file %s: %v", markerFile, err)
	}
	lines := strings.Fields(strings.TrimSpace(string(data)))
	// Each line is like "post-2" / "post-1" per the post script's `echo post-N`.
	var order []string
	for _, l := range lines {
		if l == "post-1" || l == "post-2" {
			order = append(order, l)
		}
	}
	want := []string{"post-2", "post-1"}
	if len(order) != len(want) {
		t.Fatalf("post-hooks-lifo: marker file has %v, want exactly %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("post-hooks-lifo: marker order = %v, want %v (LIFO: step2's post before step1's)", order, want)
			break
		}
	}
}

// sanitizeName makes a case name safe to embed in an agent ID / URL path
// segment (the case names are already hyphenated lowercase, so this is a
// light touch, kept for safety against future case names with spaces).
func sanitizeName(name string) string {
	return strings.ReplaceAll(name, " ", "-")
}
