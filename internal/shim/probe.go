package shim

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// Probe implements `ucd-sh secctx-probe <dir>`. It prints, one line each:
//
//	uid=<euid> gid=<egid> groups=<comma-separated supplementary gids>
//	file_uid=<n> file_gid=<n> dir_mode=<octal incl. setgid, e.g. 2770>
//	uid_map=<lines joined by ';', fields by single spaces>   (when readable)
//	gid_map=<same>                                           (when readable)
//
// The file_* line describes a file the probe creates in dir (and removes
// again), so the caller can observe setgid group inheritance and the
// user-namespace mapping. Used only by the host agent startup probe.
func Probe(dir string, w io.Writer) error {
	groups, err := os.Getgroups()
	if err != nil {
		return fmt.Errorf("getgroups: %w", err)
	}
	gs := make([]string, len(groups))
	for i, g := range groups {
		gs[i] = fmt.Sprint(g)
	}
	fmt.Fprintf(w, "uid=%d gid=%d groups=%s\n", os.Geteuid(), os.Getegid(), strings.Join(gs, ","))

	f, err := os.CreateTemp(dir, ".ucd-probe-file-")
	if err != nil {
		return fmt.Errorf("create file in %s: %w", dir, err)
	}
	name := f.Name()
	f.Close()
	defer os.Remove(name)
	fi, err := os.Lstat(name)
	if err != nil {
		return err
	}
	di, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	fuid, fgid, ok := fileOwner(fi)
	if !ok {
		return fmt.Errorf("cannot read file ownership: %w", errUnsupported)
	}
	mode := int(di.Mode().Perm())
	if di.Mode()&fs.ModeSetgid != 0 {
		mode |= 0o2000
	}
	fmt.Fprintf(w, "file_uid=%d file_gid=%d dir_mode=%04o\n", fuid, fgid, mode)

	for _, m := range []struct{ key, path string }{{"uid_map", "/proc/self/uid_map"}, {"gid_map", "/proc/self/gid_map"}} {
		b, err := os.ReadFile(m.path)
		if err != nil {
			continue
		}
		var lines []string
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			lines = append(lines, strings.Join(strings.Fields(l), " "))
		}
		fmt.Fprintf(w, "%s=%s\n", m.key, strings.Join(lines, ";"))
	}
	return nil
}
