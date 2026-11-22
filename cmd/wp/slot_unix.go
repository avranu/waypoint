//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

// slotTryLock takes an exclusive flock without waiting; false means another
// process holds it. The kernel drops the lock when the file closes, however
// the holder died.
func slotTryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) // #nosec G115 -- fds fit an int
	switch err {
	case nil:
		return true, nil
	case syscall.EWOULDBLOCK:
		return false, nil
	}
	return false, err
}

// fileLock waits for an exclusive flock on f; unlock releases it (closing f
// does too).
func fileLock(f *os.File) (unlock func(), err error) {
	fd := int(f.Fd()) // #nosec G115 -- fds fit an int
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return nil, err
	}
	return func() { _ = syscall.Flock(fd, syscall.LOCK_UN) }, nil
}

// slotPidAlive reports whether a process with this pid exists (signal 0
// checks without sending; EPERM means it exists but isn't ours).
func slotPidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// slotPidSession is the Claude Code session whose Bash call started the
// process pid (its CLAUDE_CODE_SESSION_ID, read from /proc), "" when it can't
// be read. A variable so tests set it.
var slotPidSession = func(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)) // #nosec G304 -- a pid's own environment
	if err != nil {
		return ""
	}
	for _, e := range strings.Split(string(b), "\x00") {
		if v, ok := strings.CutPrefix(e, "CLAUDE_CODE_SESSION_ID="); ok {
			return v
		}
	}
	return ""
}
