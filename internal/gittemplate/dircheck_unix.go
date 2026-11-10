//go:build !windows

package gittemplate

import (
	"fmt"
	"os"
	"syscall"
)

// checkOwnedPrivateDir refuses a directory that another user could have planted
// or could write to: it must be a real directory (not a symlink), owned by the
// current user, and not group- or other-writable. A writable known_hosts
// directory would let an attacker plant a host key and MITM every later fetch.
func checkOwnedPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory (symlinks are refused)", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d, not the controller's uid %d", dir, st.Uid, os.Geteuid())
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is group- or other-writable (mode %o); use mode 0700", dir, fi.Mode().Perm())
	}
	return nil
}
