package shim

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/stretchr/testify/assert"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fakeFS is an in-memory fsOps with recorded calls. Paths use forward
// slashes; the walkers build children with filepath.Join, so keys are
// normalised through filepath.ToSlash.
type fakeFS struct {
	nodes  map[string]*fileInfo
	mounts []string // mount points in effect
	chowns []string
	chmods []string
}

func newFakeFS() *fakeFS { return &fakeFS{nodes: map[string]*fileInfo{}} }

func (f *fakeFS) add(p string, mode fs.FileMode, uid, gid int) {
	f.nodes[p] = &fileInfo{Mode: mode, UID: uid, GID: gid}
}

func (f *fakeFS) ops() fsOps {
	return fsOps{
		MountPoints: func() ([]string, error) { return f.mounts, nil },
		Lstat: func(p string) (fileInfo, error) {
			n, ok := f.nodes[filepath.ToSlash(p)]
			if !ok {
				return fileInfo{}, os.ErrNotExist
			}
			return *n, nil
		},
		Lchown: func(p string, uid, gid int) error {
			p = filepath.ToSlash(p)
			n := f.nodes[p]
			f.chowns = append(f.chowns, p)
			if uid >= 0 {
				n.UID = uid
			}
			if gid >= 0 {
				n.GID = gid
			}
			return nil
		},
		Chmod: func(p string, m fs.FileMode) error {
			p = filepath.ToSlash(p)
			n := f.nodes[p]
			f.chmods = append(f.chmods, p)
			// Keep type bits, replace permission + setgid, like chmod(2).
			keep := n.Mode &^ (fs.ModePerm | fs.ModeSetgid)
			n.Mode = keep | (m & (fs.ModePerm | fs.ModeSetgid))
			return nil
		},
		ReadDir: func(p string) ([]string, error) {
			p = filepath.ToSlash(p)
			var names []string
			for k := range f.nodes {
				if k != p && path.Dir(k) == p {
					names = append(names, path.Base(k))
				}
			}
			sort.Strings(names)
			return names, nil
		},
	}
}

const (
	dirM  = fs.ModeDir | 0o755
	fileM = fs.FileMode(0o644)
	linkM = fs.ModeSymlink | 0o777
)

func TestFsGroup_ModesAndSymlinks(t *testing.T) {
	f := newFakeFS()
	f.add("/w", dirM, 0, 0)
	f.add("/w/file", fileM, 1000, 0)
	f.add("/w/sub", dirM, 1000, 0)
	f.add("/w/sub/inner", 0o600, 1000, 0)
	f.add("/w/link", linkM, 1000, 0)

	var out, warn bytes.Buffer
	if err := fsGroup(f.ops(), PolicyAlways, 2000, nil, []string{"/w"}, &out, &warn); err != nil {
		t.Fatal(err)
	}
	if got := f.nodes["/w/file"].Mode; got != 0o664 {
		t.Errorf("file mode = %o, want 0664", got)
	}
	if got := f.nodes["/w/sub/inner"].Mode; got != 0o660 {
		t.Errorf("inner mode = %o, want 0660", got)
	}
	wantDir := fs.ModeDir | fs.ModeSetgid | 0o775 | 0o770
	for _, d := range []string{"/w", "/w/sub"} {
		n := f.nodes[d]
		if n.Mode != wantDir {
			t.Errorf("%s mode = %v, want %v", d, n.Mode, wantDir)
		}
	}
	for p, n := range f.nodes {
		if n.GID != 2000 {
			t.Errorf("%s gid = %d, want 2000", p, n.GID)
		}
		if n.UID != 0 && n.UID != 1000 {
			t.Errorf("%s uid changed to %d", p, n.UID)
		}
	}
	// Symlink: lchowned, never chmodded.
	if !contains(f.chowns, "/w/link") {
		t.Error("symlink was not lchowned")
	}
	if contains(f.chmods, "/w/link") {
		t.Error("symlink was chmodded")
	}
	if n := f.nodes["/w/link"].Mode; n != linkM {
		t.Errorf("symlink mode changed to %v", n)
	}
	if !strings.Contains(out.String(), "fsgroup /w: 5 entries") {
		t.Errorf("summary = %q, want 5 entries", out.String())
	}
	if warn.Len() != 0 {
		t.Errorf("unexpected warnings: %s", warn.String())
	}
	// Root is visited last (kubelet walk order).
	if last := f.chowns[len(f.chowns)-1]; last != "/w" {
		t.Errorf("last visited = %s, want /w", last)
	}
}

func TestFsGroup_DoesNotFollowSymlinkToDir(t *testing.T) {
	f := newFakeFS()
	f.add("/w", dirM, 0, 0)
	f.add("/w/link", linkM, 0, 0)
	f.add("/elsewhere", dirM, 0, 7)
	f.add("/elsewhere/child", fileM, 0, 7)
	// A walker that followed the link would see the target's children under it.
	ops := f.ops()
	readDir, lstat := ops.ReadDir, ops.Lstat
	ops.ReadDir = func(p string) ([]string, error) {
		if filepath.ToSlash(p) == "/w/link" {
			return readDir("/elsewhere")
		}
		return readDir(p)
	}
	ops.Lstat = func(p string) (fileInfo, error) {
		if filepath.ToSlash(p) == "/w/link/child" {
			return *f.nodes["/elsewhere/child"], nil
		}
		return lstat(p)
	}
	if err := fsGroup(ops, PolicyAlways, 2000, nil, []string{"/w"}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if f.nodes["/elsewhere"].GID != 7 || f.nodes["/elsewhere/child"].GID != 7 {
		t.Error("walk escaped through a symlink")
	}
	if contains(f.chowns, "/w/link/child") {
		t.Error("walk descended into a symlink")
	}
}

func TestFsGroup_DoesNotCrossIntoANestedMountPoint(t *testing.T) {
	f := newFakeFS()
	f.add("/w", dirM, 0, 0)
	f.add("/w/file", fileM, 0, 0)
	f.add("/w/vol", dirM, 0, 5) // a volume bind-mounted under the workspace
	f.add("/w/vol/data", fileM, 0, 5)
	f.mounts = []string{"/", "/w", "/w/vol"}
	var out bytes.Buffer
	if err := fsGroup(f.ops(), PolicyAlways, 2000, nil, []string{"/w"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/w/vol", "/w/vol/data"} {
		if f.nodes[p].GID != 5 || contains(f.chowns, p) || contains(f.chmods, p) {
			t.Errorf("%s belongs to another mount and was touched", p)
		}
	}
	if f.nodes["/w/file"].GID != 2000 {
		t.Error("the workspace's own file was not walked")
	}
	if !strings.Contains(out.String(), "fsgroup /w: 2 entries") {
		t.Errorf("summary = %q, want 2 entries (file and root)", out.String())
	}
}

func TestFsGroup_ExcludesOtherRootsOnTheSameDevice(t *testing.T) {
	// A volume that shares the workspace's device is invisible to st_dev, so
	// the agent names the other roots explicitly.
	f := newFakeFS()
	f.add("/w", dirM, 0, 0)
	f.add("/w/file", fileM, 0, 0)
	f.add("/w/vol", dirM, 0, 5)
	f.add("/w/vol/data", fileM, 0, 5)
	if err := fsGroup(f.ops(), PolicyAlways, 2000, []string{"/w/vol/"}, []string{"/w", "/w/vol"}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if f.nodes["/w/vol/data"].GID != 2000 {
		t.Error("the volume root was not walked as its own root")
	}
	n := 0
	for _, c := range f.chowns {
		if c == "/w/vol/data" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("volume content walked %d times, want exactly once (its own root pass)", n)
	}
}

func TestFsGroup_EachRootHasItsOwnOnRootMismatchDecision(t *testing.T) {
	f := newFakeFS()
	match := fs.ModeDir | fs.ModeSetgid | 0o770
	f.add("/w", dirM, 0, 0)         // fresh workspace: mismatch
	f.add("/w/vol", match, 0, 2000) // volume already done: match
	f.add("/w/vol/data", fileM, 0, 0)
	f.mounts = []string{"/w", "/w/vol"}
	var out bytes.Buffer
	if err := fsGroup(f.ops(), PolicyOnRootMismatch, 2000, []string{"/w/vol"}, []string{"/w", "/w/vol"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if contains(f.chowns, "/w/vol/data") {
		t.Error("a matching volume root was walked because the workspace root mismatched")
	}
	if !strings.Contains(out.String(), "fsgroup /w/vol: 0 entries, skipped") {
		t.Errorf("summary = %q", out.String())
	}
}

func TestFsGroup_VanishedEntriesAreSkipped(t *testing.T) {
	f := newFakeFS()
	f.add("/w", dirM, 0, 0)
	f.add("/w/gone", fileM, 0, 0)
	f.add("/w/gonedir", dirM, 0, 0)
	f.add("/w/keep", fileM, 0, 0)
	ops := f.ops()
	lstat, readDir := ops.Lstat, ops.ReadDir
	ops.Lstat = func(p string) (fileInfo, error) {
		if filepath.ToSlash(p) == "/w/gone" {
			return fileInfo{}, &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist}
		}
		return lstat(p)
	}
	ops.ReadDir = func(p string) ([]string, error) {
		if filepath.ToSlash(p) == "/w/gonedir" {
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
		}
		return readDir(p)
	}
	var out, warn bytes.Buffer
	if err := fsGroup(ops, PolicyAlways, 2000, nil, []string{"/w"}, &out, &warn); err != nil {
		t.Fatalf("a concurrently deleted entry must not fail the claim: %v", err)
	}
	if f.nodes["/w/keep"].GID != 2000 {
		t.Error("walk stopped at the vanished entry")
	}
	if warn.Len() != 0 {
		t.Errorf("a vanished entry is not a failure, warn = %q", warn.String())
	}
}

func TestFsGroup_CapsWarningsAndCountsFailures(t *testing.T) {
	f := newFakeFS()
	f.add("/w", dirM, 0, 0)
	for i := 0; i < 50; i++ {
		f.add(fmt.Sprintf("/w/f%02d", i), fileM, 0, 0)
	}
	ops := f.ops()
	ops.Lchown = func(p string, uid, gid int) error { return errors.New("denied") }
	var out, warn bytes.Buffer
	if err := fsGroup(ops, PolicyAlways, 2000, nil, []string{"/w"}, &out, &warn); err != nil {
		t.Fatal(err)
	}
	lines := strings.Count(warn.String(), "\n")
	if lines > maxFsGroupWarnings+1 {
		t.Errorf("%d warning lines, want at most %d plus one suppression notice", lines, maxFsGroupWarnings)
	}
	if !strings.Contains(warn.String(), "suppressed") {
		t.Errorf("no suppression notice: %q", warn.String())
	}
	if !strings.Contains(out.String(), "51 failed") {
		t.Errorf("summary = %q, want the failure count (51: 50 files and the root)", out.String())
	}
}

func TestFsGroup_OnRootMismatch(t *testing.T) {
	setup := func(rootGID int, rootMode fs.FileMode) *fakeFS {
		f := newFakeFS()
		f.add("/w", rootMode, 0, rootGID)
		f.add("/w/file", fileM, 0, 0)
		return f
	}
	match := fs.ModeDir | fs.ModeSetgid | 0o770

	f := setup(2000, match)
	var out bytes.Buffer
	if err := fsGroup(f.ops(), PolicyOnRootMismatch, 2000, nil, []string{"/w"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(f.chowns) != 0 || len(f.chmods) != 0 {
		t.Errorf("matching root was walked: chowns=%v chmods=%v", f.chowns, f.chmods)
	}
	if !strings.Contains(out.String(), "skipped") {
		t.Errorf("summary = %q, want skipped", out.String())
	}

	for name, f := range map[string]*fakeFS{
		"wrong gid":    setup(1, match),
		"no setgid":    setup(2000, fs.ModeDir|0o770),
		"no group rwx": setup(2000, fs.ModeDir|fs.ModeSetgid|0o750),
	} {
		if err := fsGroup(f.ops(), PolicyOnRootMismatch, 2000, nil, []string{"/w"}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if f.nodes["/w/file"].GID != 2000 {
			t.Errorf("%s: mismatching root was not walked", name)
		}
	}

	// Always walks even when the root matches.
	f = setup(2000, match)
	if err := fsGroup(f.ops(), PolicyAlways, 2000, nil, []string{"/w"}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if f.nodes["/w/file"].Mode != 0o664 {
		t.Errorf("Always did not walk: file mode %o", f.nodes["/w/file"].Mode)
	}
}

func TestFsGroup_WarnsAndContinuesOnChownError(t *testing.T) {
	f := newFakeFS()
	f.add("/w", dirM, 0, 0)
	f.add("/w/a", fileM, 0, 0)
	ops := f.ops()
	ops.Lchown = func(p string, uid, gid int) error { return errors.New("denied") }
	var warn bytes.Buffer
	if err := fsGroup(ops, PolicyAlways, 2000, nil, []string{"/w"}, &bytes.Buffer{}, &warn); err != nil {
		t.Fatalf("lchown failure must not abort (kubelet parity): %v", err)
	}
	if !strings.Contains(filepath.ToSlash(warn.String()), "lchown /w/a") {
		t.Errorf("warn = %q", warn.String())
	}
}

func TestFsGroup_RejectsBadArgs(t *testing.T) {
	f := newFakeFS()
	if err := fsGroup(f.ops(), "Sometimes", 1, nil, []string{"/w"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Error("unknown policy accepted")
	}
	if err := fsGroup(f.ops(), PolicyAlways, 1, nil, []string{"/missing"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Error("missing root accepted")
	}
}

func TestChownAs_GroupFromSetgidParent(t *testing.T) {
	f := newFakeFS()
	f.add("/ws", fs.ModeDir|fs.ModeSetgid|0o770, 0, 2000)
	f.add("/ws/dl", dirM, 0, 0)
	f.add("/ws/dl/file", fileM, 0, 0)
	f.add("/ws/dl/sub", dirM, 0, 0)
	f.add("/ws/dl/sub/x", fileM, 0, 0)
	f.add("/ws/dl/link", linkM, 0, 0)
	if err := chownAs(f.ops(), 1000, 3000, []string{"/ws/dl"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/ws/dl", "/ws/dl/file", "/ws/dl/sub", "/ws/dl/sub/x", "/ws/dl/link"} {
		n := f.nodes[p]
		if n.UID != 1000 || n.GID != 2000 {
			t.Errorf("%s owner = %d:%d, want 1000:2000 (group from setgid parent)", p, n.UID, n.GID)
		}
	}
	for _, d := range []string{"/ws/dl", "/ws/dl/sub"} {
		if f.nodes[d].Mode&fs.ModeSetgid == 0 {
			t.Errorf("%s lost/never got setgid", d)
		}
	}
	if contains(f.chmods, "/ws/dl/link") || contains(f.chmods, "/ws/dl/file") {
		t.Errorf("chmod on symlink or file: %v", f.chmods)
	}
}

func TestChownAs_GroupFromGWhenParentNotSetgid(t *testing.T) {
	f := newFakeFS()
	f.add("/ws", dirM, 0, 2000)
	f.add("/ws/dl", dirM, 0, 0)
	f.add("/ws/dl/file", fileM, 0, 0)
	if err := chownAs(f.ops(), 1000, 3000, []string{"/ws/dl"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/ws/dl", "/ws/dl/file"} {
		if n := f.nodes[p]; n.UID != 1000 || n.GID != 3000 {
			t.Errorf("%s owner = %d:%d, want 1000:3000", p, n.UID, n.GID)
		}
	}
	if f.nodes["/ws/dl"].Mode&fs.ModeSetgid != 0 {
		t.Error("setgid added under a non-setgid parent")
	}
}

func TestChownAs_PropagatesError(t *testing.T) {
	f := newFakeFS()
	f.add("/ws", dirM, 0, 0)
	ops := f.ops()
	ops.Lchown = func(string, int, int) error { return errors.New("denied") }
	if err := chownAs(ops, 1, 1, []string{"/ws"}); err == nil {
		t.Error("lchown failure swallowed")
	}
	if err := chownAs(f.ops(), 1, 1, []string{"/nope"}); err == nil {
		t.Error("missing path accepted")
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// Design 9: runAsGroup 3000 differs from fsGroup 2000, and the agent passes
// extracted entries directly under the setgid workspace root. Each takes the
// root's group (fsGroup), not runAsGroup, and the root itself is untouched.
func TestChownAs_EntriesDirectlyUnderSetgidRootTakeFsGroup(t *testing.T) {
	f := newFakeFS()
	f.add("/ws", fs.ModeDir|fs.ModeSetgid|0o770, 0, 2000)
	f.add("/ws/a.txt", fileM, 0, 0)
	f.add("/ws/sub", dirM, 0, 0)
	f.add("/ws/sub/b.txt", fileM, 0, 0)
	if err := chownAs(f.ops(), 1000, 3000, []string{"/ws/a.txt", "/ws/sub"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/ws/a.txt", "/ws/sub", "/ws/sub/b.txt"} {
		if n := f.nodes[p]; n.UID != 1000 || n.GID != 2000 {
			t.Errorf("%s owner = %d:%d, want 1000:2000", p, n.UID, n.GID)
		}
	}
	if n := f.nodes["/ws"]; n.UID != 0 || n.GID != 2000 {
		t.Errorf("/ws owner = %d:%d, want unchanged 0:2000", n.UID, n.GID)
	}
}

func TestParseMountinfo(t *testing.T) {
	in := `22 1 0:21 / / rw - overlay overlay rw
35 22 8:1 /data /workspace/data rw,relatime - ext4 /dev/sda1 rw
36 22 8:1 /sp /with\040space rw - ext4 /dev/sda1 rw

bad line
`
	assert.Equal(t, []string{"/", "/workspace/data", "/with space"}, parseMountinfo(in))
}
