//go:build windows

package main

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd") // #nosec G204 -- running the command is the point; the job came from the runner's owner
	// cmd.exe parses its own command line; Go's argument quoting would mangle it.
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: "cmd /c " + command}
	return cmd
}

func setProcGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// killOnCancel stops the process and its children (taskkill /T walks the tree).
func killOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		pid := strconv.Itoa(cmd.Process.Pid)
		return exec.Command("taskkill", "/T", "/F", "/PID", pid).Run() // #nosec G204 -- our own child's pid
	}
	cmd.WaitDelay = 5 * time.Second
}

// procExited: taskkill has no delayed second signal to settle.
func procExited(*exec.Cmd) {}

// killGroup: Windows has no group to signal once the parent is gone, and
// taskkill /T can't find orphans by a dead parent's id.
func killGroup(*exec.Cmd) {}

// processAlive: a process with that id runs (or exists under another user).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const queryLimited = 0x1000
	h, err := syscall.OpenProcess(queryLimited, false, uint32(pid)) // #nosec G115 -- pid > 0
	if err != nil {
		return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	var code uint32
	if syscall.GetExitCodeProcess(h, &code) != nil {
		return true
	}
	const stillActive = 259
	return code == stillActive
}
