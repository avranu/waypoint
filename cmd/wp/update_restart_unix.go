//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// restartSelf runs the updated wp in place of this process, with the same
// arguments and environment: the same PID, so systemd and launchd see the
// runner carry on. If that fails, the error exits non-zero and the service
// manager starts it again.
func restartSelf(exe string) error {
	// #nosec G204 G702 -- this program, just updated, with its own arguments
	err := syscall.Exec(exe, append([]string{exe}, os.Args[1:]...), os.Environ())
	return fmt.Errorf("restarting on the new wp: %w", err)
}
