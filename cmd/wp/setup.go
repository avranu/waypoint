package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// `wp setup` sets a computer up in one go, and is safe to run again: the
// server and a sign-in (the device code, approved in the browser), the
// Claude Code hooks when Claude Code is here, and the runner service, with
// allowed directories taken from where Claude Code has worked. It says
// what it will do and asks once. `curl -fsSL <server>/setup | sh` installs
// wp and runs it.

var setupCmd = &command{
	name: "setup", group: "Setup", args: "[SERVER] [FLAGS]",
	summary: "sign in, install Claude Code hooks and the runner",
	about: `Safe to run again. Steps:

  1. sign in, if needed (approve a code in the browser)
  2. install the Claude Code hooks, if Claude Code is here (wp hook claude)
  3. install the runner, a login service that takes your jobs (wp runner install)

The runner's allowed directories default to the top-level folders where
Claude Code has worked (never / or your home); an installed runner keeps
its settings. --allow replaces them (--allow none: only agent jobs on a
fresh checkout). It shows the plan and asks once; --yes skips the question.`,
	examples: []string{
		"wp setup",
		"wp setup https://waypoint.example.com",
		"wp setup --allow ~/code --allow /srv/projects",
		"wp setup --yes --no-runner",
		"curl -fsSL https://waypoint.example.com/setup | sh -s -- --yes",
	},
	run:  cmdSetup,
	subs: []*command{setupDoctorCmd},
}

// claudeInstalled says whether Claude Code is on this computer (tests swap it).
var claudeInstalled = func() bool {
	if _, err := exec.LookPath("claude"); err == nil {
		return true
	}
	if d := claudeDir(""); d != "" {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}

// openAnswers is where answers to questions come from: stdin at a terminal,
// else the terminal itself (under curl | sh, stdin is the script); nil when
// nobody can answer (tests swap it).
var openAnswers = func() io.ReadCloser {
	if stdinIsTerminal() {
		return io.NopCloser(os.Stdin)
	}
	if runtime.GOOS != "windows" {
		if f, err := os.Open("/dev/tty"); err == nil {
			return f
		}
	}
	return nil
}

// setupPlan is what wp setup will do.
type setupPlan struct {
	me string // who is signed in ("" if nobody)

	hooks      bool   // install or update the hooks
	hooksPath  string // the settings file
	hooksState string // "install", "update", "current"
	hooksSkip  string // why not
	noPrompts  bool

	runner     bool
	runnerOpts runnerOpts
	runnerExe  string
	allowFrom  string // where the allowed directories came from
	runnerSkip string
}

func cmdSetup(c *cli, args []string) error {
	fs := c.flags()
	var allow, labels listFlag
	fs.Var(&allow, "allow", "`DIR`: directory the runner may use (repeatable; replaces default; none)")
	fs.Var(&labels, "label", "`LABEL`: runner label (repeatable)")
	name := fs.String("name", "", "`NAME`: runner name (default: host name)")
	maxJobs := fs.Int("max-jobs", 0, "`N`: most jobs at once (default 1)")
	noRunner := fs.Bool("no-runner", false, "don't install the runner")
	noHooks := fs.Bool("no-hooks", false, "don't install the Claude Code hooks")
	noPrompts := fs.Bool("no-prompts", false, "hooks leave prompt text out (no digest of it either; the session title is still sent: wp hook claude -h)")
	noAutoUpdate := fs.Bool("no-auto-update", false, "runner keeps this wp version")
	yes := fs.Bool("yes", false, "go ahead without asking")
	server := fs.String("server", "", "Waypoint `URL` (or the argument)")
	pos, err := c.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 1 && *server == "" && strings.HasPrefix(pos[0], "http") {
		*server = pos[0]
	} else if len(pos) > 0 {
		return c.usage("unexpected %q", joinArgs(pos))
	}
	c.agent = "none" // the runner and the sign-in are the person's, never an agent's

	ask := func(question string) (string, bool) {
		r := openAnswers()
		if r == nil {
			return "", false
		}
		defer func() { _ = r.Close() }()
		c.print(question)
		line, err := bufio.NewReader(r).ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			c.println() // no answer (EOF): as good as no terminal
			return "", false
		}
		return strings.TrimSpace(line), true
	}

	if s := strings.TrimRight(strings.TrimSpace(*server), "/"); s != "" && s != c.server() {
		if err := c.setConfig("server", s); err != nil {
			return err
		}
	}
	if c.server() == "" {
		if *yes {
			return c.usage("no server set: wp setup https://your-waypoint")
		}
		s, ok := ask("Your Waypoint's address (https://…): ")
		if !ok || s == "" {
			return c.usage("no server set: wp setup https://your-waypoint")
		}
		if err := c.setConfig("server", s); err != nil {
			return err
		}
	}

	plan, err := c.planSetup(setupFlags{allow: allow, labels: labels, name: *name, maxJobs: *maxJobs,
		noRunner: *noRunner, noHooks: *noHooks, noPrompts: *noPrompts, noAutoUpdate: *noAutoUpdate})
	if err != nil {
		return err
	}
	c.printPlan(plan)
	if !*yes {
		a, ok := ask("Go ahead? [Y/n] ")
		if !ok {
			return c.usage("there's no terminal to ask on: add --yes to go ahead")
		}
		if a = strings.ToLower(a); a != "" && a != "y" && a != "yes" {
			c.println("Nothing changed.")
			return nil
		}
	}
	return c.runSetup(plan)
}

// setupFlags are wp setup's choices.
type setupFlags struct {
	allow, labels                              []string
	name                                       string
	maxJobs                                    int
	noRunner, noHooks, noPrompts, noAutoUpdate bool
}

// planSetup works out what wp setup will do, changing nothing.
func (c *cli) planSetup(f setupFlags) (*setupPlan, error) {
	p := &setupPlan{}
	if c.token() != "" {
		var me struct {
			Username string `json:"username"`
		}
		err := c.do("GET", "/api/v1/me", nil, &me)
		var ae *apiError
		switch {
		case err == nil:
			p.me = me.Username
		case errors.As(err, &ae) && ae.status == 401:
		default:
			return nil, err
		}
	}

	// The hooks.
	switch {
	case f.noHooks:
		p.hooksSkip = "--no-hooks"
	case !claudeInstalled():
		p.hooksSkip = "Claude Code isn't installed here; run wp setup again once it is"
	default:
		path, err := claudeSettingsPath()
		if err != nil {
			return nil, err
		}
		res, err := installClaudeHooks(path, hookInstallOpts{exe: hookExe(), noPrompts: f.noPrompts, keepNoPrompts: true, dryRun: true})
		if err != nil {
			return nil, err
		}
		p.hooks, p.hooksPath, p.noPrompts = true, res.path, res.noPrompts
		switch {
		case !res.changed:
			p.hooksState = "current"
		case res.present:
			p.hooksState = "update"
		default:
			p.hooksState = "install"
		}
	}

	// The runner: as installed before, then the flags.
	if f.noRunner {
		p.runnerSkip = "--no-runner"
		return p, nil
	}
	o := runnerOpts{MaxJobs: 1}
	if prev := c.state.Runner; prev != nil {
		o.Name, o.Labels, o.Allow, o.Env, o.NoUsageSync, o.NoPrep, o.NoAutoUpdate = prev.Name, prev.Labels, prev.Allow, prev.Env, prev.NoUsageSync, prev.NoPrep, prev.NoAutoUpdate
		o.MaxJobs = max(prev.MaxJobs, 1)
		p.allowFrom = "as installed before"
	}
	home, _ := os.UserHomeDir()
	switch {
	case len(f.allow) == 1 && strings.EqualFold(f.allow[0], "none"):
		o.Allow, p.allowFrom = nil, "--allow none"
	case len(f.allow) > 0:
		o.Allow, p.allowFrom = f.allow, "--allow"
	case c.state.Runner == nil:
		o.Allow = claudeWorkRoots(claudeDir(""), home)
		p.allowFrom = "where Claude Code has worked on this computer"
	}
	if f.name != "" {
		o.Name = f.name
	}
	if len(f.labels) > 0 {
		o.Labels = f.labels
	}
	if f.maxJobs != 0 {
		o.MaxJobs = f.maxJobs
	}
	if f.noAutoUpdate {
		o.NoAutoUpdate = true
	}
	exe, err := prepareRunnerInstall(c, &o)
	if err != nil {
		return nil, err
	}
	p.runner, p.runnerOpts, p.runnerExe = true, o, exe
	return p, nil
}

// serviceKind names the service the runner becomes here.
func serviceKind() string {
	switch runtime.GOOS {
	case "linux":
		return "a systemd user service"
	case "darwin":
		return "a launchd agent"
	}
	return "a command to start yourself (no service on " + runtime.GOOS + " yet)"
}

func (c *cli) printPlan(p *setupPlan) {
	home, _ := os.UserHomeDir()
	c.println("wp setup, for", c.server()+":")
	if p.me != "" {
		c.println("  sign in          already signed in as @" + p.me)
	} else {
		c.println("  sign in          approve a code in your browser")
	}
	switch {
	case !p.hooks:
		c.println("  Claude Code      skipped: " + p.hooksSkip)
	case p.hooksState == "current":
		c.println("  Claude Code      the hooks in " + tildePath(p.hooksPath, home) + " are up to date")
	default:
		verb := map[string]string{"install": "install", "update": "update"}[p.hooksState]
		prompts := "prompts sent"
		if p.noPrompts {
			prompts = "no prompts"
		}
		c.println("  Claude Code      " + verb + " the hooks in " + tildePath(p.hooksPath, home) + " (sessions, requests, usage; " + prompts + ")")
	}
	if !p.runner {
		c.println("  runner           skipped: " + p.runnerSkip)
		return
	}
	o := p.runnerOpts
	name := o.Name
	if name == "" {
		name = machineName()
	}
	updates := ", updating wp when Waypoint does"
	if o.NoAutoUpdate {
		updates = ""
	}
	c.println(fmt.Sprintf("  runner           %q: %s that starts at login%s", name, serviceKind(), updates))
	if len(o.Allow) == 0 {
		c.println("                   no allowed folders: only coding-agent jobs on a fresh checkout run (--allow DIR to add)")
		return
	}
	c.println("                   command and apply jobs may run in (" + p.allowFrom + "; --allow DIR to choose):")
	for _, d := range o.Allow {
		c.println("                     " + tildePath(d, home))
	}
}

// runSetup carries the plan out and sums up.
func (c *cli) runSetup(p *setupPlan) error {
	if err := c.ensureLogin(); err != nil {
		return err
	}
	var me struct {
		Username string `json:"username"`
	}
	_ = c.do("GET", "/api/v1/me", nil, &me)
	home, _ := os.UserHomeDir()
	summary := []string{"Signed in to " + c.server() + " as @" + orDash(me.Username) + "."}

	if p.hooks {
		path, err := claudeSettingsPath()
		if err != nil {
			return err
		}
		exe := hookExe()
		res, err := installClaudeHooks(path, hookInstallOpts{exe: exe, noPrompts: p.noPrompts, keepNoPrompts: true})
		if err != nil {
			return err
		}
		c.recordHooks(path, res, exe, false)
		switch {
		case !res.changed:
			summary = append(summary, "Claude Code hooks: up to date in "+tildePath(res.path, home)+".")
		default:
			s := "Claude Code hooks: installed in " + tildePath(res.path, home)
			if res.backup != "" {
				s += " (the old file is " + tildePath(res.backup, home) + ")"
			}
			summary = append(summary, s+"; they update themselves when wp does.")
		}
		for _, n := range res.notes {
			summary = append(summary, "  Also: "+n+".")
		}
	} else {
		summary = append(summary, "Claude Code hooks: skipped ("+p.hooksSkip+").")
	}

	if p.runner {
		c.println()
		if err := installRunner(c, p.runnerExe, p.runnerOpts); err != nil {
			return err
		}
		allowed := "no allowed folders"
		if len(p.runnerOpts.Allow) > 0 {
			var ds []string
			for _, d := range p.runnerOpts.Allow {
				ds = append(ds, tildePath(d, home))
			}
			allowed = "allowed: " + strings.Join(ds, ", ")
		}
		summary = append(summary, "Runner: "+serviceKind()+"; "+allowed+".")
	} else {
		summary = append(summary, "Runner: skipped ("+p.runnerSkip+").")
	}
	summary = append(summary, agentCLISummary(detectCLIsFn(context.Background()))...)
	summary = append(summary, c.kitSyncHere())
	c.println()
	c.println("Done.")
	for _, s := range summary {
		c.println("  " + s)
	}
	c.println("  Run wp setup again any time; wp runner status shows the runner.")
	return nil
}

// cliLabels name the coding-agent CLIs as people know them.
var cliLabels = map[string]string{"claude": "Claude Code", "codex": "Codex", "opencode": "opencode", "cursor": "Cursor"}

// agentCLISummary says which coding agents the runner found and whether each
// is signed in, so a subscription that isn't connected yet shows here, with
// what to run. Cursor gets its install line: its plan (Grok and the other
// Cursor models) is spent only through cursor-agent on a runner (WP-305).
func agentCLISummary(found []runnerCLI) []string {
	var parts, todo []string
	have := map[string]bool{}
	for _, c := range found {
		have[c.Name] = true
		state := ""
		switch {
		case c.SignedIn != nil && !*c.SignedIn:
			state = " (signed out)"
			todo = append(todo, "sign "+cliLabels[c.Name]+" in: "+signInHint(c.Name))
		case c.SignedIn != nil && c.Detail != "":
			state = " (" + c.Detail + ")"
		}
		parts = append(parts, cmpStr(cliLabels[c.Name], c.Name)+state)
	}
	out := []string{"Coding agents here: none found."}
	if len(parts) > 0 {
		out[0] = "Coding agents here: " + strings.Join(parts, ", ") + "."
	}
	for _, t := range todo {
		out = append(out, "  To do: "+t+".")
	}
	if !have["cursor"] {
		out = append(out, "  Have a Cursor plan? curl https://cursor.com/install -fsS | bash, then cursor-agent login, and the runner offers it within 10 minutes.")
	}
	return out
}
