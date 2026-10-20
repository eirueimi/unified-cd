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

// A volume's contents are not part of an archive of its ancestor, and the run
// log says so. The same expectations are checked against the Kubernetes agent
// (internal/k8sagent), from the same golden file.
func TestExecuteRun_ArchiveOfAnAncestorExcludesTheVolumeAndSaysSo(t *testing.T) {
	g := loadExclusionGolden(t)
	var vols []string
	var mounts []dsl.VolumeMount
	for _, mt := range g.Mounts {
		vols = append(vols, mt.Name)
		mounts = append(mounts, vmount(mt.Name, mt.Name, mt.MountPath))
	}
	for name, native := range map[string]bool{"isolated": false, "native": true} {
		t.Run(name, func(t *testing.T) {
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

// A cache save follows the same rule as an artifact upload: the cache of an
// ancestor is not the place to keep a volume.
func TestHostBackend_CacheSaveOfAnAncestorExcludesTheVolumeAndSaysSo(t *testing.T) {
	g := loadExclusionGolden(t)

	var mu sync.Mutex
	var logs []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/a1/runs/r1/steps/{idx}/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		var reqs []api.LogAppendRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&reqs))
		mu.Lock()
		for _, l := range reqs {
			logs = append(logs, l.Line)
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var cfgs []config.VolumeConfig
	var mounts []dsl.VolumeMount
	for _, mt := range g.Mounts {
		cfgs = append(cfgs, exclusive(mt.Name))
		mounts = append(mounts, vmount(mt.Name, mt.Name, mt.MountPath))
	}
	m, _ := newTestVolumes(t, cfgs...)
	b, workDir := isolatedBackendWithVolumes(t, m, mounts...)
	store := objectstore.NewLocalObjectStore(t.TempDir())
	b.a = &Agent{ID: "a1", Client: NewClient(srv.URL, "tok"), CacheStore: store}
	b.runID = "r1"
	writeFiles(t, workDir, g.WorkspaceFiles...)
	for _, mt := range g.Mounts {
		writeFiles(t, m.PathOf(mt.Name), g.VolumeFiles...)
		// The empty mountpoint an isolated run leaves in the workspace (mountVolumes).
		require.NoError(t, os.MkdirAll(filepath.Join(workDir, filepath.FromSlash(mt.MountPath)), 0o755))
	}

	resolved, err := b.ResolveCachePath(ScopeHandle{}, g.ArchivePath)
	require.NoError(t, err)
	require.NoError(t, b.CacheSave(context.Background(), ScopeHandle{}, "k", resolved, 7))

	restored := t.TempDir()
	hit, err := cache.Restore(context.Background(), store, "job", restored, "k", nil)
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, g.Entries, listEntries(t, restored))
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, g.Notices["cache"], logs)
}
