package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Objectives (WP-927, WP-ADR111, WP-ADR142): a task worked in rounds until a check is
// met, an amount better every round, a judge finds nothing left, or as a
// loop. Waypoint reads every round itself, on a person's runner in fresh
// checkouts; what wp objective check reads here is the worker's own working
// reading, kept and shown, and it decides nothing.

var objectiveCmd = &command{
	name: "objective", group: "Agent work", args: "[set|show|check|ls|confirm|clear|retry|next|accept|end …]",
	summary: "work a task in rounds until Waypoint's own reading says it is done: a check, an amount, a judge, or a loop",
	examples: []string{
		"wp objective set WP-12 --check 'go test -count=1 ./internal/core' --until '< 1s'",
		"wp objective set WP-12 --check ./bench-search --until 2x --pace spare",
		"wp objective set WP-12 --judge 'every error path in internal/api says what to do next' --bar high",
		"wp objective check WP-12",
		"wp objective ls",
	},
	about: `A task with an objective isn't finished by one pull request. Each round
ships what it gained (wp ship, as usual); where the task would close,
Waypoint reads the round itself: its commands run on the runner of the person
who put the objective in effect, in fresh checkouts of the branch (and, for a
time or a value, of the commit the round started from, in the same run). Met,
the task closes; otherwise it goes back to To Do for the next round. What
the worker reads with wp objective check never decides it, and a person
closing or cancelling the task always ends it.

  until a fixed target   --until '< 1s'  '<= 250ms'  '>= 80%'  '< 10 warnings'  pass
                         Met when Waypoint's reading meets it.
  an amount every round  --until 2x  -10%  +5%
                         Against the commit the round started from, so it is
                         never met for good: it asks again every round.
  judged                 --judge 'TEXT' [--bar high] [--judge-model M]
                         Other models, not the worker, read the branch against
                         your words. Met when they find nothing at --bar or
                         above (critical, high, medium by default, low).
  a loop                 no --check, --until or --judge: every round opens the
                         task again, until a person ends it or --rounds run out.

--keep-passing CMD (repeatable) names a command that must keep passing: a
round that breaks it hasn't met the objective, whatever the check reads.

The check and those commands are shell commands a runner runs, so an objective
an agent sets is a proposal, shown but not run, until a person confirms it
(wp objective confirm KEY in their own terminal, or on the task's page); only a
person changes one in effect. Where the repository is read and which branch:
--repo owner/name, --branch.

A round that moved nothing waits before the next: an hour, doubling to a week,
and never less than --every (24h by default for a loop or an amount every
round, so those wait a day). It stops and says so (wp objective show KEY) when
a person is needed: no repository or runner to read it on, a check that gives no
reading at the start, a round that changed the files its own check runs (its
reading waits for you), a reading it couldn't take after retries, or no rounds
left. A person steers it: wp objective retry KEY (read again), next (move on
now), accept (count the reading that waits), end (an ordinary task again).
Agents are refused.`,
	subs: []*command{
		{name: "set", args: "KEY [--check 'CMD'] [--until TARGET] [--judge 'TEXT' [--bar LEVEL] [--judge-model M]] [--keep-passing 'CMD'] [--dir DIR] [--setup 'CMD'] [--measure time|value|pass] [--better lower|higher] [--repo owner/name] [--branch B] [--pace queue|spare] [--every DURATION] [--rounds N] [--rev REV]",
			summary: "give a task an objective, or change it (a flag left out keeps its value)",
			about: `The target decides what is measured: a time ("< 1s", 2x, -10%) is the
command's median wall time over several runs, as wp bench measures it; any
other number (">= 80%", "+5%") is a number the command prints (a line with
several is read by the unit in the target: "81.3% of 1204 statements" under
">= 80%" is 81.3; without one, the last number on the last line); pass is
its exit status. --measure value reads a number where the target alone would
mean a time (--until 2x --measure value --better higher).

--judge TEXT says what "done" means, in words, for models that didn't do
the work; --bar is the lowest severity of finding that keeps the task open
(critical, high, medium, low; medium by default); --judge-model M, repeated,
picks the models to ask (none: the audit default). A judge can be combined
with a check that has a fixed target or is a pass (not an amount
every round).
--keep-passing CMD (repeatable; a lone '' clears them) are commands that must
keep passing. --repo and --branch say where Waypoint reads the objective
(the project's repository and its default branch if left out). --dir is the
folder the commands run in, --setup runs once in each checkout before them.

--pace spare hands the task out only while your week's-end plan wants more
work ("Use what's left" in autopilot's settings), so its rounds run on
quota that would otherwise go unused; wp next KEY still takes it by name.
--every is the least time between rounds (default 24h for the shapes that
never end, 0 for a fixed target); a round that moved nothing waits longer
(an hour, doubling to a week).

A person who changes an agent's proposal starts from nothing unless they
name it: --rev REV (the "rev" of wp objective show KEY --json) builds the
change on the proposal they read.`,
			examples: []string{
				"wp objective set WP-12 --check 'go test -count=1 ./internal/core' --until '< 1s'",
				"wp objective set WP-12 --check ./scripts/coverage.sh --until '>= 80%' --keep-passing 'go vet ./...'",
				"wp objective set WP-12 --check 'make lint' --until pass",
				"wp objective set WP-12 --check ./bench-search --until 2x --pace spare --every 12h",
				"wp objective set WP-12 --judge 'the importer rejects every malformed row with its line number' --bar high",
				"wp objective set WP-12 --every 24h --rounds 30",
			},
			run: cmdObjectiveSet},
		{name: "show", args: "[KEY]", summary: "a task's objective: its phase, health, Waypoint's last reading, the judge's findings",
			examples: []string{"wp objective show WP-12", "wp objective show WP-12 --json"},
			run:      cmdObjectiveShow},
		{name: "check", args: "[KEY] [-n RUNS] [--timeout DURATION] [--wait DURATION]", summary: "take a working reading here (this tree and its base commit); decides nothing; exit 1 when not met",
			about: `Runs the objective's command in this checkout and, for a time or a value
when the tree differs from the commit the round started from, on that commit
too in a temporary worktree (with --setup run in both), and records both as a
working reading: the worker's guide, shown to people. It never decides the
round: when the round's work ends, Waypoint reads it itself in fresh checkouts
on the runner and that reading decides. A judged objective has no command to
run here. Exit 0: met by this reading; 1: not met; 3: the command failed, ran
past --timeout (15m for one command), or had no number to read.
--wait (10m) bounds the wait for a free "tests" host slot.`,
			examples: []string{"wp objective check", "wp objective check WP-12 -n 9", "wp objective check WP-12 --timeout 5m"},
			run:      cmdObjectiveCheck},
		{name: "ls", args: "[--project KEY] [--all]", summary: "the objectives in effect or proposed, with Waypoint's last reading (--all: met and ended too)",
			examples: []string{"wp objective ls", "wp objective ls --project WP --all"},
			run:      cmdObjectiveLs},
		{name: "confirm", args: "KEY", summary: "put an agent's proposed objective in effect (a person, in their own terminal)",
			examples: []string{"wp objective confirm WP-12"},
			run:      cmdObjectiveConfirm},
		{name: "clear", args: "KEY", summary: "take the objective off: an ordinary task again",
			examples: []string{"wp objective clear WP-12"},
			run:      cmdObjectiveClear},
		{name: "retry", args: "KEY", summary: "(a person) read a round again that couldn't be read, or is blocked on it",
			examples: []string{"wp objective retry WP-12"}, run: objectiveGo("retry")},
		{name: "next", args: "KEY", summary: "(a person) move on: end the wait before a round, or leave an unread round behind",
			examples: []string{"wp objective next WP-12"}, run: objectiveGo("next")},
		{name: "accept", args: "KEY", summary: "(a person) count the reading that waits because the round changed what the check runs",
			examples: []string{"wp objective accept WP-12"}, run: objectiveGo("accept")},
		{name: "end", args: "KEY", summary: "(a person) end the objective; the task stays as it is, an ordinary task",
			examples: []string{"wp objective end WP-12"}, run: objectiveGo("end")},
	},
}

// objective is what the CLI reads of a task's objective.
type objective struct {
	Key     string `json:"key"`
	Check   string `json:"check"`
	Dir     string `json:"dir"`
	Setup   string `json:"setup"`
	Measure string `json:"measure"`
	Unit    string `json:"unit"`
	Until   string `json:"until"`
	Judge   string `json:"judge"`
	Round   int    `json:"round"`
	State   string `json:"state"`
	Phase   string `json:"phase"`
	Rev     string `json:"rev"`
	// BaseSHA is the commit the round under way started from.
	BaseSHA string `json:"base_sha"`
	Text    string `json:"text"`
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
	Exact     bool     `json:"exact,omitempty"`
	// Rev is the objective as it stood when the check ran: one changed
	// since isn't kept.
	Rev string `json:"rev,omitempty"`
}

// cmdList is a repeatable flag whose values are kept whole: commas are part
// of a shell command, unlike listFlag's.
type cmdList []string

func (l *cmdList) String() string { return strings.Join(*l, "; ") }
func (l *cmdList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// objectiveFlags are the flags that give a task an objective, shared by
// wp objective set and wp new.
type objectiveFlags struct {
	check, until, judge, bar *string
	keep, models             *cmdList
	// set only on wp objective set
	dir, setup, measure, better, pace, every, repo, branch, rev *string
	rounds                                                      *int
}

func addObjectiveFlags(fs *flag.FlagSet, full bool) *objectiveFlags {
	f := &objectiveFlags{keep: new(cmdList), models: new(cmdList)}
	f.check = fs.String("check", "", "shell `COMMAND` that measures it, run in the repository")
	f.until = fs.String("until", "", "`TARGET`: '< 1s', '>= 80%', pass, 2x, -10%, +5%")
	f.judge = fs.String("judge", "", "what done means, in `TEXT`, for models that didn't do the work to judge by")
	fs.Var(f.keep, "keep-passing", "`COMMAND` that must keep passing: a round that breaks it hasn't met the objective (repeatable)")
	if !full {
		return f
	}
	f.bar = fs.String("bar", "", "lowest `LEVEL` of the judge's findings that keeps the task open: critical, high, medium (default), low")
	fs.Var(f.models, "judge-model", "`MODEL` the judge asks (repeatable; none: the audit default)")
	f.dir = fs.String("dir", "", "`DIR` within the repository the commands run in")
	f.setup = fs.String("setup", "", "shell `COMMAND` run once in each checkout before the check (not timed)")
	f.measure = fs.String("measure", "", "`MEASURE`: time, value or pass, when the target alone doesn't say")
	f.better = fs.String("better", "", "`WAY` a value improves: lower or higher")
	f.repo = fs.String("repo", "", "`REPO` (owner/name) Waypoint reads it in (default: the project's)")
	f.branch = fs.String("branch", "", "`BRANCH` Waypoint reads (default: the repository's default one)")
	f.pace = fs.String("pace", "", "`PACE`: queue (like any task) or spare (only on quota that would go unused)")
	f.every = fs.String("every", "", "least `DURATION` between rounds (0, 90m, 24h, 3d)")
	f.rounds = fs.Int("rounds", 0, "end after `N` rounds (0: no limit)")
	f.rev = fs.String("rev", "", "`REV` of the agent's proposal you read, when your change builds on it")
	return f
}

// body is what was given, as the server's ObjectiveInput; the flags that
// weren't given are left out, so they keep their values.
func (f *objectiveFlags) body(fs *flag.FlagSet) map[string]any {
	body := map[string]any{}
	strs := map[string]*string{"check": f.check, "until": f.until, "judge": f.judge, "judge_bar": f.bar, "dir": f.dir, "setup": f.setup, "measure": f.measure,
		"better": f.better, "repo": f.repo, "branch": f.branch, "pace": f.pace, "every": f.every, "rev": f.rev}
	names := map[string]string{"bar": "judge_bar"}
	fs.Visit(func(fl *flag.Flag) {
		name := fl.Name
		if n, ok := names[name]; ok {
			name = n
		}
		switch fl.Name {
		case "rounds":
			body["max_rounds"] = *f.rounds
		case "keep-passing":
			body["keep_passing"] = cmdListBody(*f.keep)
		case "judge-model":
			body["judge_models"] = cmdListBody(*f.models)
		default:
			if v := strs[name]; v != nil {
				body[name] = *v
			}
		}
	})
	return body
}

// cmdListBody is the list sent: a lone empty value clears it.
func cmdListBody(l cmdList) []string {
	out := []string{}
	for _, v := range l {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}

func cmdObjectiveSet(c *cli, args []string) error {
	fs := c.flags()
	f := addObjectiveFlags(fs, true)
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
	return c.objectiveWrite("PUT", "/api/v1/issues/"+esc(key)+"/objective", f.body(fs))
}

// objectiveGo is a person's control of an objective in effect: retry, next,
// accept or end. The server refuses an agent; what it says is printed as it
// is.
func objectiveGo(action string) func(c *cli, args []string) error {
	return func(c *cli, args []string) error {
		pos, err := c.parse(c.flags(), args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return c.usage("say which task: wp objective %s KEY", action)
		}
		return c.objectiveWrite("POST", "/api/v1/issues/"+esc(normalKey(pos[0]))+"/objective/go", map[string]any{"action": action})
	}
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

// setNewObjective gives a task just filed its objective (wp new --check…).
// The task exists whatever the objective's answer: a refusal is said with
// how to put it right, and is the command's error.
func (c *cli) setNewObjective(key string, body map[string]any) error {
	path := "/api/v1/issues/" + esc(key) + "/objective"
	if !c.asJSON {
		path += "?format=text"
	}
	raw, err := c.call("PUT", path, body)
	if err != nil {
		return fmt.Errorf("%s was filed, but its objective wasn't set: %w\nPut it right with wp objective set %s …", key, err, key)
	}
	if c.asJSON {
		return nil
	}
	c.print(string(raw))
	return nil
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

const (
	// objectiveRuns is the timed runs a tree of a check, as wp bench's.
	objectiveRuns = 5
	// objectiveCheckTimeout is the longest one command of wp objective
	// check may run; objectiveSlotWait the longest it waits for a slot.
	objectiveCheckTimeout = 15 * time.Minute
	objectiveSlotWait     = 10 * time.Minute
)

// objectiveLimits bound a working reading: the runs of a time, one command's
// time, the wait for the host's "tests" slot.
type objectiveLimits struct {
	Runs          int
	Timeout, Wait time.Duration
}

func cmdObjectiveCheck(c *cli, args []string) error {
	fs := c.flags()
	runs := fs.Int("n", objectiveRuns, "timed `RUNS` per tree, for a time")
	timeout := fs.Duration("timeout", objectiveCheckTimeout, "longest one `DURATION` the check, or its setup, may run")
	wait := fs.Duration("wait", objectiveSlotWait, "longest `DURATION` to wait for a free host slot")
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
	case *timeout <= 0 || *wait <= 0:
		return c.usage("--timeout and --wait are more than 0 (90s, 15m)")
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
	case o.Check == "" && o.Judge != "":
		return fmt.Errorf("%s's objective is judged: other models read it when the round ends, and there is no command to run here", o.Key)
	case o.Check == "":
		return fmt.Errorf("%s's objective is a loop: it has no check. wp ship ends the round", o.Key)
	}
	met, ok, err := c.objectiveCheck(&o, objectiveLimits{Runs: *runs, Timeout: *timeout, Wait: *wait})
	var stop *measureStop
	switch {
	case errors.As(err, &stop):
		_, _ = fmt.Fprintf(c.err, "wp objective check: %s\n", stop.msg)
		return exitCode(benchFailedCode)
	case err != nil:
		return err
	case !ok:
		return exitCode(benchFailedCode)
	case !met:
		return exitCode(1)
	}
	return nil
}

// objectiveMeasure measures one objective here; tests replace it.
var objectiveMeasure = measureObjective

// objectiveCheck takes a working reading here, records it and prints the
// server's word on it. ok is false when the command failed on this tree.
func (c *cli) objectiveCheck(o *objective, lim objectiveLimits) (met, ok bool, err error) {
	r, err := objectiveMeasure(c, o, lim)
	if err != nil {
		return false, false, err
	}
	r.Agent, r.Rev = c.agent, o.Rev
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
	if !c.asJSON {
		_, _ = fmt.Fprintf(c.err, "A working reading: Waypoint reads %s's round itself when it ends, in fresh checkouts on the runner, and that reading decides it, not this one.\n", o.Key)
	}
	return res.Met, r.OK, nil
}

// objectiveBase is the commit this reading compares with: the one the round
// started from, as Waypoint recorded it (the server's base_sha), not a
// merge base computed here, which a branch that merged the default branch in
// would move to the merge. "" when there is none or it isn't here.
func objectiveBase(c *cli, o *objective, headTree string) string {
	if o.BaseSHA == "" || o.Measure != "time" && o.Measure != "value" {
		return ""
	}
	sha, err := localGit("rev-parse", "--verify", "-q", o.BaseSHA+"^{commit}")
	if err != nil || sha == "" {
		// A commit this clone hasn't fetched yet.
		_, _ = localGit("fetch", "-q", "origin", o.BaseSHA)
		if sha, err = localGit("rev-parse", "--verify", "-q", o.BaseSHA+"^{commit}"); err != nil || sha == "" {
			_, _ = fmt.Fprintf(c.err, "Notice: the commit the round started from (%s) isn't in this clone, so this reading has nothing to compare with.\n", shortSHA(o.BaseSHA))
			return ""
		}
	}
	// With nothing changed since, there is nothing to compare.
	if tree, err := localGit("rev-parse", sha+"^{tree}"); err == nil && tree == headTree {
		return ""
	}
	return sha
}

// measureObjective takes the working reading in this checkout and, for a
// time or a value when the tree has changed since the commit the round
// started from, on that commit too.
func measureObjective(c *cli, o *objective, lim objectiveLimits) (*objectiveReading, error) {
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
	base := objectiveBase(c, o, r.Tree)
	if base != "" && o.Dir != "" {
		// A folder the base doesn't have: the check can't run there.
		if _, err := localGit("rev-parse", "--verify", "-q", base+":"+o.Dir); err != nil {
			no := false
			r.BaseOK, r.BaseSHA, base = &no, base, ""
		}
	}
	var baseDir, baseSHA string
	if base != "" {
		d, cleanup, sha, err := baseWorktree(base)
		if err != nil {
			_, _ = fmt.Fprintf(c.err, "Notice: the base commit %s couldn't be checked out (%v), so this check has nothing to compare with.\n", shortSHA(base), err)
		} else {
			defer cleanup()
			baseDir, baseSHA = d, sha
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	wctx, wstop := context.WithTimeout(ctx, lim.Wait)
	env, release, err := c.takeSlot(wctx, "tests", defaultSlots())
	wstop()
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("waited %s for a free \"tests\" slot and gave up (wp slot ls shows who has them; --wait waits longer)", lim.Wait)
		}
		return nil, err
	}
	defer release()

	spec := measureSpec{Check: o.Check, Setup: o.Setup, Measure: o.Measure, Unit: o.Unit, Timeout: lim.Timeout, MaxRuns: lim.Runs, Env: env,
		Prepared: func() { r.Tree = benchTree() }}
	if spec.Measure != "time" && spec.Measure != "value" {
		spec.Measure = "pass"
	}
	sides := []measureSide{{"this tree", dir}}
	if baseDir != "" {
		sides = append(sides, measureSide{"the base (" + shortSHA(baseSHA) + ")", baseDir})
	}
	reads, p, exact, err := measureCheck(ctx, spec, sides, nil)
	if err != nil {
		return nil, err
	}
	head := reads[0]
	r.OK, r.Value, r.Low, r.High, r.Runs = head.OK, head.Value, head.Low, head.High, max(head.Runs, 1)
	if !head.OK {
		_, _ = fmt.Fprintf(c.err, "The check failed in %s (exit %d):\n%s\n", dir, head.Exit, outputTail([]byte(head.Output), benchTailLines))
	}
	if len(reads) > 1 {
		b := reads[1]
		ok := b.OK
		r.BaseOK, r.BaseValue, r.BaseSHA = &ok, b.Value, baseSHA
		if head.OK && b.OK {
			r.P, r.Exact = p, exact
		}
		if !b.OK {
			_, _ = fmt.Fprintf(c.err, "Notice: the check gave nothing at the base (%s), so there is nothing to compare with.\n", excerpt(lastText(b.Output), 160))
		}
	} else if head.OK {
		r.Exact = exact
	}
	if end := benchTree(); r.Tree != "" && end != "" && end != r.Tree {
		r.Moved = true
	}
	return r, nil
}

func outputTail(b []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// objectiveShipNote tells the worker, before a round ships, who reads it.
// It no longer measures: the round's end is decided by Waypoint's own
// reading in fresh checkouts, so a check run here first would only make
// wp ship wait (a slot, several runs of a slow check) for a number that
// decides nothing. A working reading is wp objective check, when the worker
// wants one. Silent for a task with no objective in effect.
func (c *cli) objectiveShipNote(key string) {
	var o objective
	if err := c.do("GET", "/api/v1/issues/"+esc(key)+"/objective", nil, &o); err != nil || o.State != "active" || c.asJSON {
		return
	}
	what := "wp objective check " + o.Key + " takes a working reading"
	if o.Check == "" {
		what = "wp objective show " + o.Key + " says where it stands"
	}
	_, _ = fmt.Fprintf(c.err, "Notice: %s has an objective: when this round ends, Waypoint reads it itself in fresh checkouts on the runner. Nothing is measured here (%s).\n", o.Key, what)
}
