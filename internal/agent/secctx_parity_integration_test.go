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
		t.Skipf("no container runtime available (%s=%q): %v", containerRuntimeEnv, pref, err)
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

	probe := a.securityContextProbe()
	t.Logf("startup probe: ok=%v maxMappedID=%d reason=%q", probe.OK, probe.MaxMappedID, probe.Reason)
	if !probe.OK {
		// Without the probe passing the agent would not advertise
		// securitycontext, so these cases would never be routed here and their
		// results mean nothing. CI must fail loudly; a developer machine whose
		// bind mounts drop setgid (Windows, macOS) is told how to point the
		// test at a Linux path.
		msg := "the securityContext startup probe failed on this runtime/workspace: " + probe.Reason +
			" (set " + testWorkspaceBaseEnv + " to a directory with real Linux semantics)"
		if os.Getenv("CI") != "" {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}

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
		t.Skipf("ucd-sh shim not embedded (run `go generate ./internal/shim/embedded/` first): %v", err)
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
		t.Skipf("no container runtime available (%s=%q): %v", containerRuntimeEnv, pref, err)
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
	if p := a.securityContextProbe(); !p.OK {
		if os.Getenv("CI") != "" {
			t.Fatalf("securityContext startup probe failed on %s: %s", rt.Name(), p.Reason)
		}
		t.Skipf("securityContext startup probe failed (set %s to a Linux path): %s", testWorkspaceBaseEnv, p.Reason)
	}

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
