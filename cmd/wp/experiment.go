package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const (
	expNextBrief   = "next_brief"
	expSearchNudge = "search_nudge"
)

// clientExperiments are the experiments wp itself carries out.
var clientExperiments = []string{expNextBrief, expSearchNudge}

// fetchArms asks for the arms of the task just taken here and keeps them on
// its claim. It never fails the command that called it.
func (c *cli) fetchArms(key, via string) {
	if key == "" {
		return
	}
	c.delegation = ""
	var got struct {
		Arms map[string]struct {
			Arm string `json:"arm"`
		} `json:"arms"`
		Delegation *struct {
			Arm  string `json:"arm"`
			Text string `json:"text"`
		} `json:"delegation"`
	}
	body := map[string]any{"experiments": clientExperiments, "via": via, "delegation": !c.asJSON}
	if err := c.do("POST", "/api/v1/issues/"+esc(key)+"/arms", body, &got); err != nil {
		return
	}
	arms := make(map[string]string, len(got.Arms)+1)
	for k, a := range got.Arms {
		arms[k] = a.Arm
	}
	if d := got.Delegation; d != nil && d.Arm != "" {
		arms["delegation"] = d.Arm
		c.delegation = d.Text
	}
	if len(arms) == 0 {
		return
	}
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	_ = c.updateState(func(s *State) {
		if cl := s.Claims[dir]; cl != nil && sameKey(cl.Key, key) {
			cl.Arms = arms
		}
	})
}

// armHere is the arm of exp for key, taken in this directory; "" is the
// control.
func (c *cli) armHere(key, exp string) string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	if _, cl := c.state.claimFor(dir); cl != nil && sameKey(cl.Key, key) {
		return cl.Arms[exp]
	}
	return ""
}

// sessionArm is the arm of exp for the task a hook's session works on.
func (c *cli) sessionArm(cwd, session, exp string) string {
	if cl := c.state.sessionClaim(cwd, session); cl != nil {
		return cl.Arms[exp]
	}
	return ""
}

var experimentCmd = &command{
	name: "experiment", group: "Tasks", args: "[ls|show|start|stop …]",
	summary:  "A/B tests of how agents work: switch one on or off, read its results (admins)",
	examples: []string{"wp experiment ls", "wp experiment show next_brief", "wp experiment start search_nudge --days 10 --target 40", "wp experiment stop next_brief -m \"enough\""},
	about: `An experiment gives each task an agent takes one of its arms, at random
and kept, and compares the tasks of each arm: cost, tokens, turns, tool
calls, compactions, review findings and more, per task. The first arm is the
control (today's behaviour), which every task gets while no run is live.

A run's weights are fixed: changing them is a new run. A run stops by
itself at its end date, when every arm has its target of finished tasks,
or when a guardrail trips (an arm costing half again as much, or finishing
markedly fewer tasks). Starting and stopping need a person who is an admin;
reading needs an admin's token. Settings → Admin → Experiments has the same.`,
	subs: []*command{
		{name: "ls", summary: "every experiment, its arms and its live run",
			examples: []string{"wp experiment ls", "wp experiment ls --json"},
			run:      cmdExperimentLs},
		{name: "show", args: "KEY [--run N] [--scope all]", summary: "an experiment's results: per-arm medians, change against the control, intervals, caveats",
			about: `Compares the tasks of each arm of the latest run (--run N for an earlier
one). By default only finished tasks count; --scope all counts every task
with usage. Read the verdict and its caveats before acting on a number:
the interval matters more than the median, and "no difference" with few
tasks means only that a small effect can't be told apart.`,
			examples: []string{"wp experiment show prep", "wp experiment show next_brief --scope all", "wp experiment show search_nudge --run 4"},
			run:      cmdExperimentShow},
		{name: "start", args: "KEY [--weights ARM=N,…] [--days N] [--target N] [--project KEY]… [--param K=V]… [-m NOTE]", summary: "switch an experiment on (a person who is an admin)",
			about: `Starts a run. --weights splits tasks between arms (default the
experiment's own); --days ends it (default 14, at most 90: every run is
short, so what it costs stays small); --target stops it once every arm has that many finished tasks (default
40, 0 = none); --project limits it to projects (default all).`,
			examples: []string{"wp experiment start next_brief", "wp experiment start autopilot_phrase --param text='Take full responsibility for the result.' --days 7", "wp experiment start search_nudge --weights on=1,off=1 --project WP"},
			run:      cmdExperimentStart},
		{name: "stop", args: "KEY [-m REASON]", summary: "switch an experiment off: every task gets the control again",
			examples: []string{"wp experiment stop next_brief -m \"enough tasks\""},
			run:      cmdExperimentStop},
	},
}

func cmdExperimentLs(c *cli, args []string) error {
	fs := c.flags()
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	return c.getDiag(pos, "/api/v1/experiments", nil)
}

func cmdExperimentShow(c *cli, args []string) error {
	fs := c.flags()
	run := fs.Int("run", 0, "`N`: this run, not the latest")
	scope := fs.String("scope", "", "`SCOPE`: done (finished tasks, the default) or all")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("say which experiment: wp experiment show KEY")
	}
	q := url.Values{}
	setIf(q, "scope", *scope)
	if *run > 0 {
		q.Set("run", strconv.Itoa(*run))
	}
	return c.getDiag(nil, "/api/v1/experiments/"+esc(pos[0])+"/results", q)
}

func cmdExperimentStart(c *cli, args []string) error {
	fs := c.flags()
	weights := fs.String("weights", "", "`ARM=N,…`: how tasks are split between arms")
	days := fs.Int("days", 0, "`N`: end after N days (1 to 90; default 14)")
	target := fs.Int("target", -1, "`N`: stop once every arm has N finished tasks (0 = none; default 40)")
	var projects, params multiFlag
	fs.Var(&projects, "project", "`KEY`: only this project's tasks (repeatable)")
	fs.Var(&params, "param", "`K=V`: an experiment's setting (repeatable)")
	note := fs.String("message", "", "`NOTE`: why, for whoever reads the results")
	short(fs, "m", "message")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("say which experiment: wp experiment start KEY")
	}
	body := map[string]any{"note": *note}
	if *weights != "" {
		w := map[string]int{}
		for _, part := range strings.Split(*weights, ",") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			n, err := strconv.Atoi(v)
			if !ok || err != nil {
				return c.usage("--weights takes ARM=N,… like on=1,off=1, not %q", part)
			}
			w[k] = n
		}
		body["weights"] = w
	}
	if *days != 0 {
		body["days"] = *days
	}
	if *target >= 0 {
		body["target"] = *target
	}
	if len(projects) > 0 {
		body["projects"] = []string(projects)
	}
	if len(params) > 0 {
		p := map[string]string{}
		for _, kv := range params {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return c.usage("--param takes K=V, not %q", kv)
			}
			p[k] = v
		}
		body["params"] = p
	}
	return c.experimentChange("start", pos[0], body)
}

func cmdExperimentStop(c *cli, args []string) error {
	fs := c.flags()
	reason := fs.String("m", "", "`REASON`: why it stopped")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return c.usage("say which experiment: wp experiment stop KEY")
	}
	return c.experimentChange("stop", pos[0], map[string]any{"reason": *reason})
}

// experimentChange starts or stops a run and says what is live now.
func (c *cli) experimentChange(verb, key string, body map[string]any) error {
	raw, err := c.call("POST", "/api/v1/experiments/"+esc(key)+"/"+verb, body)
	if err != nil {
		return err
	}
	return c.emit(raw, func() string {
		if verb == "stop" {
			return fmt.Sprintf("Stopped %s: every task gets the control again. wp experiment show %s has the results.", key, key)
		}
		return fmt.Sprintf("Started %s. wp experiment show %s reads how it goes; wp experiment stop %s ends it.", key, key, key)
	})
}
