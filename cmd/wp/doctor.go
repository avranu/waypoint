package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

var doctorCmd = &command{
	name: "doctor", group: "Setup", args: "[TOOL] [FLAGS]",
	summary: "health checks: this machine's setup, CI, the sandbox",
	about: `Each tool says what is wrong and what to run about it. wp doctor TOOL and
wp TOOL doctor are the same command. Alone, wp doctor checks this machine's
setup and names the other tools.`,
	examples: []string{"wp doctor", "wp doctor ci", "wp doctor setup --json"},
}

func init() { doctorCmd.run = cmdDoctor }

// addDoctorTools makes every top-level `wp X doctor` also `wp doctor X`.
func addDoctorTools() {
	for _, top := range commands {
		d := top.sub("doctor")
		if d == nil {
			continue
		}
		tool := *d
		tool.name, tool.aliases = top.name, nil
		same := "wp " + top.name + " doctor"
		tool.about = strings.TrimLeft(strings.TrimRight(d.about, "\n")+"\n\nThe same as "+same+".", "\n")
		tool.examples = nil
		for _, e := range d.examples {
			tool.examples = append(tool.examples, strings.ReplaceAll(e, same, "wp doctor "+top.name))
		}
		doctorCmd.subs = append(doctorCmd.subs, &tool)
	}
}

func cmdDoctor(c *cli, args []string) error {
	for i, a := range args {
		if t := doctorCmd.sub(a); t != nil {
			return c.dispatch(t, slices.Delete(slices.Clone(args), i, i+1))
		}
	}
	var more []string
	for _, t := range doctorCmd.subs {
		if t.name != "setup" && !t.hidden {
			more = append(more, "wp doctor "+t.name)
		}
	}
	return setupDoctor(c, args, "More: "+strings.Join(more, " · ")+"\n")
}

var setupDoctorCmd = &command{
	name: "doctor", summary: "is this machine set up? what is missing and what to run",
	about: `Changes nothing. The config file, the server and your sign-in, wp's version,
the project here, the Claude Code hooks, the runner, signed-out coding agents
and the agent kit. Exits 1 if any line says FAIL.`,
	examples: []string{"wp setup doctor", "wp setup doctor --json"},
	run:      func(c *cli, args []string) error { return setupDoctor(c, args, "") },
}

// doctorCallTimeout bounds each of the setup doctor's requests: a server that
// answers some and stalls on others is what a doctor gets called for, and
// nothing is printed until the checks end. A variable for the tests.
var doctorCallTimeout = 10 * time.Second

// setupDoctor runs the setup checks; more follows them in the text output.
func setupDoctor(c *cli, args []string, more string) error {
	pos, err := c.parse(c.flags(), args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return c.usage("no doctor tool %q", joinArgs(pos))
	}
	c.hookTimeout = doctorCallTimeout
	return c.reportChecks(c.setupChecks(), more)
}

// reportChecks prints a doctor's checks, and exits 1 if one failed.
func (c *cli) reportChecks(checks []doctorCheck, more string) error {
	b, err := json.Marshal(checks)
	if err != nil {
		return err
	}
	if err := c.emit(b, func() string { return doctorText(checks) + more }); err != nil {
		return err
	}
	if slices.ContainsFunc(checks, func(ch doctorCheck) bool { return ch.State == checkFail }) {
		return exitCode(1)
	}
	return nil
}

func (c *cli) setupChecks() []doctorCheck {
	var checks []doctorCheck
	add := func(state, name, format string, a ...any) {
		checks = append(checks, doctorCheck{state, name, fmt.Sprintf(format, a...)})
	}
	c.checkConfig(add)
	signedIn := c.checkServer(add)

	agent := orDash(c.agentHeader())
	if _, set := c.explicitAgent(); !set && c.agentHeader() != "" {
		agent += " (detected)"
	}
	add(checkInfo, "agent", "%s", agent)

	c.checkRepo(signedIn, add)
	c.checkHooks(add)
	c.checkGuardLog(add)
	c.checkMemoryHealth(add)
	c.checkRunner(signedIn, add)
	checkAgentCLIs(add)
	return checks
}

type addCheck func(state, name, format string, a ...any)

func (c *cli) checkConfig(add addCheck) {
	path := filepath.Join(c.dir, "config.json")
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist) && c.server() != "":
		add(checkInfo, "config", "no %s: the settings come from the environment", path)
	case errors.Is(err, os.ErrNotExist):
		add(checkFail, "config", "no %s: wp setup https://your-waypoint", path)
	case err != nil:
		add(checkFail, "config", "%v", err)
	case runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 && c.cfg.Token != "":
		add(checkWarn, "config", "%s holds your token and others can read it: chmod 600 %s", path, path)
	default:
		add(checkOK, "config", "%s", path)
	}
}

// checkServer checks the server, wp's version against it and the sign-in. It
// says whether the server knows this token, which the later checks ask it.
func (c *cli) checkServer(add addCheck) bool {
	if c.server() == "" {
		add(checkFail, "server", "none set: wp setup https://your-waypoint")
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), doctorCallTimeout)
	defer cancel()
	sv, err := c.serverVersion(ctx)
	if err != nil {
		add(checkFail, "server", "%v", err)
		return false
	}
	add(checkOK, "server", "%s answers", c.server())
	checkVersion(add, sv)
	if c.token() == "" {
		add(checkFail, "sign-in", "not signed in: wp auth login")
		return false
	}
	var me struct {
		Username string `json:"username"`
	}
	err = c.do("GET", "/api/v1/me", nil, &me)
	var ae *apiError
	switch {
	case err == nil:
		add(checkOK, "sign-in", "@%s", me.Username)
		return true
	case errors.As(err, &ae) && ae.status == 401:
		add(checkFail, "sign-in", "the server refuses this token: wp auth login")
	default:
		add(checkWarn, "sign-in", "%v", err)
	}
	return false
}

// checkVersion compares this wp with the server's version, sv.
func checkVersion(add addCheck, sv string) {
	switch {
	case sv == "":
		add(checkInfo, "wp version", "%s; the server doesn't say its version", version)
	case sv == version:
		add(checkOK, "wp version", "%s, the same as the server", version)
	case !isRelease(sv) || !isRelease(version):
		add(checkInfo, "wp version", "%s, a development build; the server runs %s", version, sv)
	case slices.Compare(releaseParts(version), releaseParts(sv)) > 0:
		add(checkInfo, "wp version", "%s, newer than the server's %s", version, sv)
	default:
		add(checkWarn, "wp version", "%s, the server runs %s: wp upgrade", version, sv)
	}
}

// releaseParts is the numbers of a release version (see isRelease).
func releaseParts(v string) []int {
	var p [3]int
	_, _ = fmt.Sscanf(v, "v%d.%d.%d", &p[0], &p[1], &p[2])
	return p[:]
}

func (c *cli) checkRepo(signedIn bool, add addCheck) {
	repo, inRepo := gitRepoHere()
	switch {
	case repo == "" && !inRepo:
		add(checkInfo, "repo", "not in a git repo here")
		return
	case repo == "":
		add(checkWarn, "repo", "a git repo without an owner/name remote: wp finds no project from it")
		return
	case !signedIn:
		add(checkInfo, "repo", "%s", repo)
		return
	}
	if sc, err := c.implicitScope(); err != nil {
		add(checkWarn, "repo", "%s: %s", repo, strings.Join(strings.Fields(err.Error()), " "))
	} else {
		add(checkOK, "repo", "%s: %s", repo, sc.String())
	}
	c.checkKit(add)
}

// checkKit compares what this checkout last applied with the agent kit now.
func (c *cli) checkKit(add addCheck) {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	top := gitTop(cwd)
	if top == "" {
		return
	}
	repo, _, _ := gitInfo(top)
	if repo == "" {
		return
	}
	r, err := c.resolveKit(repo)
	if err != nil {
		add(checkWarn, "agent kit", "can't ask the server: %v", err)
		return
	}
	st := c.state.Kit[top]
	switch {
	case r.Commit == "":
		add(checkInfo, "agent kit", "none yet (an admin picks its repo in Admin → Forges)")
	case st == nil || st.Commit == "":
		add(checkWarn, "agent kit", "never synced here: wp kit sync")
	case st.Commit != r.Commit:
		add(checkWarn, "agent kit", "here %s, the kit is at %s: wp kit sync", short7(st.Commit), short7(r.Commit))
	default:
		add(checkOK, "agent kit", "up to date at %s", short7(r.Commit))
	}
}

func (c *cli) checkHooks(add addCheck) {
	if !claudeInstalled() {
		add(checkInfo, "hooks", "Claude Code isn't installed here")
	} else {
		path, err := claudeSettingsPath()
		if err != nil {
			add(checkWarn, "hooks", "%v", err)
		} else {
			res, err := installClaudeHooks(path, hookInstallOpts{exe: hookExe(), keepNoPrompts: true, dryRun: true})
			switch {
			case err != nil:
				add(checkWarn, "hooks", "%v", err)
			case !res.changed:
				add(checkOK, "hooks", "up to date in %s", res.path)
			case res.present:
				add(checkWarn, "hooks", "out of date in %s: wp hook claude", res.path)
			default:
				add(checkWarn, "hooks", "not installed in %s: wp hook claude", res.path)
			}
		}
	}
	if antigravityInstalled() {
		path, err := antigravityHooksPath()
		if err != nil {
			add(checkWarn, "antigravity hooks", "%v", err)
			return
		}
		res, err := installAntigravityHooks(path, hookInstallOpts{exe: hookExe(), keepNoPrompts: true, dryRun: true})
		switch {
		case err != nil:
			add(checkWarn, "antigravity hooks", "%v", err)
		case !res.changed:
			add(checkOK, "antigravity hooks", "up to date in %s", res.path)
		case res.present:
			add(checkWarn, "antigravity hooks", "out of date in %s: wp hook antigravity", res.path)
		default:
			add(checkWarn, "antigravity hooks", "not installed in %s: wp hook antigravity", res.path)
		}
	}
}

// checkGuardLog surfaces high recent claude-guard denial volume so false
// positives or misconfigured repos don't go unnoticed (N25).
func (c *cli) checkGuardLog(add addCheck) {
	if c.dir == "" {
		return
	}
	rep, err := readGuardStats(c.dir, 7, 3, time.Now().UTC())
	if err != nil || rep.Total == 0 {
		return
	}
	top := ""
	if len(rep.ByRule) > 0 {
		top = fmt.Sprintf(" (top: %s %d)", rep.ByRule[0].Key, rep.ByRule[0].Count)
	}
	if rep.Total >= 25 {
		add(checkWarn, "guard log", "%d entries in last 7d%s: wp guard stats", rep.Total, top)
		return
	}
	add(checkInfo, "guard log", "%d entries in last 7d%s: wp guard stats", rep.Total, top)
}

// checkMemoryHealth flags bloated or duplicate Claude Code MEMORY.md files for
// the current repository (N25). Silent when Claude Code is absent or no
// MEMORY.md exists.
func (c *cli) checkMemoryHealth(add addCheck) {
	if !claudeInstalled() {
		return
	}
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	top := gitTop(cwd)
	if top == "" {
		return
	}
	memPath := claudeMemoryPath(top)
	if memPath == "" {
		return
	}
	raw, err := os.ReadFile(memPath) // #nosec G304 -- local Claude memory file for current repo
	if err != nil {
		return
	}
	lines, dupes := analyzeMemoryMD(string(raw))
	switch {
	case lines >= 180 && dupes > 0:
		add(checkWarn, "claude memory", "%s has %d lines (truncates at 200) and %d duplicate entries", memPath, lines, dupes)
	case lines >= 180:
		add(checkWarn, "claude memory", "%s has %d lines (Claude Code truncates MEMORY.md at 200 lines)", memPath, lines)
	case dupes >= 3:
		add(checkWarn, "claude memory", "%s has %d duplicate entries: trim stale duplicates", memPath, dupes)
	}
}

func claudeMemoryPath(repoTop string) string {
	root := claudeDir("")
	if root == "" || repoTop == "" {
		return ""
	}
	// Claude Code encodes project directories by replacing '/' and '.' with '-'.
	slug := strings.NewReplacer("/", "-", ".", "-").Replace(filepath.Clean(repoTop))
	return filepath.Join(root, "projects", slug, "memory", "MEMORY.md")
}

// analyzeMemoryMD counts non-empty lines and duplicate substantive bullet/prose
// lines (ignoring headings, horizontal rules, and blank lines).
func analyzeMemoryMD(text string) (totalLines, duplicates int) {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if text == "" {
		return 0, 0
	}
	seen := map[string]int{}
	inFence := false
	for _, raw := range lines {
		s := strings.TrimSpace(raw)
		if strings.HasPrefix(s, "```") {
			inFence = !inFence
			continue
		}
		if inFence || s == "" || strings.HasPrefix(s, "#") || s == "---" || s == "***" {
			continue
		}
		norm := strings.ToLower(strings.Join(strings.Fields(s), " "))
		if len(norm) < 20 {
			continue
		}
		seen[norm]++
		if seen[norm] > 1 {
			duplicates++
		}
	}
	return len(lines), duplicates
}

func (c *cli) checkRunner(signedIn bool, add addCheck) {
	installed, running, service := runnerServiceFn()
	switch {
	case !installed:
		add(checkInfo, "runner", "none on this machine: wp setup")
		return
	case !running:
		add(checkWarn, "runner", "%s: wp runner install", service)
		return
	}
	name := machineName()
	if c.state.Runner != nil && c.state.Runner.Name != "" {
		name = c.state.Runner.Name
	}
	var rs []runnerInfo
	if signedIn && c.do("GET", "/api/v1/runners", nil, &rs) == nil {
		if i := slices.IndexFunc(rs, func(r runnerInfo) bool { return r.Name == name }); i >= 0 && !rs[i].Online {
			add(checkWarn, "runner", "%s, but the server has %s offline: wp runner status", service, name)
			return
		}
	}
	add(checkOK, "runner", "%s", service)
}

func checkAgentCLIs(add addCheck) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var here []string
	for _, cl := range detectCLIsFn(ctx) {
		label := cmpStr(cliLabels[cl.Name], cl.Name)
		switch {
		case cl.SignedIn != nil && !*cl.SignedIn:
			add(checkWarn, "coding agent", "%s is signed out: %s", label, signInHint(cl.Name))
		case cl.SignedIn == nil && cl.Detail == probeUnfinished:
			add(checkWarn, "coding agent", "%s: %s", label, probeUnfinished)
		default:
			here = append(here, label)
		}
	}
	if len(here) > 0 {
		add(checkOK, "coding agents", "%s", strings.Join(here, ", "))
	}
}
