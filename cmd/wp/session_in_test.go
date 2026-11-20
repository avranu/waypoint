package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// exitedPid is the pid of a process that ran and is gone.
func exitedPid(t *testing.T) (int, string) {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Skip("no process to start")
	}
	pid := cmd.Process.Pid
	start := procStart(pid)
	_ = cmd.Wait()
	return pid, start
}

func stateWith(dir string, cs *cwdSession) *State {
	cs.At = time.Now()
	return &State{Cwds: map[string]*cwdSession{dir: cs}}
}

// The session of a directory is the one whose own directory is the nearest
// above the call, and only while it is alive (WP-1370): a session that ended
// in a worktree isn't the one a command there belongs to, and a session in an
// ancestor isn't either when the nearest one is gone.
func TestSessionInLiveNearestOnly(t *testing.T) {
	if procStart(os.Getpid()) == "" {
		t.Skip("no /proc")
	}
	root := t.TempDir()
	sub := filepath.Join(root, "wt")
	live := &cwdSession{Session: "live", Pid: os.Getpid(), PidStart: procStart(os.Getpid())}
	pid, start := exitedPid(t)
	dead := &cwdSession{Session: "dead", Pid: pid, PidStart: start}

	s := stateWith(root, live)
	if got := s.sessionIn(filepath.Join(sub, "deep")); got != "live" {
		t.Errorf("a live session in an ancestor: %q", got)
	}
	s.Cwds[sub] = dead
	s.Cwds[sub].At = time.Now()
	if got := s.sessionIn(filepath.Join(sub, "deep")); got != "" {
		t.Errorf("the nearest session is gone, the ancestor's isn't its: %q", got)
	}
	s.Cwds[sub] = &cwdSession{Session: "reused", Pid: os.Getpid(), PidStart: "not-its-start", At: time.Now()}
	if got := s.sessionIn(sub); got != "" {
		t.Errorf("a reused pid is another process: %q", got)
	}
	s.Cwds[sub] = &cwdSession{Session: "near", Pid: os.Getpid(), PidStart: procStart(os.Getpid()), At: time.Now()}
	if got := s.sessionIn(sub); got != "near" {
		t.Errorf("the live nearest: %q", got)
	}
}

// Entries recorded before the pid was kept are held to their age alone.
func TestSessionInWithoutPidKeepsAge(t *testing.T) {
	dir := t.TempDir()
	s := stateWith(dir, &cwdSession{Session: "old-entry"})
	if got := s.sessionIn(dir); got != "old-entry" {
		t.Errorf("got %q", got)
	}
}
