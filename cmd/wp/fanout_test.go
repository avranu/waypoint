//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// fakeClaudeLaunches records each launch: where, WP_TASK, WP_FANOUT,
// WP_BUDGET_USD, --name and --model (after --bg --settings JSON).
const fakeClaudeLaunches = `#!/bin/sh
echo "$(pwd)|$WP_TASK|$WP_FANOUT|$WP_BUDGET_USD|$5|$7" >> "$FAKE_OUT/launches"
echo "backgrounded · c24e7b58 · name"
`

func TestReadFanoutPlan(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("a.md", "From a file.")
	p, err := readFanoutPlan(write("ok.toml", `model = "opus"
budget = 5
[[package]]
title = "One"
file = "a.md"
[[package]]
title = "Two"
body = "Inline."
model = "haiku"
`))
	if err != nil || len(p.Packages) != 2 || p.Packages[0].Body != "From a file." || p.Packages[1].Model != "haiku" || p.Model != "opus" {
		t.Fatalf("plan: %+v %v", p, err)
	}
	if _, err := readFanoutPlan(write("typo.toml", "[[package]]\ntitle = \"x\"\nbudjet = 3\n")); err == nil || !strings.Contains(err.Error(), "budjet") {
		t.Errorf("unknown key: %v", err)
	}
	if _, err := readFanoutPlan(write("both.toml", "[[package]]\ntitle = \"x\"\nbody = \"y\"\nfile = \"a.md\"\n")); err == nil || !strings.Contains(err.Error(), "both") {
		t.Errorf("body and file: %v", err)
	}
}

func TestFanoutRunsPackagesAsTaskSessions(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	t.Setenv("CLAUDECODE", "")
	t.Setenv("WP_TASK", "")
	t.Setenv("WP_FANOUT", "")
	t.Setenv("WP_BUDGET_USD", "")
	t.Setenv("WAYPOINT_TRANSCRIPT", "")
	git(t, s.repo, "commit", "--allow-empty", "-q", "-m", "first")
	rec := fakeCLI(t, "claude", fakeClaudeLaunches)
	nexts := stubStartNext(s)
	bodies := recordRequests(t)
	var detached [][]string
	old := backgroundStart
	t.Cleanup(func() { backgroundStart = old })
	backgroundStart = func(exe string, args ...string) bool { detached = append(detached, args); return true }

	plan := filepath.Join(t.TempDir(), "plan.toml")
	if err := os.WriteFile(plan, []byte(`parallel = 2
base = "HEAD"
budget = 6
[[package]]
title = "Split the importer"
body = "Read and write halves."
[[package]]
title = "Rename the flags"
model = "haiku"
budget = 2
[[package]]
title = "Document it"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	out := s.wp("fanout", plan, "--dry-run")
	if !strings.Contains(out, "dry run") || !strings.Contains(out, "Estimate: $") {
		t.Errorf("dry run:\n%s", out)
	}
	if list, _ := s.svc.ListFanouts(context.Background(), s.me, true); len(list) != 0 {
		t.Fatal("a dry run filed a fan-out")
	}

	out = s.wp("fanout", plan)
	list, err := s.svc.ListFanouts(context.Background(), s.me, true)
	if err != nil || len(list) != 1 {
		t.Fatalf("fan-outs: %v %v\n%s", list, err, out)
	}
	f := list[0]
	id := strconv.FormatInt(f.ID, 10)
	a, b := f.Items[0], f.Items[1]
	if strings.Count(out, "Started ") != 2 || !strings.Contains(out, "Started "+a.Key) || !strings.Contains(out, "Started "+b.Key) {
		t.Fatalf("wp fanout:\n%s", out)
	}
	real, _ := filepath.EvalSymlinks(s.repo)
	launches, _ := os.ReadFile(filepath.Join(rec, "launches"))
	lines := strings.Split(strings.TrimSpace(string(launches)), "\n")
	wantA := filepath.Join(real, ".claude", "worktrees", a.Branch) + "|" + a.Key + "|" + id + "|6.00|" + a.Key + " Split the importer|sonnet"
	if len(lines) != 2 || lines[0] != wantA || !strings.HasSuffix(lines[1], "|2.00|"+b.Key+" Rename the flags|haiku") {
		t.Fatalf("launches:\n%s\nwant first %s", launches, wantA)
	}
	if len(*nexts) != 2 || !strings.Contains((*nexts)[0], "started this session for "+a.Key) {
		t.Errorf("wp next answers: %q", *nexts)
	}
	if got := bodies(); !strings.Contains(got, "started this session for "+a.Key) {
		t.Errorf("the package's kickoff was not noted as typed by Waypoint:\n%s", got)
	}
	if br := git(t, filepath.Join(s.repo, ".claude", "worktrees", a.Branch), "branch", "--show-current"); br != a.Branch {
		t.Errorf("worktree on %q", br)
	}
	if st := s.wp("fanout", "status", id); !strings.Contains(st, a.Key) || !strings.Contains(st, "running") {
		t.Errorf("status:\n%s", st)
	}

	t.Setenv("WP_TASK", a.Key)
	t.Setenv("WP_FANOUT", id)
	t.Setenv("WP_BUDGET_USD", "6.00")
	s.wp("next", a.Key)
	out = s.wp("done", a.Key, "-m", "split")
	if strings.Contains(out, "Stop here") {
		t.Fatalf("wp done in a package:\n%s", out)
	}
	if launches, _ = os.ReadFile(filepath.Join(rec, "launches")); strings.Count(string(launches), "\n") != 3 {
		t.Errorf("launches:\n%s", launches)
	}

	t.Setenv("WP_TASK", b.Key)
	t.Setenv("WP_BUDGET_USD", "2.00")
	sid := "bbbbbbbb-1111-2222-3333-444455556666"
	s.hook(in(sid, "SessionStart", s.repo, map[string]any{"source": "startup"}))
	cl, err := newCLI(&strings.Builder{}, &strings.Builder{})
	if err != nil {
		t.Fatal(err)
	}
	cl.withBurnState(sid, func(st *burnState) bool { st.BudgetSaid = true; return true })
	detached = nil
	s.hook(in(sid, "Stop", s.repo, map[string]any{"background_tasks": []any{}, "session_crons": []any{}}))
	if len(detached) != 2 || detached[0][0] != "stop" || strings.Join(detached[1], " ") != "fanout next "+id+" --ended "+b.Key+" --outcome budget" {
		t.Fatalf("detached at the budget stop: %v", detached)
	}
	detached = nil
	s.hook(in(sid, "SessionEnd", s.repo, map[string]any{"reason": "other"}))
	if len(detached) != 1 || strings.Join(detached[0], " ") != "fanout next "+id+" --ended "+b.Key+" --outcome ended" {
		t.Fatalf("detached at the end: %v", detached)
	}
	s.wp("fanout", "next", id, "--ended", b.Key, "--outcome", "budget", "--spent", "2.4")
	s.wp("fanout", "next", id, "--ended", b.Key, "--outcome", "ended", "--spent", "2.5")
	got, err := s.svc.GetFanout(context.Background(), s.me, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Items[0].State != core.ItemDone || got.Items[0].Spent != 0 || got.Items[1].State != core.ItemStopped || got.Items[1].Spent != 2.5 || got.Items[1].Note != "stopped at its budget" || got.Items[2].State != core.ItemRunning {
		t.Errorf("after: %+v %+v %+v", got.Items[0], got.Items[1], got.Items[2])
	}

	t.Setenv("WP_TASK", "")
	t.Setenv("WP_FANOUT", "")
	if out := s.wp("fanout", "stop", id); !strings.Contains(out, "nothing more starts") {
		t.Errorf("stop: %s", out)
	}
}

func TestFanoutBudgetLine(t *testing.T) {
	t.Setenv("WP_TASK", "WP-7")
	t.Setenv("WP_FANOUT", "3")
	t.Setenv("WP_BUDGET_USD", "10")
	st := &burnState{}
	if line, stop, ch := fanoutBudgetLine(st, 9.99); line != "" || stop != "" || ch {
		t.Errorf("below: %q %q", line, stop)
	}
	line, stop, ch := fanoutBudgetLine(st, 10.5)
	if !ch || stop != "" || !strings.Contains(line, "Budget reached: WP-7 (fan-out #3) has spent $10.50 of its $10.00") || !strings.Contains(line, "wp release WP-7") {
		t.Errorf("at the budget: %q %q", line, stop)
	}
	if line, _, ch := fanoutBudgetLine(st, 12); line != "" || ch {
		t.Errorf("said twice: %q", line)
	}
	if _, stop, ch := fanoutBudgetLine(st, 15); stop == "" || !ch || !st.BudgetStopped {
		t.Errorf("at 1.5 times: %q", stop)
	}
	t.Setenv("WP_FANOUT", "")
	if line, stop, _ := fanoutBudgetLine(&burnState{}, 100); line != "" || stop != "" {
		t.Error("outside a fan-out")
	}
}

func TestSessionEnvCarriesTheFanout(t *testing.T) {
	l := taskLaunch{tasks: []string{"WP-3"}, fanout: 4, budget: 7.5}
	env := strings.Join(l.sessionEnv([]string{"PATH=/bin", "WP_FANOUT=1", "WP_BUDGET_USD=99"}), " ")
	if env != "PATH=/bin WP_TASK=WP-3 WP_FANOUT=4 WP_BUDGET_USD=7.50" {
		t.Errorf("env = %q", env)
	}
	if env := strings.Join(taskLaunch{tasks: []string{"WP-3"}}.sessionEnv([]string{"WP_FANOUT=1", "WP_BUDGET_USD=99"}), " "); env != "WP_TASK=WP-3" {
		t.Errorf("env = %q", env)
	}
}
