package k8sagent

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// exclusionScenario is an entry of the host agent's
// testdata/volume_exclusion_golden.json, read from its package: the one
// statement of the rule that both agents are held to, so neither can drift from
// the other without a test failing.
type exclusionScenario struct {
	Mounts []struct {
		Name      string `json:"name"`
		MountPath string `json:"mountPath"`
	} `json:"mounts"`
	ArchivePath    string              `json:"archivePath"`
	WorkspaceFiles []string            `json:"workspaceFiles"`
	VolumeFiles    []string            `json:"volumeFiles"`
	Entries        []string            `json:"entries"`
	Notices        map[string][]string `json:"notices"`
	Excluded       []string            `json:"excluded"`
}

func exclusionScenarios(t *testing.T) map[string]exclusionScenario {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "agent", "testdata", "volume_exclusion_golden.json"))
	require.NoError(t, err)
	var g struct {
		Scenarios map[string]exclusionScenario `json:"scenarios"`
	}
	require.NoError(t, json.Unmarshal(b, &g))
	return g.Scenarios
}

func scenarioNames(t *testing.T) []string {
	var names []string
	for n := range exclusionScenarios(t) {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// argvExec records every argv exec'd into a container (the sidecar's commands),
// and otherwise behaves as the fixture's exec.
type argvExec struct {
	inner *envCaptureExec
	mu    sync.Mutex
	argvs [][]string
}

func (a *argvExec) ExecStep(ctx context.Context, pod, container, script string, shell, env []string, stdout, stderr io.Writer) (int, error) {
	return a.inner.ExecStep(ctx, pod, container, script, shell, env, stdout, stderr)
}
func (a *argvExec) ExecStepArgv(ctx context.Context, pod, container string, argv []string, stdout, stderr io.Writer) (int, error) {
	a.mu.Lock()
	a.argvs = append(a.argvs, append([]string(nil), argv...))
	a.mu.Unlock()
	return a.inner.ExecStepArgv(ctx, pod, container, argv, stdout, stderr)
}

// sidecarArgv returns the argv of the sidecar command group/sub ("artifact",
// "upload"), or nil.
func (a *argvExec) sidecarArgv(group, sub string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, v := range a.argvs {
		if len(v) > 2 && v[0] == "unified-sidecar" && v[1] == group && v[2] == sub {
			return v
		}
	}
	return nil
}

func flagValues(argv []string, flag string) []string {
	var out []string
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			out = append(out, argv[i+1])
		}
	}
	return out
}

// exclusionNotices returns the run-log lines announcing an exclusion.
func (f *volFixture) exclusionNotices() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, body := range f.logs {
		var reqs []api.LogAppendRequest
		if json.Unmarshal([]byte(body), &reqs) != nil {
			continue
		}
		for _, r := range reqs {
			if strings.Contains(r.Line, "excludes volume") {
				out = append(out, r.Line)
			}
		}
	}
	return out
}

func runWithGolden(t *testing.T, g exclusionScenario, step *api.ClaimStep) (*volFixture, *argvExec) {
	t.Helper()
	var cfgs []config.VolumeConfig
	var mounts []dsl.VolumeMount
	for _, mt := range g.Mounts {
		cfgs = append(cfgs, vcfg(mt.Name, "exclusive", "1Gi"))
		mounts = append(mounts, mount(mt.Name, mt.Name, mt.MountPath))
	}
	f := newVolFixture(t, cfgs)
	f.a.cfg.SidecarS3SecretName = "s3creds"
	rec := &argvExec{inner: f.ex}
	f.a.exec = rec

	claim := f.claim(mounts)
	claim.Stages = []api.ClaimStage{{Step: step}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	f.a.executeRun(ctx, claim)
	require.Equal(t, api.RunSucceeded, f.waitFinish(t))
	return f, rec
}

// The Kubernetes agent leaves a volume out of an archive of its ancestor and
// says so, in the same words as the host agent (TestExecuteRun_ArchiveOfAnAncestor
// ExcludesTheVolumeAndSaysSo, from the same golden file). A cache step restores
// before it saves, and both leave the volumes out.
func TestExecuteRun_ArchiveOfAnAncestorExcludesTheVolumeAndSaysSo(t *testing.T) {
	for _, scenario := range scenarioNames(t) {
		g := exclusionScenarios(t)[scenario]
		t.Run(scenario+"/artifact", func(t *testing.T) {
			f, rec := runWithGolden(t, g, &api.ClaimStep{Index: 0, Name: "up", UploadArtifact: &api.UploadArtifactStep{Name: "out", Path: g.ArchivePath}})
			argv := rec.sidecarArgv("artifact", "upload")
			require.NotNil(t, argv, "the sidecar was asked to upload")
			require.Equal(t, g.Notices["artifact"], f.exclusionNotices())
			require.Equal(t, rebase(g.Excluded), flagValues(argv, "--exclude"), "every volume's mount point is passed to the sidecar to leave out")
		})

		t.Run(scenario+"/cache", func(t *testing.T) {
			f, rec := runWithGolden(t, g, &api.ClaimStep{Index: 0, Name: "c", Cache: &dsl.CacheStep{Path: g.ArchivePath, Key: "k"}})
			save := rec.sidecarArgv("cache", "save")
			restore := rec.sidecarArgv("cache", "restore")
			require.NotNil(t, save, "the sidecar was asked to save")
			require.NotNil(t, restore, "and to restore")
			require.Equal(t, append(append([]string(nil), g.Notices["cacheRestore"]...), g.Notices["cacheSave"]...), f.exclusionNotices())
			require.Equal(t, rebase(g.Excluded), flagValues(save, "--exclude"))
			require.Equal(t, rebase(g.Excluded), flagValues(restore, "--exclude"), "restore leaves the volumes alone exactly as save does")
		})
	}
}

// A path inside a volume, or an archive that holds no mount point, passes no
// exclusion and announces nothing: only an ancestor of a mount point does.
func TestExecuteRun_ArchiveNotAboveAVolumeExcludesNothing(t *testing.T) {
	g := exclusionScenarios(t)["relative"]
	for _, p := range []string{".ucd-cache/go", ".ucd-cache/go/obj", "src"} {
		f, rec := runWithGolden(t, g, &api.ClaimStep{Index: 0, Name: "up", UploadArtifact: &api.UploadArtifactStep{Name: "out", Path: p}})
		argv := rec.sidecarArgv("artifact", "upload")
		require.NotNil(t, argv, p)
		require.Empty(t, flagValues(argv, "--exclude"), p)
		require.Empty(t, f.exclusionNotices(), p)
	}
}

// The mount path's spelling does not matter: what counts is where the volume
// lands. A volume at /src/cache under a workspace at /src is the volume at
// "cache", and one outside the workspace is not a directory of it.
func TestMountedVolumes_LocatedByWhereTheyLand(t *testing.T) {
	vols := []PodVolume{
		{Name: "rel", MountPath: "cache"},
		{Name: "abs", MountPath: "/src/cache2"},
		{Name: "root", MountPath: "."},
		{Name: "outside", MountPath: "/root/.cache"},
	}
	got := map[string]string{}
	for _, v := range mountedVolumes("/src", vols) {
		got[v.Volume] = v.Location
	}
	require.Equal(t, map[string]string{"rel": "/src/cache", "abs": "/src/cache2", "outside": "/root/.cache"}, got)

	// An archive of the workspace excludes the two inside it, and not the third.
	var below []string
	for _, v := range agentlib.VolumesBelow("/src", mountedVolumes("/src", vols)) {
		below = append(below, v.Volume)
	}
	require.Equal(t, []string{"rel", "abs"}, below)
}

// rebase places workspace-relative paths in the pod's workspace.
func rebase(rel []string) []string {
	out := make([]string, len(rel))
	for i, r := range rel {
		out[i] = "/workspace/" + r
	}
	return out
}

// The two agents produce the same tarball. The Kubernetes sidecar sees the
// volumes inside the workspace, so this lays out the workspace as the sidecar
// sees it, archives it with the exclusions the agent passes to the sidecar, and
// requires exactly the entries the host agent's archive has (the golden's).
func TestArchiveOfAnAncestor_HostAndKubernetesProduceTheSameEntries(t *testing.T) {
	for _, scenario := range scenarioNames(t) {
		g := exclusionScenarios(t)[scenario]
		t.Run(scenario, func(t *testing.T) {
			_, rec := runWithGolden(t, g, &api.ClaimStep{Index: 0, Name: "up", UploadArtifact: &api.UploadArtifactStep{Name: "out", Path: g.ArchivePath}})
			argv := rec.sidecarArgv("artifact", "upload")
			require.NotNil(t, argv)

			// The container's workspace, with each volume's files at its mount path.
			ws := t.TempDir()
			for _, f := range g.WorkspaceFiles {
				writeFile(t, filepath.Join(ws, filepath.FromSlash(f)))
			}
			for _, mt := range g.Mounts {
				for _, f := range g.VolumeFiles {
					writeFile(t, filepath.Join(ws, filepath.FromSlash(strings.TrimPrefix(mt.MountPath, "/workspace/")), filepath.FromSlash(f)))
				}
			}
			var exclude []string
			for _, e := range flagValues(argv, "--exclude") {
				exclude = append(exclude, filepath.Join(ws, filepath.FromSlash(strings.TrimPrefix(e, "/workspace/"))))
			}

			rc := artifact.StreamTarZstdExcluding(ws, exclude)
			defer rc.Close()
			dest := t.TempDir()
			require.NoError(t, artifact.ExtractTarZstd(rc, dest))
			var got []string
			require.NoError(t, filepath.WalkDir(dest, func(p string, d fs.DirEntry, err error) error {
				if rel, _ := filepath.Rel(dest, p); err == nil && rel != "." {
					got = append(got, filepath.ToSlash(rel))
				}
				return err
			}))
			sort.Strings(got)
			require.Equal(t, g.Entries, got)
		})
	}
}

func writeFile(t *testing.T, p string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
}
