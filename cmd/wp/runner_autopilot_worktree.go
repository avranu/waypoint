package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Every session autopilot starts gets its own worktree (WP-ADR57): the
// restart runs `claude --worktree NAME` from the repo's main checkout, so the
// agent never thinks about where to work and the checkout stays a person's.
// When the session is done with, and before any new start, the repo's
// finished worktrees go, by wp tidy's rules. Nothing is ever forced: work a
// worktree still holds stays there and the person hears of it once.

// autopilotWorktrees is where Claude Code keeps --worktree checkouts, below
// the repo's root (excluded from git by Claude Code).
var autopilotWorktrees = filepath.Join(".claude", "worktrees")

// autopilotWorktreeBranch is the branch Claude Code gives worktree name.
func autopilotWorktreeBranch(name string) string { return "worktree-" + name }

// autopilotSetupFile is a repo's dependency setup, run in each new worktree
// before the session gets its prompt.
var autopilotSetupFile = filepath.Join(".waypoint", "worktree-setup")

// Seams for tests: which folders processes run in, and how long setup has.
var (
	procCwds            = func() ([]string, error) { return cwdsUnder("/proc") }
	autopilotSetupLimit = 10 * time.Minute
)

// cwdsUnder reads the folder of every process in a /proc at root. A single
// process can go unread (it has just exited, or is another user's), but none
// at all means there is no view: this process is always one of them.
func cwdsUnder(root string) ([]string, error) {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		if d, err := os.Readlink(filepath.Join(root, e.Name(), "cwd")); err == nil {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no process's folder could be read in %s", root)
	}
	return out, nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p) // #nosec G703 -- only whether the repo's setup file exists
	return err == nil && fi.IsDir()
}

// keptReportCap is how many kept worktrees one job reports.
const keptReportCap = 20

// planWorktree works out, before the old session is touched, where the new
// one starts. It sets rl.root when the session's folder is in a git repo
// this runner may work in (the sweep needs no more), and rl.worktree when
// Claude Code can make a worktree there. The answer is a warning when the
// session has to start without one.
func (p *runnerProc) planWorktree(ctx context.Context, rl *relaunch) string {
	out, err := tidyGit(ctx, rl.dir, "worktree", "list", "--porcelain")
	if err != nil {
		return "no worktree for this session: its folder isn't in a git repository"
	}
	wts := parseWorktrees(out)
	if len(wts) == 0 || wts[0].bare {
		return "no worktree for this session: the repository has no main checkout to start from"
	}
	root, err := resolveDir(wts[0].path, p.allowed)
	if err != nil {
		return "no worktree for this session: " + err.Error()
	}
	rl.root = root
	if !claudeTrusts(root) {
		return "no worktree for this session: Claude Code hasn't been trusted in " + root + " (run claude there once and accept)"
	}
	// The worktree the old session ran in, if it was one of autopilot's kind.
	// Only when the new session leaves it: otherwise it is still in use.
	if top := gitTop(rl.dir); top != "" {
		top = tidyReal(top)
		if base := filepath.Join(root, autopilotWorktrees); top != base && inside(top, base) {
			rl.ended = top
		}
	}
	rl.worktree = true
	rl.dir = root
	if rl.respawn {
		return ""
	}
	// `claude --worktree` works from the folder it starts in, and the pane's
	// shell may have stayed anywhere.
	if rl.shellDir == "" || tidyReal(rl.shellDir) != root {
		rl.cdTo = root
	}
	return ""
}

// claudeTrusts says whether Claude Code's trust dialog was accepted for dir
// or a folder above it: --worktree fails at once where it wasn't. A config
// that can't be read counts as trusted, and the failure then shows in the
// pane.
func claudeTrusts(dir string) bool {
	path := filepath.Join(claudeConfigDir(), ".claude.json")
	if os.Getenv("CLAUDE_CONFIG_DIR") == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return true
		}
		path = filepath.Join(home, ".claude.json")
	}
	b, err := os.ReadFile(path) // #nosec G304 -- Claude Code's own config file
	if err != nil {
		return true
	}
	var cfg struct {
		Projects map[string]struct {
			Trusted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return true
	}
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if cfg.Projects[d].Trusted {
			return true
		}
		if d == filepath.Dir(d) {
			return false
		}
	}
}

// worktreeName picks the worktree for task key: its own name when free or
// already a worktree (Claude Code carries on in one that exists), else the
// next name whose branch is free. A branch left behind by a removed worktree
// holds work that isn't merged, so it is never handed to --worktree. "" when
// no name is free.
func worktreeName(ctx context.Context, root, key string) string {
	slug := strings.ToLower(key)
	for i := 1; i <= 50; i++ {
		name := slug
		if i > 1 {
			name = fmt.Sprintf("%s-%d", slug, i)
		}
		if dirExists(filepath.Join(root, autopilotWorktrees, name)) {
			return name
		}
		// rev-parse -q --verify exits 1 when the ref is missing; any other
		// failure says nothing about the branch, so the name isn't free.
		out, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "-q", "--verify", "refs/heads/"+autopilotWorktreeBranch(name)).CombinedOutput() // #nosec G204 G702 -- fixed git subcommand
		_ = out
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return name
		}
		if err != nil {
			return ""
		}
	}
	return ""
}

// sweepWorktrees removes the repo's finished worktrees (wp tidy's rules)
// once the old session has exited, sparing the one the new session is about
// to use. It answers with what stayed and the person hasn't heard about.
// Trouble here never stops the new session from starting.
func (p *runnerProc) sweepWorktrees(ctx context.Context, run *jobRun, in autopilotJob, rl *relaunch, spare string) []string {
	say := func(format string, a ...any) { fmt.Fprintf(run.out, format+"\n", a...) }
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := tidyGit(ctx, rl.root, "fetch", "--prune", "--quiet"); err != nil {
		say("worktree sweep: fetch failed (%v); judging by the refs as they are", err)
	}
	p.sweepPushEnded(ctx, run, rl) // WP-843: the ended session's commits go to the forge
	repo, _, _ := gitInfo(rl.root)
	merged, err := p.c.tidyMergedPRs(repo)
	if err != nil {
		say("worktree sweep: Waypoint's merged pull requests unavailable: %v", err)
	}
	o := tidyOpts{worktrees: true, worktreeBranches: true, ended: rl.ended}
	rep, err := tidyScan(ctx, rl.root, merged, o)
	if err != nil {
		say("worktree sweep: %v", err)
		return nil
	}
	// Without a view of the running processes or of what other sessions hold,
	// nothing is known to be unused: remove nothing.
	cwds, err := procCwds()
	if err != nil {
		say("worktree sweep: can't see which folders processes are in (%v); removing nothing", err)
		return nil
	}
	if in.HeldUnknown {
		say("worktree sweep: what other sessions hold couldn't be read; removing nothing")
		return nil
	}
	for _, r := range rep.Rows {
		if r.Verdict != tidyRemovable || r.prunable {
			continue
		}
		switch stays, why := worktreeInUse(r, cwds, in.Held, spare); {
		case stays != "":
			r.Verdict, r.Reason, r.stays = tidyKeep, why, stays
		case why != "":
			r.Verdict, r.Reason = tidyKeep, why
		}
	}
	tidyApply(ctx, rl.root, rep, o)
	var kept []*tidyRow
	base := filepath.Join(rl.root, autopilotWorktrees)
	for _, r := range rep.Rows {
		switch {
		case r.Action != "":
			say("worktree sweep: %s: %s", r.name(), r.Action)
		case r.stays != "" && inside(tidyReal(r.Path), base):
			say("worktree sweep: kept %s: %s", r.name(), r.Reason)
			kept = append(kept, r)
		}
	}
	return p.c.keptOnce(kept)
}

// worktreeInUse says why a worktree tidy would remove has to stay. stays is
// set when the person should hear about it; why alone keeps it quietly.
func worktreeInUse(r *tidyRow, cwds []string, held []autopilotHeld, spare string) (stays, why string) {
	path := tidyReal(r.Path)
	if spare != "" && inside(tidyReal(spare), path) { // spare may be a folder within it
		return "", "the next task's worktree"
	}
	for _, d := range cwds {
		if inside(tidyReal(d), path) {
			return "in use", "a process is running in it"
		}
	}
	key := ""
	if m := branchKeyRE.FindStringSubmatch(r.Branch); m != nil {
		key = strings.ToUpper(m[1])
	}
	for _, h := range held {
		if (h.Cwd != "" && inside(tidyReal(h.Cwd), path)) || (key != "" && strings.EqualFold(h.Key, key)) {
			return "held", h.Key + " is still held by a session that went quiet"
		}
	}
	return "", ""
}

// keptOnce filters rows to the ones not reported yet, and remembers them, so
// a worktree left in place is mentioned once rather than on every start.
func (c *cli) keptOnce(rows []*tidyRow) []string {
	var out []string
	_ = c.updateState(func(s *State) {
		out = nil
		if s.WorktreesKept == nil {
			s.WorktreesKept = map[string]time.Time{}
		}
		for k := range s.WorktreesKept {
			if path, _, _ := strings.Cut(k, "\x00"); !dirExists(path) {
				delete(s.WorktreesKept, k)
			}
		}
		for _, r := range rows {
			k := r.Path + "\x00" + r.stays
			if _, told := s.WorktreesKept[k]; told || len(out) >= keptReportCap {
				continue
			}
			s.WorktreesKept[k] = time.Now()
			out = append(out, r.name()+": "+r.Reason)
		}
	})
	return out
}

// runWorktreeSetup runs the repo's setup file, if it has one, in the new
// worktree. It answers with a warning if that failed: the agent can still
// work, and sees what's missing. No Waypoint token reaches it.
func runWorktreeSetup(ctx context.Context, run *jobRun, path, session, agent string) string {
	if !fileExists(filepath.Join(path, autopilotSetupFile)) {
		return ""
	}
	fmt.Fprintf(run.out, "running %s in %s\n", autopilotSetupFile, path)
	if err := worktreeSetup(ctx, run.out, path, autopilotEnv(os.Environ(), session, agent, "", "")); err != nil {
		w := autopilotSetupFile + " failed in the new worktree: " + err.Error()
		fmt.Fprintf(run.out, "%s\n", w)
		return w
	}
	return ""
}

// keepWorktreeDialog: the screen shows Claude Code's question on leaving a
// worktree that has commits, with Keep highlighted. Its other answer deletes
// the worktree and its branch, so only this exact state gets an Enter.
func keepWorktreeDialog(screen string) bool {
	asked, keep := false, false
	for _, l := range strings.Split(screen, "\n") {
		l = strings.TrimSpace(strings.ReplaceAll(stripEscapes(l), "\u00a0", " "))
		asked = asked || strings.Contains(l, "Exiting worktree session")
		keep = keep || strings.HasPrefix(l, "❯ 1. Keep worktree")
	}
	return asked && keep
}
