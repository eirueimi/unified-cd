//go:build !linux

package shim

import "io/fs"

func realOps() (fsOps, error) { return fsOps{}, errUnsupported }

func fileOwner(fs.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
