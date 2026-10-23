//go:build linux

package shim

import (
	"io/fs"
	"os"
	"syscall"
)

func realOps() (fsOps, error) {
	return fsOps{
		Lstat: func(p string) (fileInfo, error) {
			fi, err := os.Lstat(p)
			if err != nil {
				return fileInfo{}, err
			}
			info := fileInfo{Mode: fi.Mode()}
			if uid, gid, ok := fileOwner(fi); ok {
				info.UID, info.GID = uid, gid
			}
			return info, nil
		},
		Lchown: os.Lchown,
		Chmod:  func(p string, m fs.FileMode) error { return os.Chmod(p, m) },
		ReadDir: func(p string) ([]string, error) {
			es, err := os.ReadDir(p)
			if err != nil {
				return nil, err
			}
			names := make([]string, len(es))
			for i, e := range es {
				names[i] = e.Name()
			}
			return names, nil
		},
	}, nil
}

// fileOwner returns the uid and gid of a stat result.
func fileOwner(fi fs.FileInfo) (uid, gid int, ok bool) {
	st, isStat := fi.Sys().(*syscall.Stat_t)
	if !isStat || st == nil {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
