package k8sagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/shim"
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

// scriptRunner runs a generated script with dir as its working directory and
// returns its exit code and combined output.
type scriptRunner struct {
	name string
	run  func(t *testing.T, script, dir string) (int, string)
}

// scriptRunners are the shells the clearing script is held to. The pod runs it
// through ucd-sh, whose parser and runner are internal/shim, not a real sh. The
// two disagree about globbing a quoted word that contains a glob metacharacter
// (a real sh takes the quoting literally; mvdan's expander escapes the word and
// then stats the escaped path), and only a run through the interpreter the pod
// uses can catch that class of defect.
func scriptRunners(t *testing.T) []scriptRunner {
	t.Helper()
	var rs []scriptRunner
	if sh, err := exec.LookPath("sh"); err == nil {
		rs = append(rs, scriptRunner{"sh", func(t *testing.T, script, dir string) (int, string) {
			cmd := exec.Command(sh, "-c", script)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if ee, ok := err.(*exec.ExitError); ok {
				return ee.ExitCode(), string(out)
			}
			require.NoError(t, err, string(out))
			return 0, string(out)
		}})
	}
	// The generated script assumes a Linux pod: mvdan's globber joins with
	// filepath.Separator, so on Windows no POSIX case pattern would match.
	if runtime.GOOS != "windows" {
		rs = append(rs, scriptRunner{"ucd-sh", func(t *testing.T, script, dir string) (int, string) {
			var out bytes.Buffer
			code, err := shim.Run(context.Background(), script, strings.NewReader(""), &out, &out, nil, dir)
			require.NoError(t, err, out.String())
			return code, out.String()
		}})
	}
	if len(rs) == 0 {
		t.Skip("no shell available to run the clearing script")
	}
	return rs
}

// The script the Kubernetes agent runs, run for real through each shell it can
// meet: every file of the root volume goes, dot-prefixed ones and the other
// children of a directory on the path to a mount point included, and a volume
// mounted below the root keeps every byte. The host agent is held to the same
// golden scenario (TestPrepareWorkspace_CleanWorkspaceLeavesNestedVolumesAlone).
func TestClearWorkspaceScript_LeavesNestedVolumesAloneAndEmptiesTheRest(t *testing.T) {
	g := cleanWorkspaceScenario(t)
	for _, r := range scriptRunners(t) {
		t.Run(r.name, func(t *testing.T) {
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

			code, out := r.run(t, clearWorkspaceScript(root, vols), root)
			require.Zero(t, code, out)

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
		})
	}
}

// A directory on the way to a mount point, or the mount point itself, may be
// named with glob metacharacters (the mountPath validation permits [, * and ?),
// and the other children of such a directory must still be removed. Through
// ucd-sh a quoted word is pattern-escaped rather than literal, so a script that
// globs "$base"/* with a job-authored base enumerates nothing, leaves the
// siblings behind and still exits 0.
func TestClearWorkspaceScript_GlobCharactersInMountPaths(t *testing.T) {
	cases := []struct {
		name   string
		mounts []string // mount paths, relative to the workspace
		wipe   []string // files that must be gone
		keep   []string // files that must survive
		noWin  bool     // the name cannot exist on a Windows filesystem
	}{
		{
			name:   "bracket in an ancestor",
			mounts: []string{".ucd-[cache]/go"},
			wipe:   []string{"top.txt", ".ucd-[cache]/sibling.txt", ".ucd-[cache]/sub/deep.txt"},
			keep:   []string{".ucd-[cache]/go/obj.o"},
		},
		{
			name:   "bracket in the mount point",
			mounts: []string{"cache[1]/go"},
			wipe:   []string{"top.txt", "cache[1]/sibling.txt"},
			keep:   []string{"cache[1]/go/obj.o"},
		},
		{
			name:   "star and question mark",
			mounts: []string{"a*b/c?d/vol"},
			wipe:   []string{"top.txt", "a*b/sibling.txt", "a*b/c?d/sibling.txt", "a*b/other/x.txt"},
			keep:   []string{"a*b/c?d/vol/obj.o"},
			noWin:  true,
		},
		{
			name:   "metacharacter in a mount point that holds another",
			mounts: []string{"[v]", "[v]/inner"},
			wipe:   []string{"top.txt"},
			keep:   []string{"[v]/own.txt", "[v]/inner/obj.o"},
		},
		{
			name:   "space and single quote",
			mounts: []string{"it's here/go"},
			wipe:   []string{"top.txt", "it's here/sibling.txt"},
			keep:   []string{"it's here/go/obj.o"},
		},
	}
	for _, r := range scriptRunners(t) {
		for _, c := range cases {
			t.Run(r.name+"/"+c.name, func(t *testing.T) {
				if c.noWin && runtime.GOOS == "windows" {
					t.Skip("* and ? cannot appear in a Windows file name")
				}
				root := filepath.ToSlash(t.TempDir())
				var vols []agentlib.MountedVolume
				for i, m := range c.mounts {
					vols = append(vols, agentlib.MountedVolume{Volume: fmt.Sprintf("v%d", i), MountPath: m, Location: root + "/" + m})
				}
				for _, f := range append(append([]string{}, c.wipe...), c.keep...) {
					putFile(t, filepath.FromSlash(root+"/"+f))
				}

				code, out := r.run(t, clearWorkspaceScript(root, vols), root)
				require.Zero(t, code, out)

				for _, f := range c.wipe {
					assert.NoFileExists(t, filepath.FromSlash(root+"/"+f), "a workspace file survived the wipe, and the script exited 0 (%s)", out)
				}
				for _, f := range c.keep {
					assert.FileExists(t, filepath.FromSlash(root+"/"+f), "the wipe reached a nested volume")
				}
			})
		}
	}
}

// A directory the wipe cannot enter is a workspace it did not empty: the script
// has to fail rather than skip it.
func TestClearWorkspaceScript_ReportsADirectoryItCannotEnter(t *testing.T) {
	missing := filepath.ToSlash(filepath.Join(t.TempDir(), "gone"))
	vols := []agentlib.MountedVolume{{Volume: "v", MountPath: "keep/v", Location: missing + "/keep/v"}}
	for _, r := range scriptRunners(t) {
		t.Run(r.name, func(t *testing.T) {
			code, out := r.run(t, clearWorkspaceScript(missing, vols), t.TempDir())
			assert.NotZero(t, code, "a root that cannot be entered must fail the script: %s", out)
		})
	}
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
	assert.Contains(t, s, `'/workspace/.ucd-cache'|'/workspace/a'|'/workspace/a/b') ucd_wipe "$1/$e"; cd "$1" || { ucd_rc=1; return; } ;;`, "the path to a mount point is descended into, mount points excluded from it, and the working directory is restored")
	assert.Contains(t, s, "  cd \"$1\" || { ucd_rc=1; return; }\n  for e in * .[!.]* ..?*; do\n", "the glob patterns are literals, applied after a cd that must not fail silently")
	assert.Equal(t, 1, strings.Count(s, "rm -rf"), "a single removal, of whatever is neither a mount point nor on the way to one")
	assert.NotContains(t, s, "rm -rf '/workspace", "no removal names a path directly")
	assert.NotContains(t, s, `'/workspace'/*`, "the root is never globbed into a removal")
	assert.NotContains(t, s, `"$1"/`, "no glob is built from the directory name")
	assert.True(t, strings.HasSuffix(s, "ucd_wipe '/workspace'\nexit $ucd_rc\n"))

	// Mount paths are quoted for the case patterns, where a glob character or a
	// quote in one is literal. They are never part of a glob: that is a quoted
	// word under ucd-sh, which escapes it and then cannot find it (see
	// TestClearWorkspaceScript_GlobCharactersInMountPaths).
	q := clearWorkspaceScript("/work space", []agentlib.MountedVolume{{Volume: "v", Location: "/work space/it's*/x"}})
	assert.Contains(t, q, `'/work space/it'\''s*/x') ;;`)
	assert.Contains(t, q, `ucd_wipe '/work space'`)
}
