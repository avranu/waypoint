package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// objectiveEnv is a server with one task, a git checkout whose origin/main
// is its first commit, and the CLI logged in (as an agent's token).
func objectiveEnv(t *testing.T) (*env, *core.Actor, string) {
	t.Helper()
	e := newEnv(t)
	me := e.login()
	t.Setenv("WAYPOINT_SLOT_DIR", t.TempDir())
	ctx := context.Background()
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Objectives")})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAYPOINT_PROJECT", p.Key)
	i, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "Make the report fast"})
	if err != nil {
		t.Fatal(err)
	}
	tempRepo(t)
	write(t, "n.txt", "40\n")
	gitDo(t, "add", "-A")
	gitDo(t, "commit", "-q", "-m", "slow")
	gitDo(t, "update-ref", "refs/remotes/origin/main", "HEAD")
	return e, me, i.Key
}

func (e *env) setObjective(me *core.Actor, key string, in core.ObjectiveInput) {
	e.t.Helper()
	if _, err := e.svc.SetObjective(context.Background(), me, key, in); err != nil {
		e.t.Fatal(err)
	}
}

// An agent's wp objective set is a proposal: shown as one, never checked
// (its command is an agent's text), until a person confirms it.
func TestObjectiveSetByAnAgentIsAProposalNothingRuns(t *testing.T) {
	e, me, key := objectiveEnv(t)
	out := e.wp("objective", "set", key, "--check", "touch ran; cat n.txt", "--until", "< 10", "--rounds", "4")
	if !strings.Contains(out, "roposed") {
		t.Errorf("set by an agent should say it is a proposal:\n%s", out)
	}
	out, code := e.run("objective", "check", key)
	if code == 0 || !strings.Contains(out, "confirm") {
		t.Errorf("check of a proposal: exit %d\n%s", code, out)
	}
	if _, err := os.Stat("ran"); err == nil {
		t.Error("a proposal's check ran")
	}
	if out, code := e.run("objective", "confirm", key); code == 0 {
		t.Errorf("an agent confirmed its own proposal:\n%s", out)
	}
	ctx := context.Background()
	proposed, err := e.svc.GetObjective(ctx, me, key)
	if err != nil {
		t.Fatal(err)
	}
	o, err := e.svc.ConfirmObjective(ctx, me, key, proposed.Rev)
	if err != nil || o.State != core.ObjectiveActive || o.MaxRounds != 4 || o.Measure != core.MeasureValue {
		t.Fatalf("confirmed: %+v, %v", o, err)
	}
	if out := e.wp("objective", "ls"); !strings.Contains(out, key) {
		t.Errorf("ls misses %s:\n%s", key, out)
	}
	if out := e.wp("objective", "show", key); !strings.Contains(out, "cat n.txt") {
		t.Errorf("show misses the check:\n%s", out)
	}
}

// A value's check reads the last number printed here and on the commit the
// branch started from; the exit code says whether the target is met.
func TestObjectiveCheckReadsAValueBesideTheBase(t *testing.T) {
	e, me, key := objectiveEnv(t)
	e.setObjective(me, key, core.ObjectiveInput{Check: ptr("echo items: $(cat n.txt)"), Until: ptr("< 10")})

	write(t, "n.txt", "25\n")
	out, code := e.run("objective", "check", key)
	if code != 1 {
		t.Fatalf("25 against < 10: exit %d, want 1\n%s", code, out)
	}
	o, err := e.svc.GetObjective(context.Background(), me, key)
	if err != nil || o.Last == nil {
		t.Fatalf("%+v, %v", o, err)
	}
	if l := o.Last; l.Value == nil || *l.Value != 25 || l.BaseValue == nil || *l.BaseValue != 40 || !l.Improved || l.Met {
		t.Errorf("last check %+v (value %v, base %v)", l, l.Value, l.BaseValue)
	}
	if wt := gitDo(t, "worktree", "list"); strings.Count(wt, "\n") > 0 {
		t.Errorf("temporary worktree left behind:\n%s", wt)
	}

	write(t, "n.txt", "7\n")
	if out, code := e.run("objective", "check", key); code != 0 {
		t.Fatalf("7 against < 10: exit %d\n%s", code, out)
	}
	write(t, "n.txt", "none\n")
	if out, code := e.run("objective", "check", key); code != 1 || !strings.Contains(out, "no number") {
		t.Errorf("no number: exit %d\n%s", code, out)
	}
}

// A pass objective is met when the command exits 0, run in its folder; a
// failing command exits 3 with the end of its output.
func TestObjectiveCheckPassRunsInItsFolder(t *testing.T) {
	e, me, key := objectiveEnv(t)
	if err := os.Mkdir("sub", 0o755); err != nil {
		t.Fatal(err)
	}
	e.setObjective(me, key, core.ObjectiveInput{Check: ptr("echo looking; test -f here"), Dir: ptr("sub"), Until: ptr("pass")})
	out, code := e.run("objective", "check", key)
	if code != benchFailedCode || !strings.Contains(out, "looking") {
		t.Fatalf("failing check: exit %d, want %d\n%s", code, benchFailedCode, out)
	}
	write(t, filepath.Join("sub", "here"), "")
	if out, code := e.run("objective", "check", key); code != 0 {
		t.Fatalf("passing check: exit %d\n%s", code, out)
	}
}

// A timed check is wp bench's: this tree beside the base, a change the
// runs can tell from noise.
func TestObjectiveCheckTimesBesideTheBase(t *testing.T) {
	e, me, key := objectiveEnv(t)
	write(t, "d.txt", "0.08\n")
	gitDo(t, "add", "-A")
	gitDo(t, "commit", "-q", "-m", "sleep")
	gitDo(t, "update-ref", "refs/remotes/origin/main", "HEAD")
	e.setObjective(me, key, core.ObjectiveInput{Check: ptr("test -f built && sleep $(cat d.txt)"), Setup: ptr("touch built"), Until: ptr("2x")})

	write(t, "d.txt", "0.001\n")
	out, code := e.run("objective", "check", "-n", "4", key)
	if code != 0 {
		t.Fatalf("80ms to 1ms against 2x: exit %d\n%s", code, out)
	}
	o, err := e.svc.GetObjective(context.Background(), me, key)
	if err != nil || o.Last == nil {
		t.Fatalf("%+v, %v", o, err)
	}
	if l := o.Last; !l.Met || !l.Improved || l.BaseValue == nil || l.Value == nil || *l.Value >= *l.BaseValue || l.Runs != 4 {
		t.Errorf("last check %+v", l)
	}
}

// wp ship measures the round's tree itself when the agent hasn't, and
// leaves a tree already checked this round alone.
func TestObjectiveBeforeShipChecksAnUncheckedTree(t *testing.T) {
	e, me, key := objectiveEnv(t)
	e.setObjective(me, key, core.ObjectiveInput{Check: ptr("cat n.txt"), Until: ptr("< 10")})
	ran := 0
	was := objectiveMeasure
	objectiveMeasure = func(c *cli, o *objective, runs int) (*objectiveReading, error) {
		ran++
		return was(c, o, runs)
	}
	t.Cleanup(func() { objectiveMeasure = was })

	c, err := newCLI(new(strings.Builder), new(strings.Builder))
	if err != nil {
		t.Fatal(err)
	}
	c.objectiveBeforeShip(key)
	c.objectiveBeforeShip(key) // the same tree: nothing to add
	if ran != 1 {
		t.Errorf("checked %d times, want 1", ran)
	}
	write(t, "n.txt", "3\n")
	c.objectiveBeforeShip(key)
	if ran != 2 {
		t.Errorf("a changed tree: checked %d times, want 2", ran)
	}
	o, err := e.svc.GetObjective(context.Background(), me, key)
	if err != nil || o.Last == nil || !o.Last.Met {
		t.Fatalf("%+v, %v", o, err)
	}
	// A task with no objective: silence.
	other, err := e.svc.CreateIssue(context.Background(), me, core.IssueInput{Project: o.Key[:strings.Index(o.Key, "-")], Title: "Plain"})
	if err != nil {
		t.Fatal(err)
	}
	c.objectiveBeforeShip(other.Key)
	if ran != 2 {
		t.Errorf("a plain task was checked")
	}
}

func TestLastNumber(t *testing.T) {
	for in, want := range map[string]float64{
		"12":                          12,
		"coverage: 81.5% of lines\n":  81.5,
		"a 3\nok 2 files, 1,204 ms\n": 1204,
		"delta -0.5":                  -0.5,
		"1e3 then 2.5e-1":             0.25,
	} {
		if got := lastNumber(in); got == nil || *got != want {
			t.Errorf("lastNumber(%q) = %v, want %v", in, got, want)
		}
	}
	if got := lastNumber("done\n"); got != nil {
		t.Errorf("no number: %v", *got)
	}
}
