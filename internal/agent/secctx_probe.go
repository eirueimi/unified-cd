package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

// IDs the probe uses. They sit inside the default 65536 rootless range.
const (
	probeFSGroup = 54321
	probeUID     = 54322
	probeGID     = 54323
)

// probeTimeout bounds the whole startup probe (two container starts and a
// possible image pull). A probe that does not finish in time is a failed
// probe, never a stuck agent.
const probeTimeout = 3 * time.Minute

// probeResult is what the startup probe learned. OK is true only when the
// runtime delivered every securityContext behaviour the host agent relies on
// (design 7.1); Reason then names the first check that failed. Rootless and
// MaxMappedID come from the root helper's /proc/self/{uid,gid}_map and are
// meaningful whenever the helper ran, passed or not. MaxMappedID is the highest
// mapped container ID, inclusive; 0 means unlimited (rootful).
type probeResult struct {
	OK          bool
	Reason      string
	Rootless    bool
	MaxMappedID int64
}

// probeSecurityContextFn lets tests substitute the probe.
var probeSecurityContextFn = probeSecurityContext

// probeSecurityContext measures, through the real container runtime, whether a
// claim pod's securityContext behaves like Kubernetes: a root helper (no
// --user) applies fsGroup to a bind-mounted directory, then a container run as
// --user 54322:54323 --group-add 54321 must see that identity and create files
// that inherit the group through the setgid directory (design 7.1 steps 1-5).
//
// A failed check is reported as OK=false with a Reason and a nil error; an
// infrastructure problem (cannot create the temp dir, the runtime refuses to
// start a container) is also OK=false with the cause as Reason, and the same
// cause as the error. Callers treat both the same: the capability is simply
// not advertised. Everything the probe creates (containers, temp dir) is
// removed on every path, including a cancelled ctx.
func probeSecurityContext(ctx context.Context, rt crt.ContainerRuntime, wsBase, toolsDir, pauseImage string) (res probeResult, err error) {
	fail := func(format string, args ...any) (probeResult, error) {
		res.OK = false
		res.Reason = fmt.Sprintf(format, args...)
		return res, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return fail("probe did not run: %v", cerr)
	}

	// 1. A dot-prefixed dir is out of workspace GC and job reach.
	dir, mkErr := os.MkdirTemp(wsBase, ".ucd-probe-")
	if mkErr != nil {
		res.Reason = fmt.Sprintf("cannot create probe directory in %s: %v", wsBase, mkErr)
		return res, mkErr
	}

	var handles []crt.ContainerHandle
	var root crt.ContainerHandle
	defer func() {
		// 5. Remove everything, through the root helper first (files a
		// non-root or subordinate-ID owner left behind), then the dir.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if root.ID != "" {
			_, _ = rt.Exec(cctx, root, crt.ExecSpec{Shell: []string{"find", "/p", "-mindepth", "1"}, Script: "-delete"}, io.Discard, io.Discard)
		}
		for i := len(handles) - 1; i >= 0; i-- {
			if rmErr := rt.Remove(cctx, handles[i]); rmErr != nil {
				slog.Debug("securityContext probe: remove container", "container", handles[i].ID, "error", rmErr)
			}
		}
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			slog.Warn("securityContext probe: could not remove its temp directory", "dir", dir, "error", rmErr)
		}
	}()

	mounts := []crt.Mount{{HostPath: dir, ContainerPath: "/p"}}
	if toolsDir != "" {
		mounts = append(mounts, crt.Mount{HostPath: toolsDir, ContainerPath: "/.ucd", ReadOnly: true})
	}

	// 2. The root helper: created WITHOUT --user, so a runtime that overrides
	// the image's default user (podman --userns=keep-id) shows up as uid != 0.
	h, cErr := rt.Create(ctx, crt.CreateSpec{Image: pauseImage, Entrypoint: ucdShPause, Mounts: mounts})
	if cErr != nil {
		res.Reason = fmt.Sprintf("cannot start the probe helper container (image %q): %v", pauseImage, cErr)
		return res, cErr
	}
	root = h
	handles = append(handles, h)

	exec := func(c crt.ContainerHandle, argv ...string) (string, int, error) {
		var out bytes.Buffer
		n := len(argv)
		code, eErr := rt.Exec(ctx, c, crt.ExecSpec{Shell: argv[:n-1], Script: argv[n-1]}, &out, &out)
		return out.String(), code, eErr
	}

	out, code, eErr := exec(root, "/.ucd/ucd-sh", "fsgroup", strconv.Itoa(probeFSGroup), "/p")
	if eErr != nil {
		res.Reason = fmt.Sprintf("ucd-sh fsgroup in the root helper: %v", eErr)
		return res, eErr
	}
	if code != 0 {
		return fail("ucd-sh fsgroup exited with code %d in the root helper: %s", code, strings.TrimSpace(out))
	}
	out, code, eErr = exec(root, "/.ucd/ucd-sh", "secctx-probe", "/p")
	if eErr != nil {
		res.Reason = fmt.Sprintf("ucd-sh secctx-probe in the root helper: %v", eErr)
		return res, eErr
	}
	if code != 0 {
		return fail("ucd-sh secctx-probe exited with code %d in the root helper: %s", code, strings.TrimSpace(out))
	}
	rootInfo := parseProbeOutput(out)
	res.Rootless, res.MaxMappedID = mappedRange(rootInfo.uidMap, rootInfo.gidMap)
	if !rootInfo.haveID || rootInfo.uid != 0 {
		return fail("the root helper container (no --user) ran as uid %s, not 0: the runtime overrides the default user (for example podman --userns=keep-id) or the pause image does not run as root", rootInfo.uidString())
	}

	// 3. The container that carries the identity under test.
	uh, cErr := rt.Create(ctx, crt.CreateSpec{
		Image:      pauseImage,
		Entrypoint: ucdShPause,
		User:       fmt.Sprintf("%d:%d", probeUID, probeGID),
		GroupAdd:   []string{strconv.Itoa(probeFSGroup)},
		Mounts:     mounts,
	})
	if cErr != nil {
		res.Reason = fmt.Sprintf("cannot start a container with --user %d:%d --group-add %d: %v", probeUID, probeGID, probeFSGroup, cErr)
		return res, cErr
	}
	handles = append(handles, uh)
	out, code, eErr = exec(uh, "/.ucd/ucd-sh", "secctx-probe", "/p")
	if eErr != nil {
		res.Reason = fmt.Sprintf("ucd-sh secctx-probe in the --user container: %v", eErr)
		return res, eErr
	}
	if code != 0 {
		return fail("ucd-sh secctx-probe exited with code %d in the --user container: %s", code, strings.TrimSpace(out))
	}

	// 4. Expectations.
	u := parseProbeOutput(out)
	switch {
	case !u.haveID:
		return fail("cannot read uid and gid from the --user container's probe output %q", strings.TrimSpace(out))
	case u.uid != probeUID:
		return fail("the --user container ran with uid %d, want %d", u.uid, probeUID)
	case u.gid != probeGID:
		return fail("the --user container ran with gid %d, want %d", u.gid, probeGID)
	case !u.hasGroup(probeFSGroup):
		return fail("the --user container lacks supplementary group %d (--group-add not honoured; groups=%v)", probeFSGroup, u.groups)
	case !u.haveFile:
		return fail("cannot read the created file's owner and mode from the probe output %q", strings.TrimSpace(out))
	case u.fileUID != probeUID:
		return fail("a file created in the workspace has file owner %d, want %d", u.fileUID, probeUID)
	case u.fileGID != probeFSGroup:
		return fail("a file created in the workspace has group %d, want %d: setgid inheritance did not give it the fsGroup", u.fileGID, probeFSGroup)
	case u.dirMode&0o2000 == 0:
		return fail("the workspace directory lost its setgid bit (mode %04o)", u.dirMode)
	case u.dirMode&0o070 != 0o070:
		return fail("the workspace directory does not give the group rwx (mode %04o)", u.dirMode)
	}
	res.OK = true
	return res, nil
}

// probeInfo is the parsed output of `ucd-sh secctx-probe` (internal/shim/probe.go).
type probeInfo struct {
	haveID           bool
	uid, gid         int64
	groups           []int64
	haveFile         bool
	fileUID, fileGID int64
	dirMode          int64
	uidMap, gidMap   string
}

func (p probeInfo) hasGroup(g int64) bool {
	for _, x := range p.groups {
		if x == g {
			return true
		}
	}
	return false
}

func (p probeInfo) uidString() string {
	if !p.haveID {
		return "(unreadable)"
	}
	return strconv.FormatInt(p.uid, 10)
}

// parseProbeOutput reads the fixed-format lines the shim prints. Unknown or
// malformed lines are ignored; a missing field leaves haveID/haveFile false.
func parseProbeOutput(s string) probeInfo {
	var p probeInfo
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "uid="):
			kv := keyValues(line)
			uid, e1 := strconv.ParseInt(kv["uid"], 10, 64)
			gid, e2 := strconv.ParseInt(kv["gid"], 10, 64)
			if e1 != nil || e2 != nil {
				continue
			}
			p.haveID, p.uid, p.gid = true, uid, gid
			for _, g := range strings.Split(kv["groups"], ",") {
				if n, err := strconv.ParseInt(g, 10, 64); err == nil {
					p.groups = append(p.groups, n)
				}
			}
		case strings.HasPrefix(line, "file_uid="):
			kv := keyValues(line)
			fu, e1 := strconv.ParseInt(kv["file_uid"], 10, 64)
			fg, e2 := strconv.ParseInt(kv["file_gid"], 10, 64)
			dm, e3 := strconv.ParseInt(kv["dir_mode"], 8, 64)
			if e1 != nil || e2 != nil || e3 != nil {
				continue
			}
			p.haveFile, p.fileUID, p.fileGID, p.dirMode = true, fu, fg, dm
		case strings.HasPrefix(line, "uid_map="):
			p.uidMap = strings.TrimPrefix(line, "uid_map=")
		case strings.HasPrefix(line, "gid_map="):
			p.gidMap = strings.TrimPrefix(line, "gid_map=")
		}
	}
	return p
}

func keyValues(line string) map[string]string {
	kv := map[string]string{}
	for _, f := range strings.Fields(line) {
		if k, v, ok := strings.Cut(f, "="); ok {
			kv[k] = v
		}
	}
	return kv
}

// unlimitedIDCount is the size of the identity mapping `0 0 4294967295` of a
// rootful runtime (and of any range that covers the whole 32-bit ID space).
const unlimitedIDCount = 4294967295

// mappedRange derives rootless and the highest mapped container ID from the
// helper's uid_map and gid_map (lines joined by ';'). The narrower of the two
// maps wins, because one limit has to cover both UIDs and GIDs. A map that
// covers the whole ID space, or one that could not be read, imposes no limit;
// both unlimited gives (false, 0). Rootless means at least one readable map is
// not the identity mapping.
func mappedRange(uidMap, gidMap string) (rootless bool, maxID int64) {
	for _, m := range []string{uidMap, gidMap} {
		high, unlimited, ok := highestMapped(m)
		if !ok || unlimited {
			continue
		}
		rootless = true
		if maxID == 0 || high < maxID {
			maxID = high
		}
	}
	return rootless, maxID
}

// highestMapped returns the highest container ID in one joined map. ok is false
// for an empty or unparseable map; unlimited when a line is `0 0 4294967295`
// or larger.
func highestMapped(m string) (high int64, unlimited, ok bool) {
	for _, line := range strings.Split(m, ";") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		inside, e1 := strconv.ParseInt(f[0], 10, 64)
		count, e3 := strconv.ParseInt(f[2], 10, 64)
		if e1 != nil || e3 != nil || count <= 0 {
			continue
		}
		ok = true
		if count >= unlimitedIDCount {
			return 0, true, true
		}
		if h := inside + count - 1; h > high {
			high = h
		}
	}
	return high, false, ok
}

// securityContextProbe runs the startup probe once per process and returns its
// result. It needs a resolved runtime, the shim tools dir and the pause image;
// without any of them the probe is skipped and the result carries the reason.
// It never fails agent startup: a failed probe only means the securitycontext
// capability is not advertised.
func (a *Agent) securityContextProbe() probeResult {
	a.probeOnce.Do(func() {
		rt, rtErr := a.containerRuntime()
		switch {
		case rtErr != nil:
			a.probeRes = probeResult{Reason: "no container runtime"}
			return
		case a.ToolsDir == "":
			a.probeRes = probeResult{Reason: "the ucd-sh shim tools directory is not set"}
			return
		case a.PauseImage == "":
			a.probeRes = probeResult{Reason: "no pause image is configured"}
			return
		}
		wsBase, err := ResolveWorkspaceDir(a.WorkspaceDir)
		if err != nil {
			a.probeRes = probeResult{Reason: err.Error()}
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		res, perr := probeSecurityContextFn(ctx, rt, wsBase, a.ToolsDir, a.PauseImage)
		if perr != nil && res.Reason == "" {
			res.Reason = perr.Error()
		}
		if res.OK {
			res.Reason = ""
		}
		a.probeRes = res
	})
	return a.probeRes
}
