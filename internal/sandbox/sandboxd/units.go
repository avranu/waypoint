//go:build linux

package sandboxd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// Units are the machine's transient systemd services: how the helper runs
// a session's broker and the session itself (WP-470). The tests use a
// fake.
type Units interface {
	// StartUnit starts a transient service as uid:gid and returns once
	// it runs.
	StartUnit(unit string, uid, gid int, props, argv []string) error
	// RunUnit runs a transient service as uid:gid with stdio as its
	// stdin, stdout and stderr (nil: /dev/null), and returns its exit
	// status when it ends.
	RunUnit(unit string, uid, gid int, props, argv []string, stdio [3]*os.File) (int, error)
	// StopUnits stops units and waits for them; one that isn't loaded is
	// already stopped.
	StopUnits(units ...string) error
}

const (
	systemdRun = "/usr/bin/systemd-run"
	systemctl  = "/usr/bin/systemctl"
)

func runArgs(unit string, uid, gid int, props, argv []string) []string {
	args := []string{"--unit=" + unit, "--uid=" + strconv.Itoa(uid), "--gid=" + strconv.Itoa(gid),
		"--collect", "--quiet", "--service-type=exec", "--expand-environment=no"}
	for _, p := range props {
		args = append(args, "--property="+p)
	}
	return append(append(args, "--"), argv...)
}

func (OS) StartUnit(unit string, uid, gid int, props, argv []string) error {
	return run(systemdRun, runArgs(unit, uid, gid, props, argv)...)
}

func (OS) RunUnit(unit string, uid, gid int, props, argv []string, stdio [3]*os.File) (int, error) {
	args := append([]string{"--wait", "--pipe"}, runArgs(unit, uid, gid, props, argv)...)
	cmd := exec.Command(systemdRun, args...) // #nosec G204 -- systemd-run; the unit's command is the helper's own binary
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio[0], stdio[1], stdio[2]
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

func (OS) StopUnits(units ...string) error {
	var errs []error
	for _, u := range units { // one at a time: a unit that isn't loaded fails the whole call
		err := runFor(3*time.Minute, systemctl, "stop", "--", u)
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 5 {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("stopping %s: %w", u, err))
		}
	}
	return errors.Join(errs...)
}
