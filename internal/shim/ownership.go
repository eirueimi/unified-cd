package shim

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	posix "path"
	"path/filepath"
	"strconv"
	"strings"
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
//	    Lstat or ReadDir error aborts the walk. (Here an entry that vanished
//	    mid-walk is skipped instead, and the walk never enters a mount point:
//	    kubelet applies this per volume, and every volume is its own mount.)
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
	// MountPoints lists the mount points in effect (nil: none known). The
	// walk never enters one below its root: that is another volume.
	MountPoints func() ([]string, error)
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

// maxFsGroupWarnings caps the per-entry warnings of one fsgroup run: a volume
// the runtime cannot chown would otherwise produce a line per file, each of
// which the agent forwards to the run log before the job can start.
const maxFsGroupWarnings = 20

// warner writes the first maxFsGroupWarnings warnings and counts them all.
type warner struct {
	w        io.Writer
	failures int // per-root count, reset by the caller
	total    int
}

func (w *warner) warnf(format string, args ...any) {
	w.failures++
	w.total++
	if w.total <= maxFsGroupWarnings {
		fmt.Fprintf(w.w, format, args...)
	}
}

func (w *warner) flushSuppressed() {
	if w.total > maxFsGroupWarnings {
		fmt.Fprintf(w.w, "[ucd-sh] fsgroup: %d further warnings suppressed\n", w.total-maxFsGroupWarnings)
	}
}

func fsGroupChange(ops fsOps, path string, gid int, info fileInfo, warn *warner) {
	if err := ops.Lchown(path, -1, gid); err != nil && !errors.Is(err, fs.ErrNotExist) {
		warn.warnf("[ucd-sh] fsgroup: lchown %s: %v\n", path, err)
	}
	if info.isSymlink() {
		return
	}
	mask := rwMask
	if info.isDir() {
		mask |= fs.ModeSetgid | execMask
	}
	if err := ops.Chmod(path, info.Mode|mask); err != nil && !errors.Is(err, fs.ErrNotExist) {
		warn.warnf("[ucd-sh] fsgroup: chmod %s: %v\n", path, err)
	}
}

// walkScope is what one root's walk must not enter: any mount point below the
// root (kubelet never crosses into another volume, because each volume is its
// own mount) and the explicit exclusions (the other volume roots, named by the
// agent). Mount points come from the mount table, not st_dev: a device number
// is not constant within one filesystem (overlayfs, btrfs subvolumes and
// virtiofs all vary it), so comparing it would skip ordinary files.
type walkScope struct {
	excluded map[string]bool
}

// skips reports whether child belongs to some other volume.
func (s walkScope) skips(child string) bool {
	return s.excluded[child]
}

// fsGroupWalk applies fsGroupChange to everything under path (children first,
// path last) and returns the number of entries visited. Entries that vanish
// mid-walk (a concurrent run on a shared volume deleting files) are skipped.
func fsGroupWalk(ops fsOps, path string, info fileInfo, gid int, scope walkScope, warn *warner) (int, error) {
	n := 0
	if info.isDir() {
		names, err := ops.ReadDir(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return n, nil
			}
			return n, err
		}
		for _, name := range names {
			child := posix.Join(path, name)
			ci, err := ops.Lstat(child)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return n, err
			}
			if scope.skips(child) {
				continue
			}
			cn, err := fsGroupWalk(ops, child, ci, gid, scope, warn)
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
// root to out and per-entry failures to warnOut (the first maxFsGroupWarnings;
// the summary carries the full failure count). Each root gets its own
// OnRootMismatch decision and its own walk, which never enters another mount
// or any path in exclude.
func fsGroup(ops fsOps, policy FsGroupPolicy, gid int, exclude, roots []string, out, warnOut io.Writer) error {
	if gid < 0 {
		return fmt.Errorf("invalid gid %d", gid)
	}
	if policy != PolicyAlways && policy != PolicyOnRootMismatch {
		return fmt.Errorf("unknown policy %q (want Always or OnRootMismatch)", policy)
	}
	excluded := map[string]bool{}
	for _, e := range exclude {
		excluded[posix.Clean(e)] = true
	}
	warn := &warner{w: warnOut}
	defer warn.flushSuppressed()
	if ops.MountPoints != nil {
		mps, err := ops.MountPoints()
		if err != nil {
			// The agent's explicit exclusions still hold.
			warn.warnf("[ucd-sh] fsgroup: cannot read the mount table, relying on the excluded paths only: %v\n", err)
		}
		for _, mp := range mps {
			excluded[posix.Clean(mp)] = true
		}
	}
	for _, root := range roots {
		root = posix.Clean(root)
		start := time.Now()
		if policy == PolicyOnRootMismatch && !requiresFsGroupChange(ops, root, gid) {
			fmt.Fprintf(out, "fsgroup %s: 0 entries, skipped (root already matches), %s\n", root, time.Since(start).Round(time.Millisecond))
			continue
		}
		info, err := ops.Lstat(root)
		if err != nil {
			return fmt.Errorf("%s: %w", root, err)
		}
		warn.failures = 0
		n, err := fsGroupWalk(ops, root, info, gid, walkScope{excluded: excluded}, warn)
		if err != nil {
			return fmt.Errorf("%s: %w", root, err)
		}
		failed := ""
		if warn.failures > 0 {
			failed = fmt.Sprintf(", %d failed", warn.failures)
		}
		fmt.Fprintf(out, "fsgroup %s: %d entries%s, %s\n", root, n, failed, time.Since(start).Round(time.Millisecond))
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

// FsGroup implements `ucd-sh fsgroup`. exclude lists paths no walk may enter.
func FsGroup(policy FsGroupPolicy, gid int, exclude, roots []string, out, warn io.Writer) error {
	ops, err := realOps()
	if err != nil {
		return err
	}
	return fsGroup(ops, policy, gid, exclude, roots, out, warn)
}

// ChownAs implements `ucd-sh chown-as`.
func ChownAs(uid, gid int, paths []string) error {
	ops, err := realOps()
	if err != nil {
		return err
	}
	return chownAs(ops, uid, gid, paths)
}

// parseMountinfo returns the mount points listed in /proc/self/mountinfo
// (field 5 of each line, with the kernel's \ooo octal escapes decoded).
func parseMountinfo(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		out = append(out, unescapeMountField(f[4]))
	}
	return out
}

func unescapeMountField(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
