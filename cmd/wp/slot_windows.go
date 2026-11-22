//go:build windows

package main

import (
	"errors"
	"os"
)

func slotTryLock(*os.File) (bool, error) {
	return false, errors.New("host slots need flock, which Windows lacks; run the command directly")
}

// fileLock has no flock to take on Windows; callers go on unlocked.
var fileLock = func(*os.File) (func(), error) {
	return nil, errors.New("no flock on Windows")
}

// slotPidAlive is never reached on Windows: no slot is ever held there.
func slotPidAlive(int) bool { return false }

// slotPidSession can't read another process's environment on Windows.
var slotPidSession = func(int) string { return "" }
