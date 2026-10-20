package k8sagent

import (
	"fmt"
	"path"
	"sort"
	"strings"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
)

// shQuote single-quotes s for a POSIX shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// clearWorkspaceScript returns the script that empties root, dot-prefixed
// entries included, without touching any volume mounted below it.
//
// A volume mounted below the root is a different filesystem the job asked to
// keep, and the pod mounts it into the very container this script runs in, so
// a plain "rm -rf root/*" recurses straight through its mount point and empties
// it. The host agent cannot make that mistake (its nested mount point is an
// empty host directory when the wipe runs). Here the script has to know where
// the mount points are: it removes everything except each mount point, and it
// descends into (instead of removing) each directory on the path to one, so
// that directory's other children are still removed.
//
// It is a shell function built on rm alone, because rm is the one tool the
// original one-liner already required of the job image; a find(1) with -prune
// would add a second. The paths are matched as literal strings (quoted case
// patterns), never as globs, and the only globs are literal ones in the script,
// applied after a cd, so that a mount path holding *, ? or [ cannot change what
// is enumerated. With no nested volume the script is the original one-liner.
//
// It assumes a Linux pod. The interpreter that runs it (ucd-sh, mvdan.cc/sh)
// joins glob results with filepath.Separator, so on a Windows container the
// separator would be a backslash, no case pattern here would match, and the
// clean would empty every nested volume. mountPath is forced POSIX and the
// workspace mount is /workspace, which keeps Windows pods out; do not relax
// either without rewriting this script.
func clearWorkspaceScript(root string, vols []agentlib.MountedVolume) string {
	root = path.Clean(root)
	q := shQuote(root)
	below := agentlib.VolumesBelow(root, vols)
	if len(below) == 0 {
		return fmt.Sprintf("rm -rf %[1]s/* %[1]s/.[!.]* %[1]s/..?*", q)
	}

	mounts := map[string]bool{}
	for _, v := range below {
		mounts[path.Clean(v.Location)] = true
	}
	// Every directory between root and a mount point, exclusive of both, that
	// is not itself a mount point.
	ancestors := map[string]bool{}
	for m := range mounts {
		for d := path.Dir(m); d != root && d != "/" && d != "."; d = path.Dir(d) {
			if !mounts[d] {
				ancestors[d] = true
			}
		}
	}
	list := func(set map[string]bool) string {
		var ps []string
		for p := range set {
			ps = append(ps, shQuote(p))
		}
		sort.Strings(ps)
		return strings.Join(ps, "|")
	}

	var b strings.Builder
	b.WriteString("ucd_rc=0\n")
	b.WriteString("ucd_wipe() {\n")
	// The glob patterns are literals in the script, so no expansion carries a
	// job-authored directory name: ucd-sh pattern-escapes a quoted glob base and
	// then fails to find it when the name holds *, ? or [. $1 is always an
	// absolute path, so it is only ever concatenated for case-matching and for
	// the recursive call, and the loop works on bare names relative to the
	// directory it has cd'd into. The cd is repeated after each recursion
	// because the callee leaves the working directory in its own subdirectory;
	// if that cd fails the loop's remaining relative names are meaningless, so
	// the function gives up with ucd_rc set.
	b.WriteString("  cd \"$1\" || { ucd_rc=1; return; }\n")
	b.WriteString("  for e in * .[!.]* ..?*; do\n")
	b.WriteString("    if [ ! -e \"$e\" ] && [ ! -L \"$e\" ]; then continue; fi\n")
	b.WriteString("    case \"$1/$e\" in\n")
	fmt.Fprintf(&b, "      %s) ;;\n", list(mounts))
	if len(ancestors) > 0 {
		fmt.Fprintf(&b, "      %s) ucd_wipe \"$1/$e\"; cd \"$1\" || { ucd_rc=1; return; } ;;\n", list(ancestors))
	}
	b.WriteString("      *) rm -rf \"$1/$e\" || ucd_rc=1 ;;\n")
	b.WriteString("    esac\n")
	b.WriteString("  done\n")
	b.WriteString("}\n")
	fmt.Fprintf(&b, "ucd_wipe %s\n", q)
	b.WriteString("exit $ucd_rc\n")
	return b.String()
}
