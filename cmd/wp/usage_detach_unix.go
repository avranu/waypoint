//go:build !windows

package main

import "syscall"

// detachedProc starts a child in its own session, so it outlives the
// status line command that started it.
func detachedProc() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
