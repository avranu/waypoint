//go:build windows

package main

import "syscall"

// detachedProc: a new process group with no console, so the child outlives its parent.
func detachedProc() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000200 | 0x08000000}
}
