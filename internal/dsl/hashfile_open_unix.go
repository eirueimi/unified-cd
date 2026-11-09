//go:build unix

package dsl

import "syscall"

// openNonblock keeps open(2) from blocking on a FIFO that replaced a regular
// file between the Lstat and the open.
const openNonblock = syscall.O_NONBLOCK
