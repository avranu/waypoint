package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

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
	f, err := e.svc.CreateForge(ctx, me, core.ForgeInput{Kind: ptr("github")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AddProjectRepo(ctx, me, p.Key, f.ID, "robin/report", false); err != nil {
		t.Fatal(err)
	}
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

// A value's check reads the number printed, by its unit; the exit code says
// whether the target is met. A fresh objective has no recorded base yet, so
// there is nothing to compare with (the base comparison is tested directly in
// TestMeasureObjectiveRunsSetupOnBothSides).
func TestObjectiveCheckReadsAValue(t *testing.T) {
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
	if l := o.Last; l.Value == nil || *l.Value != 25 || l.Met || l.Decides {
		t.Errorf("last check %+v (value %v)", l, l.Value)
	}
	if wt := gitDo(t, "worktree", "list"); strings.Count(wt, "\n") > 0 {
		t.Errorf("temporary worktree left behind:\n%s", wt)
	}

	write(t, "n.txt", "7\n")
	if out, code := e.run("objective", "check", key); code != 0 {
		t.Fatalf("7 against < 10: exit %d\n%s", code, out)
	}
	write(t, "n.txt", "none\n")
	if out, code := e.run("objective", "check", key); code != benchFailedCode || !strings.Contains(out, "none") {
		t.Errorf("no number: exit %d, want %d\n%s", code, benchFailedCode, out)
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

// A timed check runs this tree beside the commit the round started from,
// interleaved, the setup in each, and compares the two.
func TestObjectiveCheckTimesBesideTheBase(t *testing.T) {
	_, _, _ = objectiveEnv(t)
	write(t, "d.txt", "0.12\n")
	gitDo(t, "add", "-A")
	gitDo(t, "commit", "-q", "-m", "sleep")
	start := gitDo(t, "rev-parse", "HEAD")
	write(t, "d.txt", "0.002\n")
	gitDo(t, "commit", "-qam", "fast")
	c, err := newCLI(new(strings.Builder), new(strings.Builder))
	if err != nil {
		t.Fatal(err)
	}
	o := &objective{Check: "test -f built && sleep $(cat d.txt)", Setup: "touch built", Measure: "time", BaseSHA: start}
	r, err := measureObjective(c, o, objectiveLimits{Runs: 4, Timeout: time.Minute, Wait: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK || r.BaseOK == nil || !*r.BaseOK || r.Value == nil || r.BaseValue == nil || *r.Value >= *r.BaseValue || r.Runs < 3 || r.P == nil {
		t.Errorf("reading %+v (value %v, base %v)", r, r.Value, r.BaseValue)
	}
	if r.Moved {
		t.Error("the setup's own files were taken for a change made during the run")
	}
}

// wp ship doesn't measure: it says who reads the round, and is silent for a
// task with no objective in effect.
func TestObjectiveShipNoteMeasuresNothing(t *testing.T) {
	e, me, key := objectiveEnv(t)
	e.setObjective(me, key, core.ObjectiveInput{Check: ptr("cat n.txt"), Until: ptr("< 10")})
	ran := 0
	was := objectiveMeasure
	objectiveMeasure = func(c *cli, o *objective, lim objectiveLimits) (*objectiveReading, error) {
		ran++
		return was(c, o, lim)
	}
	t.Cleanup(func() { objectiveMeasure = was })

	var errOut strings.Builder
	c, err := newCLI(new(strings.Builder), &errOut)
	if err != nil {
		t.Fatal(err)
	}
	c.objectiveShipNote(key)
	if ran != 0 {
		t.Errorf("wp ship measured %d times", ran)
	}
	if !strings.Contains(errOut.String(), "Waypoint reads it itself") {
		t.Errorf("note: %q", errOut.String())
	}
	o, err := e.svc.GetObjective(context.Background(), me, key)
	if err != nil || o.Last != nil {
		t.Fatalf("%+v, %v", o, err)
	}
	other, err := e.svc.CreateIssue(context.Background(), me, core.IssueInput{Project: key[:strings.Index(key, "-")], Title: "Plain"})
	if err != nil {
		t.Fatal(err)
	}
	errOut.Reset()
	c.objectiveShipNote(other.Key)
	if errOut.Len() != 0 {
		t.Errorf("a plain task: %q", errOut.String())
	}
}

// The working reading says it decides nothing.
func TestObjectiveCheckSaysItDecidesNothing(t *testing.T) {
	e, me, key := objectiveEnv(t)
	e.setObjective(me, key, core.ObjectiveInput{Check: ptr("cat n.txt"), Until: ptr("< 10")})
	out, _ := e.run("objective", "check", key)
	if !strings.Contains(out, "Waypoint reads") || !strings.Contains(out, "decides it, not this one") {
		t.Errorf("output:\n%s", out)
	}
}

// The base a reading compares with is the one the server recorded, never a
// merge base: a branch that merged main in still reads against where the
// round started.
func TestObjectiveBaseIsTheServersNotAMergeBase(t *testing.T) {
	_, _, _ = objectiveEnv(t)
	start := gitDo(t, "rev-parse", "HEAD")
	write(t, "n.txt", "30\n")
	gitDo(t, "commit", "-qam", "work")
	c, err := newCLI(new(strings.Builder), new(strings.Builder))
	if err != nil {
		t.Fatal(err)
	}
	head := benchTree()
	if got := objectiveBase(c, &objective{Measure: "value", BaseSHA: start}, head); got != start {
		t.Errorf("base %q, want the recorded %q", got, start)
	}
	if got := objectiveBase(c, &objective{Measure: "pass", BaseSHA: start}, head); got != "" {
		t.Errorf("a pass compares with nothing: %q", got)
	}
	if got := objectiveBase(c, &objective{Measure: "value"}, head); got != "" {
		t.Errorf("no recorded base: %q", got)
	}
	if got := objectiveBase(c, &objective{Measure: "value", BaseSHA: gitDo(t, "rev-parse", "HEAD")}, head); got != "" {
		t.Errorf("the same tree: %q", got)
	}
}

// A value or a time reads the base with the setup run in it too.
func TestMeasureObjectiveRunsSetupOnBothSides(t *testing.T) {
	_, _, _ = objectiveEnv(t)
	start := gitDo(t, "rev-parse", "HEAD")
	write(t, "n.txt", "7\n")
	gitDo(t, "commit", "-qam", "work")
	c, err := newCLI(new(strings.Builder), new(strings.Builder))
	if err != nil {
		t.Fatal(err)
	}
	o := &objective{Check: "test -f built && echo n=$(cat n.txt)", Setup: "touch built", Measure: "value", BaseSHA: start}
	r, err := measureObjective(c, o, objectiveLimits{Runs: 3, Timeout: time.Minute, Wait: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK || r.Value == nil || *r.Value != 7 || r.BaseOK == nil || !*r.BaseOK || r.BaseValue == nil || *r.BaseValue != 40 || r.BaseSHA != start {
		t.Errorf("reading %+v (value %v, base %v)", r, r.Value, r.BaseValue)
	}
}

// A check that hangs is killed at --timeout instead of holding the slot.
func TestMeasureObjectiveTimesOut(t *testing.T) {
	_, _, _ = objectiveEnv(t)
	c, err := newCLI(new(strings.Builder), new(strings.Builder))
	if err != nil {
		t.Fatal(err)
	}
	o := &objective{Check: "sleep 300", Measure: "pass"}
	start := time.Now()
	_, err = measureObjective(c, o, objectiveLimits{Runs: 1, Timeout: 500 * time.Millisecond, Wait: time.Minute})
	var stop *measureStop
	if !errors.As(err, &stop) || !strings.Contains(stop.msg, "longer than") {
		t.Fatalf("err %v", err)
	}
	if time.Since(start) > 20*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestReadValue(t *testing.T) {
	for _, c := range []struct {
		out, unit string
		want      float64
	}{
		{"12", "", 12},
		{"coverage: 81.5% of lines\n", "", 81.5},
		{"a 3\nok 2 files, 1,204 ms\n", "", 1204},
		{"delta -0.5", "", -0.5},
		{"1e3 then 2.5e-1", "", 0.25},
		{"\x1b[32m81.3%\x1b[0m of 1204 statements", "%", 81.3},
		{"3 tests, took 12 ms", "ms", 12},
		{"took 12 MS", "ms", 12},
		{"42 Warnings", "warnings", 42},
		// Letters whose lower case is a different length, and bytes that
		// aren't text, sit before the number without moving where it is.
		{"\u0130\u0130\u0130\u0130 İstanbul 7 warnings", "warnings", 7},
		{"\xff\xfe\xff 9 ms", "ms", 9},
	} {
		if got := readValue(c.out, c.unit); got == nil || *got != c.want {
			t.Errorf("readValue(%q, %q) = %v, want %v", c.out, c.unit, got, c.want)
		}
	}
	if got := readValue("took 10-20 ms", "ms"); got == nil || *got < 0 {
		t.Errorf("10-20 ms read as %v: the dash is a range, not a sign", got)
	}
	if got := readValue("done\n", ""); got != nil {
		t.Error("no number was read as one")
	}
}

// The person's controls call the server, which refuses an agent: its words
// are what is printed.
func TestObjectiveGoActionsAreAPersonsToUse(t *testing.T) {
	e, me, key := objectiveEnv(t)
	e.setObjective(me, key, core.ObjectiveInput{Check: ptr("cat n.txt"), Until: ptr("< 10")})
	for _, a := range []string{"retry", "next", "accept", "end"} {
		out, code := e.run("objective", a, key)
		if code == 0 || strings.TrimSpace(out) == "" {
			t.Errorf("%s by an agent: exit %d, %q", a, code, out)
		}
	}
	if out, code := e.run("objective", "retry"); code == 0 || !strings.Contains(out, "which task") {
		t.Errorf("no key: exit %d %q", code, out)
	}
}

func TestObjectiveSetFlagsReachTheServer(t *testing.T) {
	e, me, key := objectiveEnv(t)
	e.wp("objective", "set", key, "--judge", "the report is quick and right", "--bar", "high", "--judge-model", "sonnet", "--judge-model", "haiku",
		"--keep-passing", "go test ./a, ./b", "--keep-passing", "make lint", "--branch", "feature", "--rev", "")
	o, err := e.svc.GetObjective(context.Background(), me, key)
	if err != nil {
		t.Fatal(err)
	}
	if o.Judge != "the report is quick and right" || o.JudgeBar != "high" || len(o.JudgeModels) != 2 || o.Branch != "feature" {
		t.Errorf("%+v", o)
	}
	if len(o.KeepPassing) != 2 || o.KeepPassing[0] != "go test ./a, ./b" {
		t.Errorf("keep-passing %q: a comma is part of a command", o.KeepPassing)
	}
	// A lone empty value clears the list.
	e.wp("objective", "set", key, "--keep-passing", "")
	if o, _ = e.svc.GetObjective(context.Background(), me, key); len(o.KeepPassing) != 0 {
		t.Errorf("not cleared: %q", o.KeepPassing)
	}
}

func TestNewWithAnObjective(t *testing.T) {
	e, me, key := objectiveEnv(t)
	project := key[:strings.Index(key, "-")]
	out := e.wp("new", "Cut the page weight", "--project", project, "--check", "cat n.txt", "--until", "< 10")
	made := regexp.MustCompile(project + `-\d+`).FindString(out)
	if made == "" {
		t.Fatalf("no key in:\n%s", out)
	}
	o, err := e.svc.GetObjective(context.Background(), me, made)
	if err != nil || o.Check != "cat n.txt" {
		t.Errorf("%+v, %v", o, err)
	}
	if out, code := e.run("new", "Alone", "--project", project, "--keep-passing", "true"); code == 0 {
		t.Errorf("--keep-passing alone: %s", out)
	}
}

// With every "tests" slot taken, wp objective check gives up after --wait
// with a message that says what it waited for.
func TestMeasureObjectiveSlotWaitTimesOut(t *testing.T) {
	_, _, _ = objectiveEnv(t)
	t.Setenv(slotHeldEnv, "")
	var holds []*slotHold
	for range defaultSlots() {
		h, err := acquireSlot(context.Background(), "tests", defaultSlots(), nil)
		if err != nil {
			t.Fatal(err)
		}
		holds = append(holds, h)
	}
	t.Cleanup(func() {
		for _, h := range holds {
			h.release()
		}
	})
	c, err := newCLI(new(strings.Builder), new(strings.Builder))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = measureObjective(c, &objective{Check: "true", Measure: "pass"}, objectiveLimits{Runs: 1, Timeout: time.Minute, Wait: 400 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "waited") || !strings.Contains(err.Error(), "--wait") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("gave up after %v", d)
	}
}

// A command that writes to both streams at once has them copied by two
// goroutines into the one tail that keeps everything (run with -race).
func TestRunCheckKeepsBothStreams(t *testing.T) {
	r, err := runCheck(context.Background(), t.TempDir(), os.Environ(),
		`for i in $(seq 1 400); do echo out$i; echo err$i >&2; done; echo 7 ms`, time.Minute)
	if err != nil || r.Code != 0 {
		t.Fatalf("run: %v, code %d", err, r.Code)
	}
	if n := strings.Count(r.All, "out"); n != 400 {
		t.Errorf("%d of 400 stdout lines kept", n)
	}
	if n := strings.Count(r.All, "err"); n != 400 {
		t.Errorf("%d of 400 stderr lines kept", n)
	}
	if strings.Contains(r.Stdout, "err") {
		t.Error("stderr reached what stdout is read from")
	}
}

// A side other than the head that runs past the timeout reads as failed,
// and the head's reading stands: a round that made a hung check finish is
// exactly the round to read.
func TestMeasureCheckLaterSideTimingOutIsAnAnswer(t *testing.T) {
	head, base := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(head, "d"), []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "d"), []byte("300\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, measure := range []string{"pass", "time"} {
		spec := measureSpec{Check: "sleep $(cat d)", Measure: measure, Timeout: time.Second, Budget: 20 * time.Second, MaxRuns: 3, Env: os.Environ()}
		reads, _, _, err := measureCheck(context.Background(), spec, []measureSide{{"the head", head}, {"the base", base}}, nil)
		if err != nil {
			t.Fatalf("%s: %v", measure, err)
		}
		if !reads[0].OK || reads[1].OK || !strings.Contains(reads[1].Output, "longer than") {
			t.Errorf("%s: head %+v, base %+v", measure, reads[0], reads[1])
		}
	}
}

// wp objective diagnose (WP-1449): what it asks for before the server is
// asked, and what the server says to a worker whose round isn't a diagnose round.
func TestObjectiveDiagnoseCommand(t *testing.T) {
	e, me, key := objectiveEnv(t)
	for name, args := range map[string][]string{
		"no key":       {"objective", "diagnose", "-m", "x"},
		"no text":      {"objective", "diagnose", key},
		"blank text":   {"objective", "diagnose", key, "-m", "  "},
		"both":         {"objective", "diagnose", key, "-m", "x", "-f", "d.md"},
		"missing file": {"objective", "diagnose", key, "-f", "nowhere.md"},
	} {
		if out, code := e.run(args...); code == 0 || strings.TrimSpace(out) == "" {
			t.Errorf("%s: exit %d, %q", name, code, out)
		}
	}
	// No objective yet.
	if out, code := e.run("objective", "diagnose", key, "-m", "x"); code == 0 || !strings.Contains(out, "no objective") {
		t.Errorf("no objective: exit %d, %q", code, out)
	}
	e.setObjective(me, key, core.ObjectiveInput{Check: ptr("cat n.txt"), Until: ptr("< 10")})
	// The server's own words say the round isn't a diagnose round, from a file too.
	write(t, "d.md", "# Why\n\nThe check measures the wrong file.\n")
	for _, args := range [][]string{
		{"objective", "diagnose", key, "-m", "the check is wrong", "--ask"},
		{"objective", "diagnose", key, "-f", "d.md"},
	} {
		if out, code := e.run(args...); code == 0 || !strings.Contains(out, "diagnose") {
			t.Errorf("%v: exit %d, %q", args, code, out)
		}
	}
	// What wp objective set and wp objective diagnose say in help.
	if out := e.wp("help", "objective", "diagnose"); !strings.Contains(out, "--ask") || !strings.Contains(out, "-f FILE") {
		t.Errorf("help:\n%s", out)
	}
}

func TestObjectiveDiagnoseAfterFlag(t *testing.T) {
	e, me, key := objectiveEnv(t)
	e.wp("objective", "set", key, "--check", "cat n.txt", "--until", "< 10", "--diagnose-after", "4")
	o, err := e.svc.GetObjective(context.Background(), me, key)
	if err != nil || o.DiagnoseAfter != 4 {
		t.Fatalf("diagnose_after = %+v, %v", o, err)
	}
	e.wp("objective", "set", key, "--diagnose-after", "0")
	if o, _ = e.svc.GetObjective(context.Background(), me, key); o.DiagnoseAfter != 0 {
		t.Errorf("diagnose_after = %d, want 0 (never)", o.DiagnoseAfter)
	}
	if out, code := e.run("objective", "set", key, "--diagnose-after", "99"); code == 0 {
		t.Errorf("99: %s", out)
	}
	project := key[:strings.Index(key, "-")]
	if out, code := e.run("new", "Alone", "--project", project, "--diagnose-after", "3"); code == 0 || !strings.Contains(out, "--diagnose-after") {
		t.Errorf("--diagnose-after alone: exit %d %s", code, out)
	}
	out := e.wp("new", "Cut it", "--project", project, "--check", "cat n.txt", "--until", "< 10", "--diagnose-after", "3")
	made := regexp.MustCompile(project + `-\d+`).FindString(out)
	if o, err = e.svc.GetObjective(context.Background(), me, made); err != nil || o.DiagnoseAfter != 3 {
		t.Errorf("new: %+v, %v", o, err)
	}
}
