//go:build !windows

package main

import (
	"os"
	"syscall"
)

// inodeOf identifies a file across renames, where the OS has inodes.
func inodeOf(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}
