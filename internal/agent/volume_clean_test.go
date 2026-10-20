package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

// cleanGolden is the "cleanWorkspace" entry of
// testdata/volume_exclusion_golden.json: what wiping a volume mounted at the
// workspace root does to the volumes mounted below it. The Kubernetes agent is
// held to the same file (TestClearWorkspaceScript_LeavesNestedVolumesAlone
// AndEmptiesTheRest in internal/k8sagent).
type cleanGolden struct {
	Mounts []struct {
		Name      string `json:"name"`
		MountPath string `json:"mountPath"`
	} `json:"mounts"`
	RootFiles   []string `json:"rootFiles"`
	VolumeFiles []string `json:"volumeFiles"`
}

func cleanWorkspaceGolden(t *testing.T) cleanGolden {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "volume_exclusion_golden.json"))
	require.NoError(t, err)
	var g struct {
		Clean cleanGolden `json:"cleanWorkspace"`
	}
	require.NoError(t, json.Unmarshal(b, &g))
	require.NotEmpty(t, g.Clean.Mounts, "the golden file lost its cleanWorkspace scenario")
	return g.Clean
}

// cleanFixture lays out a root volume with the golden's files and, for each
// nested mount, a separate volume directory holding the golden's volume files.
// nativeLinks makes the mount points the symlinks a native run leaves behind;
// otherwise they are the empty directories an isolated run mounts onto.
func cleanFixture(t *testing.T, nativeLinks bool) (root string, volDirs []string, files []string) {
	t.Helper()
	g := cleanWorkspaceGolden(t)
	root = t.TempDir()
	for _, f := range g.RootFiles {
		writeFile(t, filepath.Join(root, filepath.FromSlash(f)), "x")
	}
	for _, m := range g.Mounts {
		vd := t.TempDir()
		volDirs = append(volDirs, vd)
		for _, f := range g.VolumeFiles {
			p := filepath.Join(vd, filepath.FromSlash(f))
			writeFile(t, p, "x")
			files = append(files, p)
		}
		mp := filepath.Join(root, filepath.FromSlash(m.MountPath))
		require.NoError(t, os.MkdirAll(filepath.Dir(mp), 0o755))
		if nativeLinks {
			if err := os.Symlink(vd, mp); err != nil {
				t.Skipf("cannot create symlinks here: %v", err)
			}
		} else {
			require.NoError(t, os.MkdirAll(mp, 0o755))
		}
	}
	return root, volDirs, files
}

// The host agent wipes a root volume before it mounts anything, so a nested
// volume is a link or an empty mount point at that moment and its bytes are
// never in reach. The Kubernetes agent has to be told to skip them; this pins
// the host side of the same promise.
func TestPrepareWorkspace_CleanWorkspaceLeavesNestedVolumesAlone(t *testing.T) {
	g := cleanWorkspaceGolden(t)
	for name, native := range map[string]bool{"native": true, "isolated": false} {
		t.Run(name, func(t *testing.T) {
			root, _, volFiles := cleanFixture(t, native)
			require.NoError(t, prepareWorkspace(context.Background(), root, name, testVolume(t), true, noRuntime, ""))

			for _, f := range g.RootFiles {
				assert.NoFileExists(t, filepath.Join(root, filepath.FromSlash(f)))
			}
			entries, err := os.ReadDir(root)
			require.NoError(t, err)
			assert.Empty(t, entries, "the root volume is emptied, links and mount points included")
			for _, f := range volFiles {
				assert.FileExists(t, f, "the wipe reached a nested volume")
			}
		})
	}
}

// When the plain removal fails and a root cleanup container does the work, it
// mounts only the root volume's directory, so it cannot reach another volume.
func TestPrepareWorkspace_CleanWorkspaceFallbackMountsOnlyTheRootVolume(t *testing.T) {
	root, volDirs, volFiles := cleanFixture(t, false)

	cleaned := false
	rt := &cleanupRT{dir: root, onExec: func() { cleaned = true }}
	failRemoval(t, func(string) bool { return !cleaned })
	require.NoError(t, prepareWorkspace(context.Background(), root, "isolated", testVolume(t), true,
		func() (crt.ContainerRuntime, error) { return rt, nil }, ""))

	require.Len(t, rt.created, 1, "the cleanup container must have run")
	for _, m := range rt.created[0].Mounts {
		assert.Equal(t, root, m.HostPath, "the cleanup container mounts the root volume's directory and nothing else")
		for _, vd := range volDirs {
			assert.NotEqual(t, vd, m.HostPath)
		}
	}
	for _, f := range volFiles {
		assert.FileExists(t, f)
	}
}
