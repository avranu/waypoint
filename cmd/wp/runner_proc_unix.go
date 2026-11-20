//go:build !windows

package main

import (
	"context"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// A job's process and everything it starts form one process group, so
// cancelling kills the lot.

func shellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", command) // #nosec G204 -- running the command is the point; the job came from the runner's owner
}

func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTimers holds the pending SIGKILL of each cancelled command, until it
// is waited for.
var killTimers sync.Map // *exec.Cmd -> *time.Timer

// killOnCancel makes a cancelled context stop the group: politely, then for
// good. Once the command has been waited for, procExited settles the pending
// kill, so a delayed signal never lands on a reused process id.
func killOnCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		pid := cmd.Process.Pid
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		killTimers.Store(cmd, time.AfterFunc(3*time.Second, func() {
			killTimers.Delete(cmd)
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}))
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
}

// procExited is called once cmd has been waited for. If its group was told
// to stop and the SIGKILL is still pending, the timer is dropped and any
// stragglers in the group are killed now, while the group id is still ours.
func procExited(cmd *exec.Cmd) {
	if t, ok := killTimers.LoadAndDelete(cmd); ok && t.(*time.Timer).Stop() && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// killGroup ends whatever is left in cmd's process group after cmd itself
// has been waited for: background processes it started and didn't wait for.
// Only for a cmd started with setProcGroup; the group's id stays reserved
// while any member lives, so it can't reach a stranger.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// processAlive: a process with that id runs (signal 0 checks without
// sending). Not 0 or below: kill(0) and kill(-n) reach process groups.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
