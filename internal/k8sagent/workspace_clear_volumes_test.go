package k8sagent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
)

// cleanScenario is the "cleanWorkspace" entry of the host agent's
// testdata/volume_exclusion_golden.json: the statement of what wiping a root
// volume does to the volumes mounted below it that both agents are held to.
type cleanScenario struct {
	Mounts []struct {
		Name      string `json:"name"`
		MountPath string `json:"mountPath"`
	} `json:"mounts"`
	RootFiles   []string `json:"rootFiles"`
	VolumeFiles []string `json:"volumeFiles"`
}

func cleanWorkspaceScenario(t *testing.T) cleanScenario {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "agent", "testdata", "volume_exclusion_golden.json"))
	require.NoError(t, err)
	var g struct {
		Clean cleanScenario `json:"cleanWorkspace"`
	}
	require.NoError(t, json.Unmarshal(b, &g))
	require.NotEmpty(t, g.Clean.Mounts, "the golden file lost its cleanWorkspace scenario")
	return g.Clean
}

func putFile(t *testing.T, p string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
}

// The script the Kubernetes agent runs, run for real: every file of the root
// volume goes, dot-prefixed ones and the other children of a directory on the
// path to a mount point included, and a volume mounted below the root keeps
// every byte. The host agent is held to the same golden scenario
// (TestPrepareWorkspace_CleanWorkspaceLeavesNestedVolumesAlone).
func TestClearWorkspaceScript_LeavesNestedVolumesAloneAndEmptiesTheRest(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH to run the clearing script")
	}
	g := cleanWorkspaceScenario(t)

	root := filepath.ToSlash(t.TempDir())
	var vols []agentlib.MountedVolume
	for _, m := range g.Mounts {
		vols = append(vols, agentlib.MountedVolume{Volume: m.Name, MountPath: m.MountPath, Location: root + "/" + m.MountPath})
		// A mounted volume: its files sit at the mount point inside the workspace.
		for _, f := range g.VolumeFiles {
			putFile(t, filepath.FromSlash(root+"/"+m.MountPath+"/"+f))
		}
	}
	for _, f := range g.RootFiles {
		putFile(t, filepath.FromSlash(root+"/"+f))
	}

	out, err := exec.Command(sh, "-c", clearWorkspaceScript(root, vols)).CombinedOutput()
	require.NoError(t, err, string(out))

	for _, f := range g.RootFiles {
		assert.NoFileExists(t, filepath.FromSlash(root+"/"+f), "a workspace file survived the wipe")
	}
	for _, m := range g.Mounts {
		for _, f := range g.VolumeFiles {
			assert.FileExists(t, filepath.FromSlash(root+"/"+m.MountPath+"/"+f), "the wipe reached volume %q", m.Name)
		}
	}
	entries, err := os.ReadDir(filepath.FromSlash(root))
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{".ucd-cache", "tools"}, names, "only the path to the mount points is left")
}

// The wipe must still fail the run when something cannot be removed: a
// workspace that was not emptied is not one to run in. The removal is made to
// fail with an rm that exits 1, placed first on PATH.
func TestClearWorkspaceScript_ReportsAFailedRemoval(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH to run the clearing script")
	}
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "rm"), []byte("#!/bin/sh\nexit 1\n"), 0o755))

	root := filepath.ToSlash(t.TempDir())
	putFile(t, filepath.FromSlash(root+"/doomed.txt"))
	putFile(t, filepath.FromSlash(root+"/keep/v/f"))
	vols := []agentlib.MountedVolume{{Volume: "v", MountPath: "keep/v", Location: root + "/keep/v"}}

	for name, script := range map[string]string{"with a nested volume": clearWorkspaceScript(root, vols), "plain": clearWorkspaceScript(root, nil)} {
		cmd := exec.Command(sh, "-c", script)
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		require.Error(t, err, "%s: a removal that fails must make the script fail: %s", name, out)
	}
}

// What the script is made of, independent of any shell: a nested mount point is
// only ever named as something to skip, the directories on the way to it are
// descended into, and a plain workspace keeps the original one-liner.
func TestClearWorkspaceScript_Shape(t *testing.T) {
	plain := clearWorkspaceScript("/workspace", nil)
	assert.Equal(t, `rm -rf '/workspace'/* '/workspace'/.[!.]* '/workspace'/..?*`, plain)

	// A volume at the root, or outside it, is not a nested mount point.
	assert.Equal(t, plain, clearWorkspaceScript("/workspace", []agentlib.MountedVolume{
		{Volume: "outside", Location: "/root/.cache"},
	}))

	s := clearWorkspaceScript("/workspace", []agentlib.MountedVolume{
		{Volume: "go", Location: "/workspace/.ucd-cache/go"},
		{Volume: "tools", Location: "/workspace/a/b/tools"},
		{Volume: "inner", Location: "/workspace/a/b/tools/inner"},
	})
	assert.Contains(t, s, `'/workspace/.ucd-cache/go'|'/workspace/a/b/tools'|'/workspace/a/b/tools/inner') ;;`, "mount points are skipped")
	assert.Contains(t, s, `'/workspace/.ucd-cache'|'/workspace/a'|'/workspace/a/b') ucd_wipe "$e" ;;`, "the path to a mount point is descended into, mount points excluded from it")
	assert.Equal(t, 1, strings.Count(s, "rm -rf"), "a single removal, of whatever is neither a mount point nor on the way to one")
	assert.NotContains(t, s, "rm -rf '/workspace", "no removal names a path directly")
	assert.NotContains(t, s, `'/workspace'/*`, "the root is never globbed into a removal")
	assert.True(t, strings.HasSuffix(s, "ucd_wipe '/workspace'\nexit $ucd_rc\n"))

	// Paths are quoted, so a glob character or a quote in one is literal.
	q := clearWorkspaceScript("/work space", []agentlib.MountedVolume{{Volume: "v", Location: "/work space/it's*/x"}})
	assert.Contains(t, q, `'/work space/it'\''s*/x') ;;`)
	assert.Contains(t, q, `ucd_wipe '/work space'`)
}
