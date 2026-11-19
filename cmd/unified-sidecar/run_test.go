package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/objtransfer"
	"github.com/klauspost/compress/zstd"
)

// seededStore returns a cache.Remote over a LocalObjectStore (rooted at a
// fresh t.TempDir) with a cache entry already saved under key, so a "restore
// --key <key>" call hits.
func seededStore(t *testing.T, key string) cache.Remote {
	t.Helper()
	remote := &cache.StoreRemote{Store: objectstore.NewLocalObjectStore(t.TempDir()), JobName: "test-job"}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "dep.txt"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cache.Save(context.Background(), remote, src, key, 7); err != nil {
		t.Fatal(err)
	}
	return remote
}

// emptyStore returns a cache.Remote over a fresh, empty LocalObjectStore, so
// any "restore" call against it misses.
func emptyStore(t *testing.T) cache.Remote {
	t.Helper()
	return &cache.StoreRemote{Store: objectstore.NewLocalObjectStore(t.TempDir()), JobName: "test-job"}
}

// localProvider returns a client provider whose transfers go through a fake
// controller (controller mode) backed by a LocalObjectStore rooted at dir.
func localProvider(t *testing.T, dir string) clientProvider {
	t.Helper()
	return newFakeController(t, objectstore.NewLocalObjectStore(dir)).provider(t, "test-token")
}

// erroringProvider is a client provider that always fails, as newClient does
// when the k8s agent's settings are absent. Used to prove that "idle" never
// invokes the provider, and that cache/artifact subcommands fail loudly when
// it does.
func erroringProvider(runID string) (*objtransfer.Client, error) {
	return nil, errNoControllerEnv
}

// TestExtractPodRun covers every accepted spelling of --pod-run, that it is
// unaffected by other flags around it, and that it degrades to "" (never an
// error/panic) when absent or trailing with no value.
func TestExtractPodRun(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"space form", []string{"--key", "k", "--pod-run", "run-1", "--path", "/x"}, "run-1"},
		{"equals form", []string{"--key", "k", "--pod-run=run-2", "--path", "/x"}, "run-2"},
		{"single-dash space form", []string{"-pod-run", "run-3"}, "run-3"},
		{"single-dash equals form", []string{"-pod-run=run-4"}, "run-4"},
		{"absent", []string{"--key", "k", "--path", "/x"}, ""},
		{"empty args", []string{}, ""},
		{"trailing with no value", []string{"--key", "k", "--pod-run"}, ""},
		{"first flag", []string{"--pod-run", "run-5", "--key", "k"}, "run-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractPodRun(tc.args); got != tc.want {
				t.Fatalf("extractPodRun(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// TestRun_ThreadsPodRunToClientProvider verifies run()'s dispatcher extracts
// --pod-run from the subcommand's own args and passes it to newClient BEFORE
// the subcommand's own FlagSet parses them, for cache and artifact alike, and
// that it is not the artifact's own --run.
func TestRun_ThreadsPodRunToClientProvider(t *testing.T) {
	inner := localProvider(t, t.TempDir())
	var gotRunID string
	prov := func(runID string) (*objtransfer.Client, error) {
		gotRunID = runID
		return inner(runID)
	}

	code := run(context.Background(), prov,
		[]string{"cache", "restore", "--key", "k1", "--root", fsRoot(), "--path", t.TempDir(), "--job", "j", "--pod-run", "run-cache"},
		io.Discard)
	if code != 0 {
		t.Fatalf("cache restore exit=%d", code)
	}
	if gotRunID != "run-cache" {
		t.Fatalf("cache restore: newClient got runID=%q, want %q", gotRunID, "run-cache")
	}

	gotRunID = ""
	_ = run(context.Background(), prov,
		[]string{"artifact", "download", "--run", "artifact-source-run", "--name", "nope", "--root", fsRoot(), "--dest", t.TempDir(), "--pod-run", "run-artifact"},
		io.Discard) // expected to fail (no such artifact); only gotRunID matters here
	if gotRunID != "run-artifact" {
		t.Fatalf("artifact download: newClient got runID=%q, want %q (must NOT be the artifact's own --run)", gotRunID, "run-artifact")
	}
}

// TestRun_CacheRestoreUsesPresignClient: the sidecar asks the controller for a
// presigned get on the Pod's run, authenticated by the token file, and fetches
// the archive from the URL it was handed.
func TestRun_CacheRestoreUsesPresignClient(t *testing.T) {
	remote := seededStore(t, "k")
	rc, _, err := remote.Open(context.Background(), "k", nil)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}

	fc := newFakeController(t, objectstore.NewLocalObjectStore(t.TempDir()))
	fc.direct = true
	fc.blobs["k"] = blob
	prov := fc.provider(t, "pod-token-123\n")

	dest := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := runCmd(t, prov, &stdout, &stderr, "cache", "restore", "--key", "k", "--root", fsRoot(), "--path", dest, "--job", "test-job", "--pod-run", "mine")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "UCD_CACHE_RESULT=hit") {
		t.Fatalf("stdout = %q, want UCD_CACHE_RESULT=hit", stdout.String())
	}
	var presign *seenRequest
	for _, r := range fc.requests() {
		if r.Presign != nil {
			r := r
			presign = &r
		}
	}
	if presign == nil {
		t.Fatal("no presign request reached the controller")
	}
	if presign.Path != "/api/v1/runs/mine/objects/presign" {
		t.Fatalf("presign path = %q", presign.Path)
	}
	if presign.Auth != "Bearer pod-token-123" {
		t.Fatalf("Authorization = %q, want the token file's contents as a bearer", presign.Auth)
	}
	if presign.Presign.Op != api.ObjectOpGet || presign.Presign.Kind != api.ObjectKindCache || presign.Presign.Key != "k" {
		t.Fatalf("presign request = %+v", presign.Presign)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "dep.txt")); err != nil || string(got) != "cached" {
		t.Fatalf("dep.txt = %q, %v", got, err)
	}
	if strings.Contains(stderr.String(), "/blob/") || strings.Contains(stderr.String(), "pod-token") {
		t.Fatalf("stderr leaks a URL or the token: %q", stderr.String())
	}
}

// TestRun_ArtifactDownloadCrossRunSendsSourceRunID: --run is the source run
// and maps to sourceRunId; --pod-run is the presign path's run.
func TestRun_ArtifactDownloadCrossRunSendsSourceRunID(t *testing.T) {
	store := objectstore.NewLocalObjectStore(t.TempDir())
	fc := newFakeController(t, store)
	prov := fc.provider(t, "tok")

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "out.txt"), []byte("other-run"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run(context.Background(), prov, []string{"artifact", "upload", "--run", "other", "--name", "build", "--root", fsRoot(), "--path", src, "--pod-run", "other"}, io.Discard); code != 0 {
		t.Fatalf("upload exit=%d", code)
	}
	dest := t.TempDir()
	if code := run(context.Background(), prov, []string{"artifact", "download", "--run", "other", "--name", "build", "--root", fsRoot(), "--dest", dest, "--pod-run", "mine"}, io.Discard); code != 0 {
		t.Fatalf("download exit=%d", code)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "out.txt")); err != nil || string(got) != "other-run" {
		t.Fatalf("out.txt = %q, %v", got, err)
	}
	var found bool
	for _, r := range fc.requests() {
		if r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/api/v1/runs/mine/objects/artifacts/") {
			found = true
			if !strings.Contains(r.Query, "sourceRunId=other") {
				t.Fatalf("download query = %q, want sourceRunId=other", r.Query)
			}
		}
	}
	if !found {
		t.Fatalf("no download under the Pod's run path: %+v", fc.requests())
	}
}

// TestRun_JobFlagOptional: --job is used only for logging, so cache save
// without it succeeds and commits the entry.
func TestRun_JobFlagOptional(t *testing.T) {
	fc := newFakeController(t, objectstore.NewLocalObjectStore(t.TempDir()))
	prov := fc.provider(t, "tok")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "dep.txt"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code := run(context.Background(), prov, []string{"cache", "save", "--key", "k", "--root", fsRoot(), "--path", src, "--pod-run", "r1"}, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "cache saved: k") {
		t.Fatalf("stderr = %q, want a saved line", stderr.String())
	}
	dest := t.TempDir()
	if code := run(context.Background(), prov, []string{"cache", "restore", "--key", "k", "--root", fsRoot(), "--path", dest, "--pod-run", "r1"}, io.Discard); code != 0 {
		t.Fatalf("restore exit=%d", code)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "dep.txt")); err != nil || string(got) != "cached" {
		t.Fatalf("restore after job-less save: dep.txt = %q, %v (the save did not commit)", got, err)
	}
}

// TestRun_ControllerModeFallback: when the presign endpoint answers
// {"mode":"controller"} the transfer goes to the controller's streaming route.
func TestRun_ControllerModeFallback(t *testing.T) {
	fc := newFakeController(t, objectstore.NewLocalObjectStore(t.TempDir()))
	prov := fc.provider(t, "tok")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "dep.txt"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run(context.Background(), prov, []string{"cache", "save", "--key", "k", "--root", fsRoot(), "--path", src, "--pod-run", "r1"}, io.Discard); code != 0 {
		t.Fatalf("save exit=%d", code)
	}
	var stream bool
	for _, r := range fc.requests() {
		if r.Method == http.MethodPut && r.Path == "/api/v1/runs/r1/objects/caches" {
			stream = true
			if r.Auth != "Bearer tok" {
				t.Fatalf("streaming upload Authorization = %q", r.Auth)
			}
		}
	}
	if !stream {
		t.Fatalf("no PUT to /objects/caches: %+v", fc.requests())
	}
}

// TestRun_NoControllerEnv_ExactMessage: with the k8s agent's settings absent a
// cache or artifact subcommand exits 1 with the documented message, while idle
// is unaffected.
func TestRun_NoControllerEnv_ExactMessage(t *testing.T) {
	t.Setenv(envControllerURL, "")
	t.Setenv(envPodTokenFile, "")
	for _, group := range []string{"cache", "artifact"} {
		var stderr bytes.Buffer
		args := []string{group, "restore", "--root", fsRoot()}
		if group == "artifact" {
			args = []string{group, "upload", "--root", fsRoot()}
		}
		if code := run(context.Background(), newClient, args, &stderr); code != 1 {
			t.Fatalf("%s: exit=%d, want 1", group, code)
		}
		want := group + " requires UNIFIED_CONTROLLER_URL and UNIFIED_POD_TOKEN_FILE (set by the k8s agent)\n"
		if stderr.String() != want {
			t.Fatalf("%s: stderr = %q, want %q", group, stderr.String(), want)
		}
	}
}

func TestRunCache_RestoreEmitsHitMarkerToStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ec := runCache(context.Background(), seededStore(t, "k"), "restore",
		[]string{"--key", "k", "--root", fsRoot(), "--path", t.TempDir(), "--job", "test-job"}, &stdout, &stderr)
	if ec != 0 {
		t.Fatalf("exit=%d", ec)
	}
	if !strings.Contains(stdout.String(), "UCD_CACHE_RESULT=hit") {
		t.Fatalf("stdout = %q, want UCD_CACHE_RESULT=hit", stdout.String())
	}
	if strings.Contains(stderr.String(), "UCD_CACHE_RESULT") {
		t.Fatalf("marker leaked onto stderr: %q", stderr.String())
	}
}

func TestRunCache_RestoreEmitsMissMarkerToStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ec := runCache(context.Background(), emptyStore(t), "restore",
		[]string{"--key", "absent", "--root", fsRoot(), "--path", t.TempDir(), "--job", "test-job"}, &stdout, &stderr)
	if ec != 0 {
		t.Fatalf("exit=%d", ec)
	}
	if !strings.Contains(stdout.String(), "UCD_CACHE_RESULT=miss") {
		t.Fatalf("stdout = %q, want UCD_CACHE_RESULT=miss", stdout.String())
	}
	if strings.Contains(stderr.String(), "UCD_CACHE_RESULT") {
		t.Fatalf("marker leaked onto stderr: %q", stderr.String())
	}
}

func TestRun_CacheSaveThenRestore(t *testing.T) {
	prov := localProvider(t, t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "dep.txt"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if code := run(ctx, prov, []string{"cache", "save", "--key", "k1", "--ttl-days", "7", "--root", fsRoot(), "--path", src, "--job", "test-job"}, io.Discard); code != 0 {
		t.Fatalf("cache save exit=%d", code)
	}
	dest := t.TempDir()
	if code := run(ctx, prov, []string{"cache", "restore", "--key", "k1", "--root", fsRoot(), "--path", dest, "--job", "test-job"}, io.Discard); code != 0 {
		t.Fatalf("cache restore exit=%d", code)
	}
	got, err := os.ReadFile(filepath.Join(dest, "dep.txt"))
	if err != nil || string(got) != "cached" {
		t.Fatalf("dep.txt = %q, %v", got, err)
	}
}

func TestRun_CacheRestoreMiss_Exit0(t *testing.T) {
	prov := localProvider(t, t.TempDir())
	dest := t.TempDir()
	if code := run(context.Background(), prov, []string{"cache", "restore", "--key", "nope", "--root", fsRoot(), "--path", dest, "--job", "test-job"}, io.Discard); code != 0 {
		t.Fatalf("cache restore miss should exit 0, got %d", code)
	}
}

func TestRun_ArtifactUploadDownload(t *testing.T) {
	prov := localProvider(t, t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "out.txt"), []byte("art"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if code := run(ctx, prov, []string{"artifact", "upload", "--run", "r1", "--name", "build", "--root", fsRoot(), "--path", src}, io.Discard); code != 0 {
		t.Fatalf("artifact upload exit=%d", code)
	}
	dest := t.TempDir()
	if code := run(ctx, prov, []string{"artifact", "download", "--run", "r1", "--name", "build", "--root", fsRoot(), "--dest", dest}, io.Discard); code != 0 {
		t.Fatalf("artifact download exit=%d", code)
	}
	got, err := os.ReadFile(filepath.Join(dest, "out.txt"))
	if err != nil || string(got) != "art" {
		t.Fatalf("out.txt = %q, %v", got, err)
	}
}

func TestRun_ArtifactDownloadMissing_Exit1(t *testing.T) {
	prov := localProvider(t, t.TempDir())
	if code := run(context.Background(), prov, []string{"artifact", "download", "--run", "r1", "--name", "nope", "--root", fsRoot(), "--dest", t.TempDir()}, io.Discard); code == 0 {
		t.Fatal("missing artifact download should exit non-zero")
	}
}

func TestRun_Idle_BlocksUntilCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, erroringProvider, []string{"idle"}, io.Discard) }()
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("idle exit=%d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle did not return after cancel")
	}
}

// Regression test for TODO #21: idle must survive degraded mode (no controller settings)
// by never invoking the client provider at all.
func TestRun_Idle_NeverInvokesProvider_DegradedMode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	called := false
	prov := func(string) (*objtransfer.Client, error) {
		called = true
		return nil, errors.New("should never be called")
	}
	done := make(chan int, 1)
	go func() { done <- run(ctx, prov, []string{"idle"}, io.Discard) }()
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("idle exit=%d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle did not return after cancel")
	}
	if called {
		t.Fatal("idle must not invoke the client provider (degraded mode should stay resident)")
	}
}

// Regression test for TODO #21: cache/artifact subcommands must fail loudly
// (non-zero exit, clear message) when the client provider errors, instead of
// crashing the whole process before dispatch.
func TestRun_CacheSubcommand_ProviderError_FailsLoudly(t *testing.T) {
	var stderr strings.Builder
	code := run(context.Background(), erroringProvider, []string{"cache", "restore", "--key", "k1", "--root", fsRoot(), "--path", t.TempDir()}, &stderr)
	if code == 0 {
		t.Fatal("cache restore with erroring provider should exit non-zero")
	}
	if !strings.Contains(stderr.String(), "UNIFIED_CONTROLLER_URL") {
		t.Fatalf("expected a clear controller-settings error message, got: %q", stderr.String())
	}
}

func TestRun_ArtifactSubcommand_ProviderError_FailsLoudly(t *testing.T) {
	var stderr strings.Builder
	code := run(context.Background(), erroringProvider, []string{"artifact", "upload", "--run", "r1", "--name", "n1", "--root", fsRoot(), "--path", t.TempDir()}, &stderr)
	if code == 0 {
		t.Fatal("artifact upload with erroring provider should exit non-zero")
	}
	if !strings.Contains(stderr.String(), "UNIFIED_CONTROLLER_URL") {
		t.Fatalf("expected a clear controller-settings error message, got: %q", stderr.String())
	}
}

// fsRoot is the filesystem root of the temp directory's volume, a --root that
// contains every t.TempDir() the tests pass as --path or --dest.
func fsRoot() string {
	return filepath.VolumeName(os.TempDir()) + string(filepath.Separator)
}

// A restore that skips symlink/hardlink entries still reports a hit, but must
// say so in the step log so the user knows the tree may be incomplete.
func TestRunCache_RestoreWarnsWhenLinksSkipped(t *testing.T) {
	// Build the entry by hand so the test needs no symlink privilege (Windows)
	// and does not depend on how the archiver treats links: seed a real entry
	// with the rooted save path, then replace its archive with one holding a
	// regular file plus a symlink and a hardlink entry.
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "dep.txt"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := objectstore.NewLocalObjectStore(t.TempDir())
	ctx := context.Background()
	if err := cache.SaveIn(ctx, &cache.StoreRemote{Store: store, JobName: "test-job"}, src, src, "k", 7, nil); err != nil {
		t.Fatal(err)
	}
	keys, err := store.List(ctx, "caches/")
	if err != nil {
		t.Fatal(err)
	}
	var archiveKey string
	for _, k := range keys {
		if strings.HasSuffix(k, ".tar.zst") {
			archiveKey = k
		}
	}
	if archiveKey == "" {
		t.Fatalf("no archive object in %v", keys)
	}
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	body := []byte("cached")
	for _, h := range []*tar.Header{
		{Name: "dep.txt", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(body))},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "dep.txt", Mode: 0o777},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, archiveKey, bytes.NewReader(buf.Bytes()), int64(buf.Len())); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	var stdout, stderr bytes.Buffer
	ec := runCache(context.Background(), &cache.StoreRemote{Store: store, JobName: "test-job"}, "restore",
		[]string{"--key", "k", "--root", fsRoot(), "--path", dest, "--job", "test-job"}, &stdout, &stderr)
	if ec != 0 {
		t.Fatalf("exit=%d", ec)
	}
	if !strings.Contains(stdout.String(), "UCD_CACHE_RESULT=hit") {
		t.Fatalf("stdout = %q, want hit marker", stdout.String())
	}
	if !strings.Contains(stderr.String(), "cache restore warning: skipped 1 symlink/hardlink entries") {
		t.Fatalf("stderr = %q, want a skipped-links warning", stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(dest, "link")); err == nil {
		t.Fatal("link was restored; it must be skipped")
	}
}

func TestRunCache_RestoreNoWarningWithoutLinks(t *testing.T) {
	var stdout, stderr bytes.Buffer
	runCache(context.Background(), seededStore(t, "k"), "restore",
		[]string{"--key", "k", "--root", fsRoot(), "--path", t.TempDir(), "--job", "test-job"}, &stdout, &stderr)
	if strings.Contains(stderr.String(), "warning") {
		t.Fatalf("unexpected warning: %q", stderr.String())
	}
}

// TestNewClient_SendsTokenFileAndPicksUpRotation drives the production
// newClient: the controller receives the token file's contents as the bearer,
// and rewriting the file (the kubelet rotates the projected token) is picked up
// by the next request of the SAME client.
func TestNewClient_SendsTokenFileAndPicksUpRotation(t *testing.T) {
	fc := newFakeController(t, objectstore.NewLocalObjectStore(t.TempDir()))
	prov := fc.provider(t, "token-one\n")
	client, err := prov("run-x")
	if err != nil {
		t.Fatal(err)
	}
	open := func() {
		t.Helper()
		// A cache miss is fine: only the presign request's header matters.
		if rc, _, err := client.Caches().Open(context.Background(), "k", nil); err == nil {
			rc.Close()
		}
	}
	lastAuth := func() string {
		t.Helper()
		var auth string
		for _, r := range fc.requests() {
			if r.Presign != nil || strings.Contains(r.Path, "/objects/") {
				auth = r.Auth
			}
		}
		return auth
	}

	open()
	if got := lastAuth(); got != "Bearer token-one" {
		t.Fatalf("first request Authorization = %q, want the token file's contents", got)
	}
	if err := os.WriteFile(fc.tokenFile, []byte("token-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	open()
	if got := lastAuth(); got != "Bearer token-two" {
		t.Fatalf("after rotation Authorization = %q, want the new token", got)
	}

	if err := os.Remove(fc.tokenFile); err != nil {
		t.Fatal(err)
	}
	if rc, _, err := client.Caches().Open(context.Background(), "k", nil); err == nil {
		rc.Close()
		t.Fatal("a missing token file must fail the request")
	} else if strings.Contains(err.Error(), "token-") {
		t.Fatalf("error leaks the token: %v", err)
	}
}
