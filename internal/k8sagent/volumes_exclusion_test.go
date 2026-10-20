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

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// exclusionGolden is the host agent's testdata/volume_exclusion_golden.json,
// read from its package: the one statement of the rule that both agents are held
// to, so neither can drift from the other without a test failing.
type exclusionGolden struct {
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

func loadExclusionGolden(t *testing.T) exclusionGolden {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "agent", "testdata", "volume_exclusion_golden.json"))
	require.NoError(t, err)
	var g exclusionGolden
	require.NoError(t, json.Unmarshal(b, &g))
	return g
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

func runWithGolden(t *testing.T, g exclusionGolden, step *api.ClaimStep) (*volFixture, *argvExec) {
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
// ExcludesTheVolumeAndSaysSo, from the same golden file).
func TestExecuteRun_ArchiveOfAnAncestorExcludesTheVolumeAndSaysSo(t *testing.T) {
	g := loadExclusionGolden(t)

	t.Run("artifact", func(t *testing.T) {
		f, rec := runWithGolden(t, g, &api.ClaimStep{Index: 0, Name: "up", UploadArtifact: &api.UploadArtifactStep{Name: "out", Path: g.ArchivePath}})
		argv := rec.sidecarArgv("artifact", "upload")
		require.NotNil(t, argv, "the sidecar was asked to upload")
		require.Equal(t, g.Notices["artifact"], f.exclusionNotices())
		require.Equal(t, rebase(g.Excluded), flagValues(argv, "--exclude"), "every volume's mount point is passed to the sidecar to leave out")
	})

	t.Run("cache", func(t *testing.T) {
		f, rec := runWithGolden(t, g, &api.ClaimStep{Index: 0, Name: "c", Cache: &dsl.CacheStep{Path: g.ArchivePath, Key: "k"}})
		argv := rec.sidecarArgv("cache", "save")
		require.NotNil(t, argv, "the sidecar was asked to save")
		require.Equal(t, g.Notices["cache"], f.exclusionNotices())
		require.Equal(t, rebase(g.Excluded), flagValues(argv, "--exclude"))
	})
}

// A path inside a volume, or an archive that holds no mount point, passes no
// exclusion and announces nothing: only an ancestor of a mount point does.
func TestExecuteRun_ArchiveNotAboveAVolumeExcludesNothing(t *testing.T) {
	g := loadExclusionGolden(t)
	for _, p := range []string{".ucd-cache/go", ".ucd-cache/go/obj", "src"} {
		f, rec := runWithGolden(t, g, &api.ClaimStep{Index: 0, Name: "up", UploadArtifact: &api.UploadArtifactStep{Name: "out", Path: p}})
		argv := rec.sidecarArgv("artifact", "upload")
		require.NotNil(t, argv, p)
		require.Empty(t, flagValues(argv, "--exclude"), p)
		require.Empty(t, f.exclusionNotices(), p)
	}
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
	g := loadExclusionGolden(t)
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
			writeFile(t, filepath.Join(ws, filepath.FromSlash(mt.MountPath), filepath.FromSlash(f)))
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
}

func writeFile(t *testing.T, p string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
}
