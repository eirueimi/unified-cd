package shim

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"time"
)

// Ownership helpers behind `ucd-sh fsgroup` and `ucd-sh chown-as`. The shim
// runs them inside the claim pod's pause container (root in the runtime's
// user namespace) so a host-run claim gets the same fsGroup semantics
// Kubernetes gives a pod. See
// docs/superpowers/specs/2026-10-23-host-security-context-design.md, section 6.
//
// The walk is a port of the kubelet volume ownership code, read at Kubernetes
// v1.35.1 (the kindest/node version of the Docker Desktop cluster; CI's
// helm/kind-action pins no node image of its own),
// pkg/volume/volume_linux.go:
//
//	rwMask   = os.FileMode(0660)
//	roMask   = os.FileMode(0440)   // read-only volumes only; not used here
//	execMask = os.FileMode(0110)
//
//	changeFilePermission(path, fsGroup, readonly, info):
//	    Lchown(path, -1, fsGroup)          // errors are logged, not returned
//	    if info is a symlink: return       // never chmod a symlink
//	    mask := rwMask                     // 0660
//	    if info.IsDir(): mask |= ModeSetgid | execMask   // 02000|0110
//	    Chmod(path, info.Mode() | mask)    // errors are logged, not returned
//
//	requiresPermissionChange(root, fsGroup, readonly)  // OnRootMismatch only:
//	    true when stat fails, root is not a dir, root gid != fsGroup,
//	    (0660|0110) & perm != (0660|0110), or setgid is not set.
//
//	walkDeep: Lstat root, depth-first, children before their parent (the root
//	    is visited last), os.Lstat per entry, symlinks are never followed; a
//	    Lstat or ReadDir error aborts the walk.
//
// Like kubelet, a failed lchown or chmod is reported as a warning and the walk
// continues; the claim-start probe (spec 7.1) is what proves the mechanism
// works at all.

// FsGroupPolicy mirrors the pod fsGroupChangePolicy.
type FsGroupPolicy string

const (
	PolicyAlways         FsGroupPolicy = "Always"
	PolicyOnRootMismatch FsGroupPolicy = "OnRootMismatch"
)

const (
	rwMask   = fs.FileMode(0o660)
	execMask = fs.FileMode(0o110)
)

// fileInfo is the part of an lstat result the walkers need. Mode carries the
// type bits (ModeDir, ModeSymlink) and ModeSetgid alongside the permissions.
type fileInfo struct {
	Mode fs.FileMode
	UID  int
	GID  int
}

func (i fileInfo) isDir() bool     { return i.Mode&fs.ModeDir != 0 }
func (i fileInfo) isSymlink() bool { return i.Mode&fs.ModeSymlink != 0 }

// fsOps is the filesystem surface, injectable so the walkers are testable
// without root or a Linux host.
type fsOps struct {
	Lstat   func(path string) (fileInfo, error)
	Lchown  func(path string, uid, gid int) error
	Chmod   func(path string, mode fs.FileMode) error
	ReadDir func(path string) ([]string, error) // entry names, any order
}

// requiresFsGroupChange reports whether OnRootMismatch must walk.
func requiresFsGroupChange(ops fsOps, root string, gid int) bool {
	info, err := ops.Lstat(root)
	if err != nil || info.GID != gid || !info.isDir() {
		return true
	}
	want := rwMask | execMask
	return want&info.Mode.Perm() != want || info.Mode&fs.ModeSetgid == 0
}

func fsGroupChange(ops fsOps, path string, gid int, info fileInfo, warn io.Writer) {
	if err := ops.Lchown(path, -1, gid); err != nil {
		fmt.Fprintf(warn, "[ucd-sh] fsgroup: lchown %s: %v\n", path, err)
	}
	if info.isSymlink() {
		return
	}
	mask := rwMask
	if info.isDir() {
		mask |= fs.ModeSetgid | execMask
	}
	if err := ops.Chmod(path, info.Mode|mask); err != nil {
		fmt.Fprintf(warn, "[ucd-sh] fsgroup: chmod %s: %v\n", path, err)
	}
}

// fsGroupWalk applies fsGroupChange to everything under path (children first,
// path last) and returns the number of entries visited.
func fsGroupWalk(ops fsOps, path string, info fileInfo, gid int, warn io.Writer) (int, error) {
	n := 0
	if info.isDir() {
		names, err := ops.ReadDir(path)
		if err != nil {
			return n, err
		}
		for _, name := range names {
			child := filepath.Join(path, name)
			ci, err := ops.Lstat(child)
			if err != nil {
				return n, err
			}
			cn, err := fsGroupWalk(ops, child, ci, gid, warn)
			n += cn
			if err != nil {
				return n, err
			}
		}
	}
	fsGroupChange(ops, path, gid, info, warn)
	return n + 1, nil
}

// fsGroup runs the kubelet walk over each root, writing one summary line per
// root to out and per-entry failures to warn.
func fsGroup(ops fsOps, policy FsGroupPolicy, gid int, roots []string, out, warn io.Writer) error {
	if gid < 0 {
		return fmt.Errorf("invalid gid %d", gid)
	}
	if policy != PolicyAlways && policy != PolicyOnRootMismatch {
		return fmt.Errorf("unknown policy %q (want Always or OnRootMismatch)", policy)
	}
	for _, root := range roots {
		start := time.Now()
		if policy == PolicyOnRootMismatch && !requiresFsGroupChange(ops, root, gid) {
			fmt.Fprintf(out, "fsgroup %s: 0 entries, skipped (root already matches), %s\n", root, time.Since(start).Round(time.Millisecond))
			continue
		}
		info, err := ops.Lstat(root)
		if err != nil {
			return fmt.Errorf("%s: %w", root, err)
		}
		n, err := fsGroupWalk(ops, root, info, gid, warn)
		if err != nil {
			return fmt.Errorf("%s: %w", root, err)
		}
		fmt.Fprintf(out, "fsgroup %s: %d entries, %s\n", root, n, time.Since(start).Round(time.Millisecond))
	}
	return nil
}

// chownAs makes each path and its descendants look as if a process with euid
// uid and egid gid had created them: owner uid; group is the parent
// directory group when the parent is setgid, otherwise gid; a directory
// under a setgid parent gains setgid. Symlinks are lchowned only, never
// chmodded and never followed. Unlike fsGroup, any failure is returned: this
// repairs files the platform wrote itself and must not report false success.
func chownAs(ops fsOps, uid, gid int, paths []string) error {
	if uid < 0 || gid < 0 {
		return fmt.Errorf("invalid owner %d:%d", uid, gid)
	}
	for _, p := range paths {
		parentGID, parentSetgid := gid, false
		if parent, err := ops.Lstat(filepath.Dir(p)); err == nil && parent.isDir() && parent.Mode&fs.ModeSetgid != 0 {
			parentGID, parentSetgid = parent.GID, true
		}
		info, err := ops.Lstat(p)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if err := chownAsWalk(ops, uid, gid, p, info, parentGID, parentSetgid); err != nil {
			return err
		}
	}
	return nil
}

func chownAsWalk(ops fsOps, uid, gid int, path string, info fileInfo, parentGID int, parentSetgid bool) error {
	g := gid
	if parentSetgid {
		g = parentGID
	}
	if err := ops.Lchown(path, uid, g); err != nil {
		return fmt.Errorf("lchown %s: %w", path, err)
	}
	if info.isSymlink() || !info.isDir() {
		return nil
	}
	setgid := info.Mode&fs.ModeSetgid != 0
	if parentSetgid && !setgid {
		if err := ops.Chmod(path, info.Mode|fs.ModeSetgid); err != nil {
			return fmt.Errorf("chmod %s: %w", path, err)
		}
		setgid = true
	}
	names, err := ops.ReadDir(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for _, name := range names {
		child := filepath.Join(path, name)
		ci, err := ops.Lstat(child)
		if err != nil {
			return fmt.Errorf("%s: %w", child, err)
		}
		if err := chownAsWalk(ops, uid, gid, child, ci, g, setgid); err != nil {
			return err
		}
	}
	return nil
}

var errUnsupported = errors.New("not supported on this platform (the shim only runs in Linux containers)")

// FsGroup implements `ucd-sh fsgroup`.
func FsGroup(policy FsGroupPolicy, gid int, roots []string, out, warn io.Writer) error {
	ops, err := realOps()
	if err != nil {
		return err
	}
	return fsGroup(ops, policy, gid, roots, out, warn)
}

// ChownAs implements `ucd-sh chown-as`.
func ChownAs(uid, gid int, paths []string) error {
	ops, err := realOps()
	if err != nil {
		return err
	}
	return chownAs(ops, uid, gid, paths)
}
