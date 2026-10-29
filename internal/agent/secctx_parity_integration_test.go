//go:build integration

package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/paritycases"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/require"
)

// containerRuntimeEnv names the container runtime the real-runtime integration
// tests drive: docker, podman or nerdctl. Unset, crt.Detect("") picks the first
// one installed, which on a machine with several is not necessarily the one a
// CI matrix leg means to test.
const containerRuntimeEnv = "UCD_TEST_CONTAINER_RUNTIME"

// testWorkspaceBaseEnv, when set, is a directory the runtime can bind with real
// Linux semantics (setgid inheritance, chown) to hold the test workspaces. On a
// Windows or macOS machine the default (a t.TempDir on the host disk) goes
// through a file-sharing mount that does not honour them.
const testWorkspaceBaseEnv = "UCD_TEST_WSBASE"

// testRuntimePref is the runtime preference the integration tests pass to
// crt.Detect and Agent.RuntimePref.
func testRuntimePref() string { return os.Getenv(containerRuntimeEnv) }

// requireRuntimeEnv names the switch that turns "this environment lacks a
// runtime" from a skip into a failure. CI sets CI=true itself; a developer can
// set UCD_REQUIRE_RUNTIME=1 to get the same strictness locally. Without it a
// CI leg whose runtime silently failed to install would go green by skipping
// every test (go test reports a skip as ok).
const requireRuntimeEnv = "UCD_REQUIRE_RUNTIME"

// expectRootlessEnv, when "1", makes the host driver assert that the startup
// probe saw a rootless runtime. The rootless CI legs set it so a leg that
// quietly fell back to a rootful daemon cannot pass as rootless.
const expectRootlessEnv = "UCD_TEST_EXPECT_ROOTLESS"

// skipOrFailInCI skips the test on a developer machine and fails it when CI (or
// UCD_REQUIRE_RUNTIME=1) says the environment is supposed to provide what is
// missing.
func skipOrFailInCI(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("CI") != "" || os.Getenv(requireRuntimeEnv) == "1" {
		t.Fatalf("required environment missing (CI=%q %s=%q): "+format,
			append([]any{os.Getenv("CI"), requireRuntimeEnv, os.Getenv(requireRuntimeEnv)}, args...)...)
	}
	t.Skipf(format, args...)
}

// requireProbe runs the agent's startup probe, logs what it saw, and fails or
// skips when it did not pass (without it the agent would not advertise
// securitycontext, so the cases would never be routed here). When
// UCD_TEST_EXPECT_ROOTLESS=1 it also requires the probe to report a rootless
// runtime.
func requireProbe(t *testing.T, rt crt.ContainerRuntime, a *Agent) {
	t.Helper()
	probe := a.securityContextProbe()
	t.Logf("runtime=%s probe: ok=%v rootless=%v maxMappedID=%d reason=%q",
		rt.Name(), probe.OK, probe.Rootless, probe.MaxMappedID, probe.Reason)
	if !probe.OK {
		skipOrFailInCI(t, "the securityContext startup probe failed on this runtime/workspace: %s (set %s to a directory with real Linux semantics)",
			probe.Reason, testWorkspaceBaseEnv)
		return
	}
	if os.Getenv(expectRootlessEnv) == "1" && !probe.Rootless {
		t.Fatalf("%s=1 but the probe reports a rootful %s runtime", expectRootlessEnv, rt.Name())
	}
}

const secCtxAgentID = "secctx-parity-agent"

// TestSecCtxParity_HostAgent runs every paritycases.SecurityContextCases claim
// through the real host agent against a real container runtime and asserts the
// shared Expectation. The k8s twin (internal/k8sagent/secctx_parity_k8s_test.go)
// asserts the same Expectation against a real kubelet; together they are the
// parity golden (design §10).
func TestSecCtxParity_HostAgent(t *testing.T) {
	pref := testRuntimePref()
	rt, err := crt.Detect(pref)
	if err != nil {
		skipOrFailInCI(t, "no container runtime available (%s=%q): %v", containerRuntimeEnv, pref, err)
	}
	t.Logf("container runtime: %s", rt.Name())

	wsBase := os.Getenv(testWorkspaceBaseEnv)
	if wsBase == "" {
		wsBase = t.TempDir()
	}
	require.NoError(t, os.MkdirAll(wsBase, 0o755))
	toolsDir := installShimIntoOrSkip(t, wsBase)

	a := &Agent{
		ID:           secCtxAgentID,
		PauseImage:   paritycases.SecurityContextImage,
		RunnerImage:  paritycases.SecurityContextImage,
		ToolsDir:     toolsDir,
		RuntimePref:  pref,
		WorkspaceDir: wsBase,
	}

	requireProbe(t, rt, a)

	for _, tc := range paritycases.SecurityContextCases() {
		t.Run(tc.Name, func(t *testing.T) {
			runSecCtxHostCase(t, a, wsBase, tc)
		})
	}
}

// installShimIntoOrSkip is installShimOrSkip for a caller-chosen base.
func installShimIntoOrSkip(t *testing.T, base string) string {
	t.Helper()
	toolsDir, err := InstallShim(base)
	if err != nil {
		skipOrFailInCI(t, "ucd-sh shim not embedded (run `go generate ./internal/shim/embedded/` first): %v", err)
	}
	return toolsDir
}

func runSecCtxHostCase(t *testing.T, a *Agent, wsBase string, tc paritycases.Case) {
	t.Helper()
	caseBase, err := os.MkdirTemp(wsBase, "secctx-")
	require.NoError(t, err)
	rtFn := a.containerRuntime
	t.Cleanup(func() {
		// Files owned by other UIDs: only the root cleanup container can
		// remove them, exactly as the agent's own workspace sweep does.
		if err := wipeDir(context.Background(), caseBase, false, rtFn, a.ToolsDir); err != nil {
			t.Logf("cleanup %s: %v", caseBase, err)
		}
	})

	// The agent's volumes are shared by the Setup runs and the main run, so a
	// root-owned leftover written by one is seen by the next.
	var vcfgs []config.VolumeConfig
	for _, v := range tc.Claim().Volumes {
		vcfgs = append(vcfgs, config.VolumeConfig{Name: v.Name[0], Mode: "exclusive"})
	}
	if len(vcfgs) > 0 {
		vm, err := NewVolumeManager(filepath.Join(caseBase, "vols"), vcfgs)
		require.NoError(t, err)
		a.Volumes = vm
	} else {
		a.Volumes = nil
	}
	defer func() { a.Volumes = nil }()

	for i, c := range append(append([]paritycases.Case(nil), tc.Setup...), tc) {
		t.Logf("running claim %s", c.Name)
		runSecCtxHostClaim(t, a, caseBase, i, c)
	}
}

func runSecCtxHostClaim(t *testing.T, a *Agent, caseBase string, n int, c paritycases.Case) {
	t.Helper()
	h := newParityHostHarness()
	inner := newParityHostServer(t, secCtxAgentID, h)
	srv := newSecCtxServer(t, inner.URL, c.Artifacts)
	a.Client = NewClient(srv.URL, "tok")

	workDir := filepath.Join(caseBase, fmt.Sprintf("working%d", n))
	require.NoError(t, prepareWorkspace(context.Background(), workDir, "isolated", nil, false, a.containerRuntime, a.ToolsDir))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	a.executeRun(ctx, c.Claim(), workDir)

	paritycases.Assert(t, c.Expect, h.observation())
}

// newSecCtxServer fronts the parity fake controller with an artifact endpoint
// serving files as the run's single artifact (a zstd tar, as the real
// controller streams it), and proxies everything else to inner.
func newSecCtxServer(t *testing.T, innerURL string, files map[string]string) *httptest.Server {
	t.Helper()
	target, err := url.Parse(innerURL)
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.Handle("/", httputil.NewSingleHostReverseProxy(target))
	if len(files) > 0 {
		dir := t.TempDir()
		for name, content := range files {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
		}
		mux.HandleFunc("GET /api/v1/runs/{runId}/artifacts/{name}", func(w http.ResponseWriter, r *http.Request) {
			if r.PathValue("name") != paritycases.ArtifactName {
				http.NotFound(w, r)
				return
			}
			rc := artifact.StreamTarZstd(dir)
			defer rc.Close()
			w.Header().Set("Content-Type", "application/zstd")
			_, _ = io.Copy(w, rc)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestSecCtx_Integration_UploadFallbackReadsOwnerOnlyFile is design §11 V5. A
// step running as runAsUser 1000 leaves a 0600 file the agent's own user cannot
// read; the upload must still carry its content, through the root copy taken
// from the claim pod's pause container (docker cp / podman cp of a path under a
// bind mount of a running container).
func TestSecCtx_Integration_UploadFallbackReadsOwnerOnlyFile(t *testing.T) {
	pref := testRuntimePref()
	rt, err := crt.Detect(pref)
	if err != nil {
		skipOrFailInCI(t, "no container runtime available (%s=%q): %v", containerRuntimeEnv, pref, err)
	}
	wsBase := os.Getenv(testWorkspaceBaseEnv)
	if wsBase == "" {
		wsBase = t.TempDir()
	}
	require.NoError(t, os.MkdirAll(wsBase, 0o755))
	a := &Agent{
		ID: secCtxAgentID, PauseImage: paritycases.SecurityContextImage, RunnerImage: paritycases.SecurityContextImage,
		ToolsDir: installShimIntoOrSkip(t, wsBase), RuntimePref: pref, WorkspaceDir: wsBase,
	}
	requireProbe(t, rt, a)

	caseBase, err := os.MkdirTemp(wsBase, "secctx-upload-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = wipeDir(context.Background(), caseBase, false, a.containerRuntime, a.ToolsDir) })

	var uploaded []byte
	h := newParityHostHarness()
	inner := newParityHostServer(t, secCtxAgentID, h)
	target, err := url.Parse(inner.URL)
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.Handle("/", httputil.NewSingleHostReverseProxy(target))
	mux.HandleFunc("PUT /api/v1/runs/{runId}/artifacts/{name}", func(w http.ResponseWriter, r *http.Request) {
		dir, derr := os.MkdirTemp(caseBase, "got-")
		if derr == nil {
			derr = artifact.ExtractTarZstd(r.Body, dir)
		}
		if derr == nil {
			uploaded, derr = os.ReadFile(filepath.Join(dir, "out.txt"))
		}
		if derr != nil {
			http.Error(w, derr.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	a.Client = NewClient(srv.URL, "tok")

	workDir := filepath.Join(caseBase, "working")
	require.NoError(t, prepareWorkspace(context.Background(), workDir, "isolated", nil, false, a.containerRuntime, a.ToolsDir))

	claim := paritycases.SecurityContextCases()[0].Claim() // pod-level identity: runAsUser 1000, fsGroup 3000
	claim.RunID, claim.JobName = "run-secctx-upload", "run-secctx-upload"
	claim.Stages = []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "write", Run: "echo owner-only > out.txt && chmod 600 out.txt && stat -c '%a %u' out.txt"}},
		{Step: &api.ClaimStep{Index: 1, StageIndex: 1, Name: "upload", UploadArtifact: &api.UploadArtifactStep{Name: "out", Path: "out.txt"}}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	a.executeRun(ctx, claim, workDir)

	obs := h.observation()
	paritycases.Assert(t, paritycases.Expectation{
		StepStatus:     map[string]string{"write": "Succeeded", "upload": "Succeeded"},
		RunFinished:    "Succeeded",
		LogMustContain: []paritycases.LogLine{{Step: "write", Stream: "stdout", Substring: "600 1000"}},
	}, obs)
	require.Equal(t, "owner-only\n", string(uploaded), "the uploaded artifact must carry the 0600 file's content")
}

// rootCleanedTempDir is a t.TempDir that the root cleanup container empties first. A
// container running as root (or any other UID) leaves files in the workspace
// that the non-root user running the test, such as a CI runner, cannot delete,
// which would fail the test in TempDir's own cleanup. The agent's workspace
// sweep has the same fallback (wipeDir).
func rootCleanedTempDir(t *testing.T, a *Agent) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		if err := wipeDir(context.Background(), dir, true, a.containerRuntime, a.ToolsDir); err != nil {
			t.Logf("cleanup %s: %v", dir, err)
		}
	})
	return dir
}
