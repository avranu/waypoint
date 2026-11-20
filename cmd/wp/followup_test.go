package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// wp followup (WP-D22): the session's person queues steps from a terminal;
// agents are refused, and told who can do it.
func TestFollowupCommands(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	s.hook(in("sess-1", "SessionStart", s.repo, map[string]any{"source": "startup"}))
	sid := s.sessions()[0].ID
	ctx := context.Background()
	t.Setenv("WAYPOINT_SESSION", "sess-1")

	// The agent can't, and the error says how its person can.
	out, code := s.run("followup", "set", "--tell", "keep going")
	if code == 0 || !strings.Contains(out, "for people") || !strings.Contains(out, "WAYPOINT_AGENT=none wp followup set") {
		t.Fatalf("an agent queued steps (exit %d): %s", code, out)
	}
	if out, code := s.run("followup"); code == 0 || !strings.Contains(out, "for people") {
		t.Fatalf("an agent read the follow-up (exit %d): %s", code, out)
	}

	// The person, in the same shell: this session, steps in flag order.
	s.asHuman()
	if out := s.wp("followup"); !strings.Contains(out, "Nothing is queued") {
		t.Fatalf("show with nothing queued: %s", out)
	}
	prompt := filepath.Join(t.TempDir(), "next.md")
	_ = os.WriteFile(prompt, []byte("Review what the last agent did"), 0o600)
	out = s.wp("followup", "set", "--tell", "Now write the tests", "--fresh", "opus", "-f", prompt, "--permission-mode", "auto")
	if !strings.Contains(out, "Queued 2 steps") {
		t.Fatalf("set: %s", out)
	}
	fu, err := s.svc.GetFollowUp(ctx, s.me, sid)
	if err != nil {
		t.Fatal(err)
	}
	if fu.State != "armed" || len(fu.Steps) != 2 || fu.Steps[0].Type != "tell_agent" || fu.Steps[0].Message != "Now write the tests" ||
		fu.Steps[1].Type != "start_session" || fu.Steps[1].Extra["model"] != "opus" || fu.Steps[1].Extra["prompt"] != "Review what the last agent did" ||
		fu.Steps[1].Extra["permission_mode"] != "auto" {
		t.Fatalf("queued: %+v", fu)
	}
	out = s.wp("followup", "show")
	if !strings.Contains(out, "armed") || !strings.Contains(out, "1. tell the agent: Now write the tests") || !strings.Contains(out, "2. start a fresh agent (opus): Review") {
		t.Fatalf("show: %s", out)
	}

	// --append adds after; --steps-json takes any step; by number from elsewhere.
	t.Setenv("WAYPOINT_SESSION", "")
	t.Chdir(t.TempDir())
	if out, code := s.run("followup"); code == 0 || !strings.Contains(out, "no agent session here") {
		t.Fatalf("outside the session: %s", out)
	}
	steps := filepath.Join(t.TempDir(), "steps.json")
	_ = os.WriteFile(steps, []byte(`[{"type":"notify","user":"actor","message":"all done"}]`), 0o600)
	s.wp("followup", "set", strconv.FormatInt(sid, 10), "--append", "--steps-json", steps)
	if fu, _ = s.svc.GetFollowUp(ctx, s.me, sid); len(fu.Steps) != 3 || fu.Steps[2].Type != "notify" {
		t.Fatalf("appended: %+v", fu.Steps)
	}
	// In the session's directory, it is found without being named.
	t.Chdir(s.repo)
	if out := s.wp("followup", "show", "--json"); !strings.Contains(out, `"notify"`) {
		t.Fatalf("by directory: %s", out)
	}
	// Mistakes are caught before anything is sent.
	if out, code := s.run("followup", "set", "--prompt", "x"); code != 2 || !strings.Contains(out, "goes after the --fresh") {
		t.Fatalf("--prompt without --fresh: %d %s", code, out)
	}
	if out, code := s.run("followup", "set", "--fresh", "opus"); code != 2 || !strings.Contains(out, "needs a prompt") {
		t.Fatalf("--fresh without a prompt: %d %s", code, out)
	}
	// Someone else's session isn't found.
	if out, code := s.run("followup", "show", "other-cli-id"); code == 0 || !strings.Contains(out, "no session other-cli-id") {
		t.Fatalf("an unknown session: %s", out)
	}

	if out := s.wp("followup", "clear"); !strings.Contains(out, "Cleared") {
		t.Fatalf("clear: %s", out)
	}
	if fu, _ = s.svc.GetFollowUp(ctx, s.me, sid); fu.State != "none" {
		t.Fatalf("after clear: %+v", fu)
	}
	if out, code := s.run("followup", "run"); code == 0 || !strings.Contains(out, "nothing is queued") {
		t.Fatalf("run with nothing queued: %s", out)
	}
}
