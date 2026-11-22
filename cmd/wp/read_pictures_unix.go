//go:build !windows

package main

import (
	"os"
	"syscall"
)

// ownedByMe: the file belongs to the user running wp, not one planted in a
// shared temp directory by someone else.
func ownedByMe(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
