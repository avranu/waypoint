//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
)

// restartSelf runs the updated wp with the same arguments and waits for it
// (Windows has no exec): this process, the old wp renamed aside, stays as
// its parent and exits with its exit code.
func restartSelf(exe string) error {
	cmd := exec.Command(exe, os.Args[1:]...) // #nosec G204 -- this program, just updated, with its own arguments
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.ExitCode())
	}
	if err != nil {
		return err
	}
	os.Exit(0)
	return nil
}
