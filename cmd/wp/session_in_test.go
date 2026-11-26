package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// exitedPid is the pid of a process that ran and is gone, with the start time
// it had while it ran (procStart is "" for a zombie, so it is read while the
// process is alive).
func exitedPid(t *testing.T) (int, string) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		if runtime.GOOS == "linux" {
			t.Fatalf("can't start sleep: %v", err)
		}
		t.Skip("no process to start")
	}
	pid := cmd.Process.Pid
	start := procStart(pid)
	if start == "" && runtime.GOOS == "linux" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("no start time for a running process")
	}
	_ = cmd.Process.Kill()
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
	needProc(t)
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

// A command run through a symlink to the directory a session started in, and a
// session recorded under a symlinked path, are in the same place: the nearest
// session is found, not an ancestor's (WP-1370).
func TestSessionInResolvesSymlinks(t *testing.T) {
	needProc(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symlinks")
	}
	self := procStart(os.Getpid())
	pid, start := exitedPid(t)
	ancestor := &cwdSession{Session: "ancestor", Pid: os.Getpid(), PidStart: self, At: time.Now(), Cwd: root}

	nearest := &cwdSession{Session: "nearest-dead", Pid: pid, PidStart: start, At: time.Now(), Cwd: real}
	s := &State{Cwds: map[string]*cwdSession{"a": ancestor, "n": nearest}}
	if got := s.sessionIn(filepath.Join(link, "deep")); got != "" {
		t.Errorf("command through a symlink: %q, want none (the nearest session is gone)", got)
	}
	nearest = &cwdSession{Session: "nearest", Pid: os.Getpid(), PidStart: self, At: time.Now(), Cwd: link}
	s = &State{Cwds: map[string]*cwdSession{"a": ancestor, "n": nearest}}
	if got := s.sessionIn(filepath.Join(real, "deep")); got != "nearest" {
		t.Errorf("session recorded by link, command by real path: %q", got)
	}
	if got := s.sessionIn(filepath.Join(link, "deep")); got != "nearest" {
		t.Errorf("both by link: %q", got)
	}
}
