//go:build !windows

package main

import (
	"context"
	"strings"
	"testing"
)

// WP-242: wp next shows wp done without a KEY when that can only mean the
// task it hands out; wp done finds it from another directory of the session.
func TestNextShowsKeylessDoneForTheOnlyTask(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	t.Setenv("WP_TASK", "")
	t.Setenv("WAYPOINT_SESSION", "sess-1")
	first, second, third := s.task("First"), s.task("Second"), s.task("Third")

	out := s.wp("next", first, "--no-brief")
	if !strings.Contains(out, "When done, run `wp ship -m \"what you did\" --summary \"your reply to the person\"`; if it ships cleanly, that reply ends your turn (add `--check \"CMD\"` to run your checks first, and `--shot URL` for each new frontend change). File questions with `wp ask \"question\"`, and if you need to give the task back, use `wp release -m \"why\"`.") {
		t.Errorf("one task:\n%s", out)
	}
	if out = s.wp("next", first, "--no-brief"); !strings.Contains(out, "`wp ship -m") {
		t.Errorf("asked again, still the one:\n%s", out)
	}
	if out = s.wp("next", second, "--no-brief"); !strings.Contains(out, "`wp ship "+second+" -m") {
		t.Errorf("second task:\n%s", out)
	}
	if out, code := s.run("done", "-m", "x"); code == 0 || !strings.Contains(out, "you hold "+second+", "+first) {
		t.Errorf("two held, no KEY: exit %d\n%s", code, out)
	}
	s.wp("done", first, "-m", "x")
	s.wp("done", "-m", "x")

	s.wp("next", third, "--no-brief")
	t.Chdir(t.TempDir())
	if out = s.wp("done", "-m", "finished"); !strings.Contains(out, third) {
		t.Errorf("done elsewhere in the session:\n%s", out)
	}
	fourth := s.task("Fourth")
	if out = s.wp("next", fourth, "-p", claimProject(fourth), "--no-brief"); !strings.Contains(out, "`wp ship -m") {
		t.Errorf("the only one left:\n%s", out)
	}
	fifth := s.task("Fifth")
	t.Chdir(s.repo)
	if out = s.wp("next", fifth, "--no-brief"); !strings.Contains(out, "`wp ship "+fifth+" -m") {
		t.Errorf("one held in the worktree:\n%s", out)
	}
	if out, code := s.run("release", "-m", "x"); code == 0 || !strings.Contains(out, "you hold ") {
		t.Errorf("one here, one in the worktree: exit %d\n%s", code, out)
	}
	s.asHuman()
	s.wp("edit", fourth, "--status", "Done")
	s.asAgent()
	if out = s.wp("done", "-m", "finished"); !strings.Contains(out, fifth) {
		t.Errorf("the other finished on the board:\n%s", out)
	}
	sixth, seventh := s.task("Sixth"), s.task("Seventh")
	s.wp("next", sixth, "--no-brief")
	s.asHuman()
	s.wp("edit", sixth, "--status", "Done")
	s.asAgent()
	if out = s.wp("next", seventh, "--no-brief"); !strings.Contains(out, "`wp ship -m") {
		t.Errorf("held one finished on the board:\n%s", out)
	}
	s.wp("done", "-m", "x")

	for _, k := range []string{first, second, third, fourth, fifth, sixth, seventh} {
		if d, err := s.svc.GetIssue(context.Background(), s.me, k); err != nil || !d.Closed() {
			t.Errorf("%s not done: %v", k, err)
		}
	}
}

// WP-770: autopilot's answer opens by saying so and ends with what to do; a
// session can decline what it was handed with the wp hold the answer names.
func TestAssignedNextOpensAndEndsTheAnswer(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	t.Setenv("WP_TASK", "")
	t.Setenv("WAYPOINT_SESSION", "sess-1")
	key := s.task("First")

	out := strings.TrimSpace(s.wp("next", key, "--assigned", "--no-brief"))
	if !strings.HasPrefix(out, "Autopilot assigned "+key+" to you: \"First\"") {
		t.Errorf("head:\n%s", out)
	}
	last := out[strings.LastIndex(out, "\n")+1:]
	if !strings.HasPrefix(last, "Assess and complete the task as appropriate.") {
		t.Errorf("last line isn't what to do:\n%s", out)
	}
	if strings.Count(out, "Assess and complete") != 1 {
		t.Errorf("said twice:\n%s", out)
	}
	s.wp("hold", "-m", "stale")
	if show := s.wp("show", key); !strings.Contains(show, "On hold") {
		t.Errorf("declined:\n%s", show)
	}
	if out, code := s.run("next", key, "--no-brief"); code == 0 {
		t.Errorf("a held task was handed out again:\n%s", out)
	}
}
