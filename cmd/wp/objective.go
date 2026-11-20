package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Objectives (WP-927, WP-ADR111): a task worked in rounds until a check is
// met ("until this runs in under a second"), by an amount every round
// ("twice as fast"), or as a loop. The server keeps the objective and
// decides what a round's end does; wp runs the check here, on this tree and
// beside it on the commit the branch started from, and reports what it read.

var objectiveCmd = &command{
	name: "objective", group: "Agent work", args: "[set|show|check|ls|confirm|clear …]",
	summary: "work a task in rounds: until a check is met, by an amount every round, or as a loop",
	examples: []string{
		"wp objective set WP-12 --check 'go test -count=1 ./internal/core' --until '< 1s'",
		"wp objective set WP-12 --check ./bench-search --until 2x --pace spare",
		"wp objective check WP-12",
		"wp objective ls",
	},
	about: `A task with an objective isn't finished by one pull request. Each round
ships what it gained (wp ship, as usual); where the task would close, the
objective decides: met, it closes; otherwise it goes back to To Do for the
next round. A person closing the task themselves always closes it.

  until a fixed target   --until '< 1s'  '<= 250ms'  '>= 80%'  '< 10 warnings'  pass
                         Closes when a round's last check meets it.
  an amount every round  --until 2x  -10%  +5%
                         Judged against the commit the round's branch started
                         from, so it is never met for good: it asks again
                         every round, until a person closes the task.
  a loop                 no --check and no --until: every round opens the
                         task again, until a person closes it or --rounds run out.

The check is a shell command agents' machines run, so an objective an agent
sets is a proposal, shown but not run and not looping, until a person
confirms it (wp objective confirm KEY in their own terminal, or on the
task's page), and only a person changes one in effect.`,
	subs: []*command{
		{name: "set", args: "KEY [--check 'CMD'] [--until TARGET] [--dir DIR] [--setup 'CMD'] [--measure time|value|pass] [--better lower|higher] [--pace queue|spare] [--every DURATION] [--rounds N]",
			summary: "give a task an objective, or change it (a flag left out keeps its value)",
			about: `The target decides what is measured: a time ("< 1s", 2x, -10%) is the
command's median wall time over several runs, as wp bench measures it; any
other number (">= 80%", "+5%") is the last number on the command's last
line of output; pass is its exit status. --measure value reads a number
where the target alone would mean a time (--until 2x --measure value
--better higher).

--pace spare hands the task out only while your week's-end plan wants more
work ("Use what's left" in autopilot's settings), so its rounds run on
quota that would otherwise go unused; wp next KEY still takes it by name.
--every is the least time between rounds (default 24h for the shapes that
never end, 0 for a fixed target). A round that moved nothing backs the task
off by itself: an hour, doubling to a week.`,
			examples: []string{
				"wp objective set WP-12 --check 'go test -count=1 ./internal/core' --until '< 1s'",
				"wp objective set WP-12 --check ./scripts/coverage.sh --until '>= 80%'",
				"wp objective set WP-12 --check 'make lint' --until pass",
				"wp objective set WP-12 --check ./bench-search --until 2x --pace spare --every 12h",
				"wp objective set WP-12 --every 24h --rounds 30",
			},
			run: cmdObjectiveSet},
		{name: "show", args: "[KEY]", summary: "a task's objective: its rounds and last checks",
			examples: []string{"wp objective show WP-12", "wp objective show WP-12 --json"},
			run:      cmdObjectiveShow},
		{name: "check", args: "[KEY] [-n RUNS]", summary: "run the check on this tree (and its base commit) and record it; exit 1 when not met",
			about: `Runs the objective's command in this checkout and, when the tree differs
from the commit the branch started from, on that commit too in a temporary
worktree, then records both against the round under way. wp ship does this
before it ships when this tree hasn't been checked. Exit 0: met; 1: not
met; 3: the command failed.`,
			examples: []string{"wp objective check", "wp objective check WP-12 -n 9"},
			run:      cmdObjectiveCheck},
		{name: "ls", args: "[--project KEY] [--all]", summary: "the objectives in effect or proposed (--all: met and ended too)",
			examples: []string{"wp objective ls", "wp objective ls --project WP --all"},
			run:      cmdObjectiveLs},
		{name: "confirm", args: "KEY", summary: "put an agent's proposed objective in effect (a person, in their own terminal)",
			examples: []string{"wp objective confirm WP-12"},
			run:      cmdObjectiveConfirm},
		{name: "clear", args: "KEY", summary: "take the objective off: an ordinary task again",
			examples: []string{"wp objective clear WP-12"},
			run:      cmdObjectiveClear},
	},
}

// objective is what the CLI reads of a task's objective.
type objective struct {
	Key     string `json:"key"`
	Check   string `json:"check"`
	Dir     string `json:"dir"`
	Setup   string `json:"setup"`
	Measure string `json:"measure"`
	Until   string `json:"until"`
	Round   int    `json:"round"`
	State   string `json:"state"`
	Text    string `json:"text"`
	Last    *struct {
		Round int    `json:"round"`
		Tree  string `json:"tree"`
	} `json:"last"`
}

// objectiveReading is what one check read; the server's ObjectiveCheckInput.
type objectiveReading struct {
	Agent     string   `json:"agent,omitempty"`
	OK        bool     `json:"ok"`
	Value     *float64 `json:"value,omitempty"`
	BaseOK    *bool    `json:"base_ok,omitempty"`
	BaseValue *float64 `json:"base_value,omitempty"`
	BaseSHA   string   `json:"base_sha,omitempty"`
	Tree      string   `json:"tree,omitempty"`
	Commit    string   `json:"commit,omitempty"`
	Runs      int      `json:"runs,omitempty"`
	Low       *float64 `json:"low,omitempty"`
	High      *float64 `json:"high,omitempty"`
	P         *float64 `json:"p,omitempty"`
	Moved     bool     `json:"moved,omitempty"`
}

func cmdObjectiveSet(c *cli, args []string) error {
	fs := c.flags()
	check := fs.String("check", "", "shell `COMMAND` that measures it, run in the repository")
	until := fs.String("until", "", "`TARGET`: '< 1s', '>= 80%', pass, 2x, -10%, +5%")
	dir := fs.String("dir", "", "`DIR` within the repository the command runs in")
	setup := fs.String("setup", "", "shell `COMMAND` run once before a timed check (not timed)")
	measure := fs.String("measure", "", "`MEASURE`: time, value or pass, when the target alone doesn't say")
	better := fs.String("better", "", "`WAY` a value improves: lower or higher")
	pace := fs.String("pace", "", "`PACE`: queue (like any task) or spare (only on quota that would go unused)")
	every := fs.String("every", "", "least `DURATION` between rounds (0, 90m, 24h, 3d)")
	rounds := fs.Int("rounds", 0, "end after `N` rounds (0: no limit)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	key, rest, err := c.currentKey(pos)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return c.usage("unexpected %q: the command goes in --check 'CMD', the target in --until", joinArgs(rest))
	}
	body := map[string]any{}
	vals := map[string]any{"check": *check, "until": *until, "dir": *dir, "setup": *setup, "measure": *measure, "better": *better, "pace": *pace, "every": *every}
	fs.Visit(func(f *flag.Flag) {
		switch {
		case f.Name == "rounds":
			body["max_rounds"] = *rounds
		case vals[f.Name] != nil:
			body[f.Name] = vals[f.Name]
		}
	})
	return c.objectiveWrite("PUT", "/api/v1/issues/"+esc(key)+"/objective", body)
}

// objectiveWrite sends a change and prints the objective as it now stands.
func (c *cli) objectiveWrite(method, path string, body any) error {
	if !c.asJSON {
		path += "?format=text"
	}
	if body == nil {
		body = map[string]any{}
	}
	raw, err := c.call(method, path, body)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string { return string(raw) })
}

func cmdObjectiveShow(c *cli, args []string) error {
	pos, err := c.parse(c.flags(), args)
	if err != nil {
		return err
	}
	key, rest, err := c.currentKey(pos)
	if err != nil {
		return err
	}
	return c.getDiag(rest, "/api/v1/issues/"+esc(key)+"/objective", nil)
}

func cmdObjectiveLs(c *cli, args []string) error {
	fs := c.flags()
	project := fs.String("project", "", "`KEY`: only this project's")
	all := fs.Bool("all", false, "met and ended ones too")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	q := url.Values{}
	setIf(q, "project", *project)
	if *all {
		q.Set("all", "1")
	}
	return c.getDiag(pos, "/api/v1/objectives", q)
}

func cmdObjectiveConfirm(c *cli, args []string) error {
	pos, err := c.parse(c.flags(), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("say which task: wp objective confirm KEY")
	}
	// The proposal as it stands is the one confirmed: the server refuses
	// if an agent writes another over it in between.
	path := "/api/v1/issues/" + esc(normalKey(pos[0])) + "/objective"
	raw, err := c.call("GET", path, nil)
	if err != nil {
		return err
	}
	var o struct {
		Rev string `json:"rev"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return fmt.Errorf("the objective: %w", err)
	}
	return c.objectiveWrite("POST", path+"/confirm", map[string]any{"rev": o.Rev})
}

func cmdObjectiveClear(c *cli, args []string) error {
	pos, err := c.parse(c.flags(), args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("say which task: wp objective clear KEY")
	}
	key := normalKey(pos[0])
	if _, err := c.call("DELETE", "/api/v1/issues/"+esc(key)+"/objective", nil); err != nil {
		return err
	}
	c.println(key + " has no objective now: it is an ordinary task.")
	return nil
}

func cmdObjectiveCheck(c *cli, args []string) error {
	fs := c.flags()
	runs := fs.Int("n", objectiveRuns, "timed `RUNS` per tree, for a time")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	key, rest, err := c.currentKey(pos)
	if err != nil {
		return err
	}
	switch {
	case len(rest) > 0:
		return c.usage("unexpected %q: the check is the objective's own command", joinArgs(rest))
	case *runs < 1 || *runs > benchMaxRuns:
		return c.usage("-n is 1 to %d", benchMaxRuns)
	}
	var o objective
	if err := c.do("GET", "/api/v1/issues/"+esc(key)+"/objective", nil, &o); err != nil {
		return err
	}
	switch {
	case o.State == "proposed":
		return fmt.Errorf("%s's objective is an agent's proposal, so its check isn't run: a person confirms it first (wp objective confirm %s in their own terminal, or on the task's page)", o.Key, o.Key)
	case o.State != "active":
		return fmt.Errorf("%s's objective is %s: there is no round to check", o.Key, o.State)
	case o.Check == "":
		return fmt.Errorf("%s's objective is a loop: it has no check. wp ship ends the round", o.Key)
	}
	met, ok, err := c.objectiveCheck(&o, *runs)
	switch {
	case err != nil:
		return err
	case !ok:
		return exitCode(benchFailedCode)
	case !met:
		return exitCode(1)
	}
	return nil
}

// objectiveRuns is the timed runs a tree of a check, as wp bench's.
const objectiveRuns = 5

// objectiveMeasure measures one objective here; tests replace it.
var objectiveMeasure = measureObjective

// objectiveCheck runs the objective's check here, records it and prints the
// server's verdict. ok is false when the command failed on this tree.
func (c *cli) objectiveCheck(o *objective, runs int) (met, ok bool, err error) {
	r, err := objectiveMeasure(c, o, runs)
	if err != nil {
		return false, false, err
	}
	r.Agent = c.agent
	var res struct {
		Met     bool   `json:"met"`
		Message string `json:"message"`
	}
	raw, err := c.call("POST", "/api/v1/issues/"+esc(o.Key)+"/objective/checks", r)
	if err != nil {
		return false, false, err
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return false, false, err
	}
	if err := c.emit(raw, func() string { return res.Message }); err != nil {
		return false, false, err
	}
	return res.Met, r.OK, nil
}

// measureObjective runs the check in this checkout and, when the tree has
// changed since the commit the branch started from, on that commit too.
func measureObjective(c *cli, o *objective, runs int) (*objectiveReading, error) {
	root, err := localGit("rev-parse", "--show-toplevel")
	if err != nil || root == "" {
		return nil, fmt.Errorf("the check runs in the task's repository: run this in its checkout")
	}
	dir := filepath.Join(root, filepath.FromSlash(o.Dir))
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("the objective's folder %s isn't in this checkout", o.Dir)
	}
	was, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if err := os.Chdir(dir); err != nil {
		return nil, err
	}
	defer func() { _ = os.Chdir(was) }()

	r := &objectiveReading{Tree: benchTree(), Runs: 1}
	r.Commit, _ = localGit("rev-parse", "HEAD")
	// The base: where this branch left the default one. With nothing
	// changed since, there is nothing to compare.
	base := ""
	if ref := baseRef(""); ref != "" {
		if sha, err := localGit("merge-base", ref, "HEAD"); err == nil && sha != "" {
			if tree, err := localGit("rev-parse", sha+"^{tree}"); err == nil && tree != r.Tree {
				base = sha
			}
		}
	}
	if base != "" && o.Dir != "" {
		// A folder the base doesn't have: the check can't run there.
		if _, err := localGit("rev-parse", "--verify", "-q", base+":"+o.Dir); err != nil {
			no := false
			r.BaseOK, r.BaseSHA, base = &no, base, ""
		}
	}
	if o.Measure == "time" {
		return r, c.timeObjective(o, r, base, runs)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	env, release, err := c.takeSlot(ctx, "tests", defaultSlots())
	if err != nil {
		return nil, err
	}
	defer release()
	read := func(dir string) (bool, *float64, error) {
		ok, out, tail, err := objectiveExec(ctx, dir, env, o.Check)
		if err != nil {
			return false, nil, err
		}
		if !ok {
			_, _ = fmt.Fprintf(c.err, "The check failed in %s:\n%s\n", dir, outputTail(tail, benchTailLines))
			return false, nil, nil
		}
		if o.Measure != "value" {
			return true, nil, nil
		}
		v := lastNumber(out)
		if v == nil {
			_, _ = fmt.Fprintf(c.err, "The check printed no number on its last line in %s: %q\n", dir, lastLine(out))
		}
		return true, v, nil
	}
	if r.OK, r.Value, err = read(dir); err != nil {
		return nil, err
	}
	if base != "" {
		baseDir, cleanup, sha, err := baseWorktree(base)
		if err != nil {
			_, _ = fmt.Fprintf(c.err, "Notice: the base commit %s couldn't be checked out (%v), so this check has nothing to compare with.\n", shortSHA(base), err)
		} else {
			defer cleanup()
			ok, v, err := read(baseDir)
			if err != nil {
				return nil, err
			}
			r.BaseOK, r.BaseValue, r.BaseSHA = &ok, v, sha
		}
	}
	if end := benchTree(); r.Tree != "" && end != "" && end != r.Tree {
		r.Moved = true
	}
	return r, nil
}

// timeObjective times the check as wp bench does, beside the base when
// there is one. A base that can't run the command (it didn't exist there)
// leaves this tree's time alone.
func (c *cli) timeObjective(o *objective, r *objectiveReading, base string, runs int) error {
	bench := func(base string) (*benchResult, error) {
		var out bytes.Buffer
		sub := *c
		sub.out, sub.asJSON, sub.cur = &out, true, benchCmd
		args := []string{"--json", "-n", strconv.Itoa(runs)}
		if base != "" {
			args = append(args, "--base", base)
		}
		if o.Setup != "" {
			args = append(args, "--setup", o.Setup)
		}
		err := cmdBench(&sub, append(args, "--", "sh", "-c", o.Check))
		var code exitCode
		if errors.As(err, &code) && int(code) == benchFailedCode {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		var res benchResult
		if err := json.Unmarshal(out.Bytes(), &res); err != nil {
			return nil, fmt.Errorf("wp bench's result: %w: %s", err, firstLine(out.String()))
		}
		return &res, nil
	}
	res, err := bench(base)
	if err != nil {
		return err
	}
	if res == nil && base != "" {
		no := false
		r.BaseOK, r.BaseSHA = &no, base
		if res, err = bench(""); err != nil {
			return err
		}
	}
	if res == nil {
		return nil // failed here: ok stays false
	}
	r.OK, r.Runs, r.Moved = true, res.Runs, res.Moved
	r.Value, r.Low, r.High = &res.Head.Wall.Median, &res.Head.Wall.Low, &res.Head.Wall.High
	if res.Head.Tree != "" {
		r.Tree = res.Head.Tree
	}
	if res.Base != nil {
		yes := true
		r.BaseOK, r.BaseValue, r.BaseSHA = &yes, &res.Base.Wall.Median, base
		if res.Change != nil {
			r.P = res.Change.WallP
		}
	}
	return nil
}

// objectiveExec runs the check once: whether it exited 0, what it printed,
// and the end of everything it wrote.
func objectiveExec(ctx context.Context, dir string, env []string, check string) (ok bool, stdout string, tail []byte, err error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", check) // #nosec G204 G702 -- running the objective's command is the point
	setProcGroup(cmd)
	killOnCancel(cmd)
	cmd.Dir, cmd.Env = dir, env
	var out, all tailBuffer
	cmd.Stdout, cmd.Stderr = &teeTail{&out, &all}, &all
	runErr := cmd.Run()
	procExited(cmd)
	killGroup(cmd)
	if ctx.Err() != nil {
		return false, "", nil, ctx.Err()
	}
	var ee *exec.ExitError
	switch {
	case runErr == nil:
		return true, out.String(), all.b, nil
	case errors.As(runErr, &ee):
		return false, out.String(), all.b, nil
	}
	return false, "", nil, fmt.Errorf("sh: %w", runErr)
}

// teeTail writes to two tails.
type teeTail struct{ a, b *tailBuffer }

func (t *teeTail) Write(p []byte) (int, error) {
	_, _ = t.a.Write(p)
	return t.b.Write(p)
}

func outputTail(b []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// numberRE reads "1,204" as one number: a summary line's thousands.
var numberRE = lazyre.New(`-?(?:[0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)(?:\.[0-9]+)?(?:[eE][-+]?[0-9]+)?`)

// lastNumber is the last number on the last line that has text: what a
// check for a value prints ("coverage: 81.3% of statements" reads 81.3).
func lastNumber(out string) *float64 {
	all := numberRE.FindAllString(strings.TrimSpace(lastLine(out)), -1)
	if len(all) == 0 {
		return nil
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(all[len(all)-1], ",", ""), 64)
	if err != nil {
		return nil
	}
	return &v
}

// objectiveBeforeShip measures a task's objective before its round ships,
// when this tree hasn't been checked in the round: the round's end is
// judged by its last check. It never stops the ship: a failure is said and
// the round ends unmeasured. It says whether it printed anything: the
// check's result, or why there is none.
func (c *cli) objectiveBeforeShip(key string) (said bool) {
	var o objective
	err := c.do("GET", "/api/v1/issues/"+esc(key)+"/objective", nil, &o)
	if err != nil || o.State != "active" || o.Check == "" {
		var ae *apiError
		if err != nil && (!errors.As(err, &ae) || ae.status != http.StatusNotFound) {
			_, _ = fmt.Fprintf(c.err, "Notice: %s's objective couldn't be read (%v), so this round ships unchecked.\n", key, err)
			return true
		}
		return false
	}
	if root, err := localGit("rev-parse", "--show-toplevel"); err != nil || root == "" {
		return false
	}
	if tree := benchTree(); tree != "" && o.Last != nil && o.Last.Round == o.Round && o.Last.Tree == tree {
		return false // this tree's check is the round's last
	}
	_, _ = fmt.Fprintf(c.err, "Checking %s's objective on this tree before it ships (%s)…\n", o.Key, o.Check)
	if _, _, err := c.objectiveCheck(&o, objectiveRuns); err != nil {
		_, _ = fmt.Fprintf(c.err, "Notice: the objective's check didn't run (%v), so this round ships unchecked. wp objective check %s runs it.\n", err, o.Key)
	}
	return true
}
