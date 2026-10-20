package agent

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// uploadRun is what one run of a single upload-artifact step produced.
type uploadRun struct {
	status  string
	entries []string // the uploaded archive's entries, slash-separated, sorted
	logs    []string // the run-level (step -1) log lines
}

// runUploadStep drives executeRun for a claim that mounts mounts and uploads
// path, on the host agent, isolated or native. seed fills the workspace and the
// volumes before the run. The upload endpoint is a stub that keeps the archive.
func runUploadStep(t *testing.T, native bool, vols []string, mounts []dsl.VolumeMount, path string, seed func(workDir string, m *VolumeManager)) uploadRun {
	t.Helper()
	const agentID = "iso-vol-art-agent"
	h := newIsolatedHarness()
	inner := newIsolatedServer(t, agentID, h)

	var mu sync.Mutex
	var uploaded []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/artifacts/") {
			b, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			mu.Lock()
			uploaded = b
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	var cfgs []config.VolumeConfig
	for _, v := range vols {
		cfgs = append(cfgs, exclusive(v))
	}
	m, _ := newTestVolumes(t, cfgs...)
	workDir := t.TempDir()
	seed(workDir, m)

	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok"), PauseImage: "pause:img", RunnerImage: "runner:img", Volumes: m}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = &podFakeRT{}

	claim := api.ClaimResponse{
		RunID: "run-iso-vol-art", JobName: "j", Native: native, Volumes: mounts,
		Stages: []api.ClaimStage{{Step: &api.ClaimStep{
			Index: 0, Name: "up", UploadArtifact: &api.UploadArtifactStep{Name: "out", Path: path},
		}}},
	}
	if !native {
		claim.PodTemplate = mysqlTemplate()
	}
	a.executeRun(context.Background(), claim, workDir)

	var res uploadRun
	h.mu.Lock()
	res.status = h.finishStatus
	for _, l := range h.logsByStep[-1] {
		res.logs = append(res.logs, l.Line)
	}
	h.mu.Unlock()

	mu.Lock()
	body := uploaded
	mu.Unlock()
	require.NotEmpty(t, body, "the upload endpoint must have been hit")
	dest := t.TempDir()
	require.NoError(t, artifact.ExtractTarZstd(strings.NewReader(string(body)), dest), "the archive extracts: it holds no link")
	res.entries = listEntries(t, dest)
	return res
}

func listEntries(t *testing.T, dir string) []string {
	t.Helper()
	var got []string
	require.NoError(t, filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel, _ := filepath.Rel(dir, p); rel != "." {
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	}))
	sort.Strings(got)
	return got
}

func writeFiles(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, n := range names {
		p := filepath.Join(root, filepath.FromSlash(n))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(n), 0o600))
	}
}

// scenarioMounts returns a scenario's volumes as a claim names them.
func scenarioMounts(g exclusionScenario) (vols []string, mounts []dsl.VolumeMount) {
	for _, mt := range g.Mounts {
		vols = append(vols, mt.Name)
		mounts = append(mounts, vmount(mt.Name, mt.Name, mt.MountPath))
	}
	return vols, mounts
}

// An isolated run whose upload-artifact path lies inside a volume mount used to
// be failed (the host mountpoint is empty). It must now archive the volume's
// own contents, which is what the same job does on Kubernetes.
func TestExecuteRun_UploadsAnArtifactFromInsideAVolume(t *testing.T) {
	run := runUploadStep(t, false, []string{"go-cache"}, []dsl.VolumeMount{vmount("go-cache", "gc", ".ucd-cache/go")}, ".ucd-cache/go/obj",
		func(workDir string, m *VolumeManager) { writeFiles(t, m.PathOf("go-cache"), "obj/out.txt") })

	require.Equal(t, "Succeeded", run.status, "the artifact path is reachable, so the run is green")
	require.Equal(t, []string{"out.txt"}, run.entries, "the archive holds the volume's file, not an empty mountpoint")
	require.Empty(t, run.logs, "naming a volume's own path excludes nothing, so there is nothing to announce")
}

// A native claim reaches a volume through a link in the workspace, and an
// archiver does not follow a link it is pointed at: an artifact of the mount
// point itself used to store the link, which extraction refuses. At the mount
// point, and inside it, the artifact is the volume's real contents.
func TestExecuteRun_NativeArtifactAtAndInsideAVolume(t *testing.T) {
	for path, want := range map[string][]string{
		".ucd-cache/go":     {"obj", "obj/out.txt", "top.txt"},
		".ucd-cache/go/obj": {"out.txt"},
	} {
		t.Run(path, func(t *testing.T) {
			run := runUploadStep(t, true, []string{"go-cache"}, []dsl.VolumeMount{vmount("go-cache", "gc", ".ucd-cache/go")}, path,
				func(workDir string, m *VolumeManager) { writeFiles(t, m.PathOf("go-cache"), "obj/out.txt", "top.txt") })

			require.Equal(t, "Succeeded", run.status)
			require.Equal(t, want, run.entries)
			require.Empty(t, run.logs, "naming a volume's own path excludes nothing")
		})
	}
}

// A volume's contents are not part of an archive of its ancestor, and the run
// log says so. The same expectations are checked against the Kubernetes agent
// (internal/k8sagent), from the same golden file. An absolute mount path that
// lands inside the workspace is the same case as a relative one; a native claim
// cannot mount that way, so only the isolated claim runs that scenario.
func TestExecuteRun_ArchiveOfAnAncestorExcludesTheVolumeAndSaysSo(t *testing.T) {
	for _, scenario := range scenarioNames(t) {
		g := exclusionScenarios(t)[scenario]
		vols, mounts := scenarioMounts(g)
		for name, native := range map[string]bool{"isolated": false, "native": true} {
			if native && scenario == "absolute" {
				continue
			}
			t.Run(scenario+"/"+name, func(t *testing.T) {
				run := runUploadStep(t, native, vols, mounts, g.ArchivePath, func(workDir string, m *VolumeManager) {
					writeFiles(t, workDir, g.WorkspaceFiles...)
					for _, mt := range g.Mounts {
						writeFiles(t, m.PathOf(mt.Name), g.VolumeFiles...)
					}
				})

				require.Equal(t, "Succeeded", run.status)
				require.Equal(t, g.Entries, run.entries, "the archive is the workspace without the volumes, whatever the volumes hold")
				require.Equal(t, g.Notices["artifact"], run.logs, "one line per excluded volume, naming the path, the volume and its mount path")
			})
		}
	}
}

// A volume nested in another is left out of an archive of the outer one, and a
// volume at the root of the archive is archived in full.
func TestExecuteRun_ArchiveOfAVolumeExcludesVolumesNestedInIt(t *testing.T) {
	run := runUploadStep(t, false, []string{"outer", "inner"},
		[]dsl.VolumeMount{vmount("outer", "o", "dir"), vmount("inner", "i", "dir/inner")}, "dir",
		func(workDir string, m *VolumeManager) {
			writeFiles(t, m.PathOf("outer"), "o.txt")
			writeFiles(t, m.PathOf("inner"), "i.txt")
			// Where the container runtime creates the nested mountpoint.
			require.NoError(t, os.MkdirAll(filepath.Join(m.PathOf("outer"), "inner"), 0o755))
		})

	require.Equal(t, "Succeeded", run.status)
	require.Equal(t, []string{"o.txt"}, run.entries)
	require.Len(t, run.logs, 1)
	require.Contains(t, run.logs[0], `excludes volume "inner" mounted at "dir/inner"`)
}

// cacheHarness is an isolated host backend over a local cache store, with a
// stub controller that keeps the run-level (step -1) log lines.
type cacheHarness struct {
	b       *hostBackend
	m       *VolumeManager
	workDir string
	store   objectstore.ObjectStore
	mu      sync.Mutex
	logs    []string
}

func newCacheHarness(t *testing.T, g exclusionScenario) *cacheHarness {
	t.Helper()
	h := &cacheHarness{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/a1/runs/r1/steps/{idx}/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		var reqs []api.LogAppendRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&reqs))
		h.mu.Lock()
		for _, l := range reqs {
			h.logs = append(h.logs, l.Line)
		}
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var cfgs []config.VolumeConfig
	for _, mt := range g.Mounts {
		cfgs = append(cfgs, exclusive(mt.Name))
	}
	_, mounts := scenarioMounts(g)
	h.m, _ = newTestVolumes(t, cfgs...)
	h.b, h.workDir = isolatedBackendWithVolumes(t, h.m, mounts...)
	h.store = objectstore.NewLocalObjectStore(t.TempDir())
	h.b.a = &Agent{ID: "a1", Client: NewClient(srv.URL, "tok"), CacheStore: h.store}
	h.b.runID = "r1"
	return h
}

func (h *cacheHarness) announced() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.logs...)
}

// A cache save follows the same rule as an artifact upload: the cache of an
// ancestor is not the place to keep a volume.
func TestHostBackend_CacheSaveOfAnAncestorExcludesTheVolumeAndSaysSo(t *testing.T) {
	for _, scenario := range scenarioNames(t) {
		g := exclusionScenarios(t)[scenario]
		t.Run(scenario, func(t *testing.T) {
			h := newCacheHarness(t, g)
			writeFiles(t, h.workDir, g.WorkspaceFiles...)
			for _, mt := range g.Mounts {
				writeFiles(t, h.m.PathOf(mt.Name), g.VolumeFiles...)
				// The empty mountpoint an isolated run leaves in the workspace
				// (mountVolumes), where the mount path is a relative one.
				if !strings.HasPrefix(mt.MountPath, "/") {
					require.NoError(t, os.MkdirAll(filepath.Join(h.workDir, filepath.FromSlash(mt.MountPath)), 0o755))
				}
			}

			resolved, err := h.b.ResolveCachePath(ScopeHandle{}, g.ArchivePath)
			require.NoError(t, err)
			require.NoError(t, h.b.CacheSave(context.Background(), ScopeHandle{}, "k", resolved, 7))

			restored := t.TempDir()
			hit, err := cache.Restore(context.Background(), h.store, "job", restored, "k", nil)
			require.NoError(t, err)
			require.True(t, hit)
			require.Equal(t, g.Entries, listEntries(t, restored))
			require.Equal(t, g.Notices["cacheSave"], h.announced())
		})
	}
}

// Restore resolves a cache path exactly as save does. Inside a volume, both
// reach the volume's own directory: what one saved, the other puts back where
// the container will see it.
func TestHostBackend_CacheRoundTripInsideAVolumeStaysInTheVolume(t *testing.T) {
	g := exclusionScenarios(t)["relative"]
	h := newCacheHarness(t, g)
	vol := h.m.PathOf("go-cache")
	writeFiles(t, vol, "pkg/mod/a.go")

	resolved, err := h.b.ResolveCachePath(ScopeHandle{}, ".ucd-cache/go/pkg")
	require.NoError(t, err)
	require.NoError(t, h.b.CacheSave(context.Background(), ScopeHandle{}, "k", resolved, 7))
	require.NoError(t, os.RemoveAll(filepath.Join(vol, "pkg")))

	hit, err := h.b.CacheRestore(context.Background(), ScopeHandle{}, "k", nil, resolved)
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, []string{"pkg", "pkg/mod", "pkg/mod/a.go"}, listEntries(t, vol), "the restored files are in the volume")
	require.NoDirExists(t, filepath.Join(h.workDir, ".ucd-cache"), "and nothing was written to the host mountpoint")
	require.Empty(t, h.announced(), "naming a volume's own path excludes nothing")
}

// A restore of an ancestor must not write into a volume either. An archive saved
// before the volume was declared holds files at the mount path; extracting them
// would put them in the volume on Kubernetes, into a link's target on a native
// host, and into an empty mountpoint (so nowhere) on an isolated host.
func TestHostBackend_CacheRestoreOfAnAncestorLeavesTheVolumesAlone(t *testing.T) {
	for _, scenario := range scenarioNames(t) {
		g := exclusionScenarios(t)[scenario]
		if g.ArchivePath != "." {
			continue // the legacy archive here is one of the whole workspace
		}
		t.Run(scenario, func(t *testing.T) {
			h := newCacheHarness(t, g)

			// The archive as an older run saved it: volumes' files included.
			old := t.TempDir()
			writeFiles(t, old, g.WorkspaceFiles...)
			for _, mt := range g.Mounts {
				writeFiles(t, filepath.Join(old, filepath.FromSlash(strings.TrimPrefix(mt.MountPath, "/workspace/"))), "old.bin")
			}
			require.NoError(t, cache.Save(context.Background(), h.store, "job", old, "k", 7))

			for _, mt := range g.Mounts {
				writeFiles(t, h.m.PathOf(mt.Name), g.VolumeFiles...)
			}
			resolved, err := h.b.ResolveCachePath(ScopeHandle{}, g.ArchivePath)
			require.NoError(t, err)
			hit, err := h.b.CacheRestore(context.Background(), ScopeHandle{}, "k", nil, resolved)
			require.NoError(t, err)
			require.True(t, hit)

			require.Equal(t, g.Entries, listEntries(t, h.workDir), "the workspace is restored without what lay under the volumes")
			for _, mt := range g.Mounts {
				require.NotContains(t, listEntries(t, h.m.PathOf(mt.Name)), "old.bin", "volume %q", mt.Name)
			}
			require.Equal(t, g.Notices["cacheRestore"], h.announced())
		})
	}
}
