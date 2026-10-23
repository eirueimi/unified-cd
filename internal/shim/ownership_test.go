package shim

import (
	"bytes"
	"errors"
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
	chowns []string
	chmods []string
}

func newFakeFS() *fakeFS { return &fakeFS{nodes: map[string]*fileInfo{}} }

func (f *fakeFS) add(p string, mode fs.FileMode, uid, gid int) {
	f.nodes[p] = &fileInfo{Mode: mode, UID: uid, GID: gid}
}

func (f *fakeFS) ops() fsOps {
	return fsOps{
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
	if err := fsGroup(f.ops(), PolicyAlways, 2000, []string{"/w"}, &out, &warn); err != nil {
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
	f.add("/w/link", linkM, 0, 0) // a symlink has no children in the fake either way
	f.add("/elsewhere", dirM, 0, 7)
	if err := fsGroup(f.ops(), PolicyAlways, 2000, []string{"/w"}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if f.nodes["/elsewhere"].GID != 7 {
		t.Error("walk escaped through a symlink")
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
	if err := fsGroup(f.ops(), PolicyOnRootMismatch, 2000, []string{"/w"}, &out, &bytes.Buffer{}); err != nil {
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
		if err := fsGroup(f.ops(), PolicyOnRootMismatch, 2000, []string{"/w"}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if f.nodes["/w/file"].GID != 2000 {
			t.Errorf("%s: mismatching root was not walked", name)
		}
	}

	// Always walks even when the root matches.
	f = setup(2000, match)
	if err := fsGroup(f.ops(), PolicyAlways, 2000, []string{"/w"}, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
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
	if err := fsGroup(ops, PolicyAlways, 2000, []string{"/w"}, &bytes.Buffer{}, &warn); err != nil {
		t.Fatalf("lchown failure must not abort (kubelet parity): %v", err)
	}
	if !strings.Contains(filepath.ToSlash(warn.String()), "lchown /w/a") {
		t.Errorf("warn = %q", warn.String())
	}
}

func TestFsGroup_RejectsBadArgs(t *testing.T) {
	f := newFakeFS()
	if err := fsGroup(f.ops(), "Sometimes", 1, []string{"/w"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Error("unknown policy accepted")
	}
	if err := fsGroup(f.ops(), PolicyAlways, 1, []string{"/missing"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
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
