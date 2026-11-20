//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Task sessions end themselves once their tasks are done (WP-97); wp done
// starts nothing itself (WP-271).

func TestDoneStartsNothing(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	t.Setenv("CLAUDECODE", "")
	first := s.task("First")
	s.task("Second")
	t.Setenv("WP_TASK", first)
	rec := fakeCLI(t, "claude", fakeClaudeStart)
	s.wp("next", first)
	out := s.wp("done", first, "-m", "done")
	if strings.Contains(out, "Stop here") || strings.Contains(out, "--next") {
		t.Errorf("output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(rec, "args")); err == nil {
		t.Error("wp done started a session")
	}
}

func TestTaskSessionEndsWhenFinished(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	t.Setenv("CLAUDECODE", "")
	bound := s.task("Bound")
	var stopped [][]string
	old := backgroundStart
	t.Cleanup(func() { backgroundStart = old })
	backgroundStart = func(exe string, args ...string) { stopped = append(stopped, args) }
	fakeCLI(t, "claude", fakeClaudeStart)

	sid := "c24e7b58-1111-2222-3333-444455556666"
	t.Setenv("WP_TASK", bound)
	s.hook(in(sid, "SessionStart", s.repo, map[string]any{"source": "startup"}))
	t.Setenv("WAYPOINT_SESSION", sid)
	stop := func() {
		s.hook(in(sid, "Stop", s.repo, map[string]any{"background_tasks": []any{}, "session_crons": []any{}}))
	}

	stop() // nothing done yet
	s.wp("next", bound)
	stop()              // holds a task
	s.wp("done", bound) // nothing else is ready: it may end
	if len(stopped) != 0 {
		t.Fatalf("ended too early: %v", stopped)
	}
	// Background work keeps it open.
	s.hook(in(sid, "Stop", s.repo, map[string]any{"background_tasks": []any{map[string]any{"type": "shell", "status": "running"}}, "session_crons": []any{}}))
	if len(stopped) != 0 {
		t.Fatalf("ended with background work: %v", stopped)
	}
	stop()
	if len(stopped) != 1 || strings.Join(stopped[0], " ") != "stop c24e7b58" {
		t.Fatalf("stopped = %v", stopped)
	}

	// Your own session is never ended.
	stopped = nil
	t.Setenv("WP_TASK", "")
	mine := s.task("Mine")
	s.hook(in("mine-1", "SessionStart", s.repo, map[string]any{"source": "startup"}))
	t.Setenv("WAYPOINT_SESSION", "mine-1")
	s.wp("next", mine)
	s.wp("done", mine)
	s.hook(in("mine-1", "Stop", s.repo, map[string]any{"background_tasks": []any{}, "session_crons": []any{}}))
	if len(stopped) != 0 {
		t.Errorf("ended your own session: %v", stopped)
	}
}

func TestTaskSessionNotEndedAfterAPersonPromptsOrWithARequestOpen(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	t.Setenv("CLAUDECODE", "")
	var stopped [][]string
	old := backgroundStart
	t.Cleanup(func() { backgroundStart = old })
	backgroundStart = func(exe string, args ...string) { stopped = append(stopped, args) }
	fakeCLI(t, "claude", fakeClaudeStart)
	finish := func(sid string) {
		s.hook(in(sid, "Stop", s.repo, map[string]any{"background_tasks": []any{}, "session_crons": []any{}}))
	}
	start := func(sid, key string) {
		t.Setenv("WP_TASK", key)
		s.hook(in(sid, "SessionStart", s.repo, map[string]any{"source": "startup"}))
		t.Setenv("WAYPOINT_SESSION", sid)
		s.hook(in(sid, "UserPromptSubmit", s.repo, map[string]any{"prompt": "Robin started this session for " + key + ": \"A\""}))
		s.wp("next", key)
	}

	// A person prompts after the done: left alone.
	a := s.task("A")
	start("aaaaaaaa-1", a)
	s.wp("done", a)
	time.Sleep(5 * time.Millisecond)
	s.hook(in("aaaaaaaa-1", "UserPromptSubmit", s.repo, map[string]any{"prompt": "actually, also check the docs"}))
	finish("aaaaaaaa-1")
	if len(stopped) != 0 {
		t.Fatalf("ended a session a person was using: %v", stopped)
	}

	// A request left open: not finished, so not ended.
	b := s.task("B")
	start("bbbbbbbb-1", b)
	s.wp("request", "ask", "Which one?", "--choice", "x", "--choice", "y")
	s.wp("done", b)
	finish("bbbbbbbb-1")
	if len(stopped) != 0 {
		t.Fatalf("ended with a request open: %v", stopped)
	}
}
