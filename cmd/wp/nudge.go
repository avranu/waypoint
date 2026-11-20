package main

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Nudges (ADR2 §5, WP-47): a PreToolUse(Bash) hook that names the better
// tool in one line when an agent reaches for a worse one. It is a note added
// to what the agent sees, never a refusal: the four refusals that used to
// live here (task-wait, nudge_wait.go; a Grok review that would be cut short,
// nudge_grok.go; pgrep-self; worktree-place) are emitted by the guard hook
// (refuse.go, WP-1411), because Claude Code lets a call through when a hook
// outlasts its timeout. It needs no network: it must finish in a few
// milliseconds, since it runs before every Bash call.

// nudgeRule is one substitution. match gets the command (and the git remote's
// host, looked up only by rules that set needsRemote).
type nudgeRule struct {
	kind        string
	needsRemote *lazyre.Regexp // set: the rule reads the remote, looked up only for commands this matches
	match       func(cmd, remoteHost string) bool
	text        string
}

var (
	reLoop       = lazyre.New(`\b(while|until|for)\b`)
	reSleep      = lazyre.New(`\bsleep\s+\d`)
	rePolled     = lazyre.New(`\b(curl|wget|gh|ci_wait|ci_wait\.py)\b|\bwp\s+pr\b|\bgit\s+ls-remote\b`)
	reCIWait     = lazyre.New(`\bci_wait(\.py)?\b`)
	reWPWait     = lazyre.New(`\bwp\s+pr\s+wait\b`)
	reFetcher    = lazyre.New(`(^|[\s;&|(])(curl|wget)\s`)
	reForgeAPI   = lazyre.New(`(/api/v1/repos/\S+|api\.github\.com/repos/\S+)`)
	reForgeThing = lazyre.New(`/(pulls?|statuses|status|commits|check-runs|check-suites)\b`)
	reGhPR       = lazyre.New(`(^|[;&|(\n]|\bxargs\s)\s*gh\s+pr\b`)
	reBranchDel  = lazyre.New(`\bgit\s+branch\s+(-[a-zA-Z]*D[a-zA-Z]*|--delete\s+--force|-d\s+--force|--force\s+--delete)\s+[^\s;&|]+\s+[^\s;&|-]`)
	reBranchLoop = lazyre.New(`\b(xargs|for|while)\b[^\n]*\bgit\s+branch\s+(-[a-zA-Z]*D\b|--delete)`)
	reWorktreeRm = lazyre.New(`\bgit\s+worktree\s+remove\b`)
)

var nudgeRules = []nudgeRule{
	{kind: "sleep-poll", match: func(cmd, _ string) bool {
		if reWPWait.MatchString(cmd) {
			return false
		}
		return reCIWait.MatchString(cmd) || (reSleep.MatchString(cmd) && rePolled.MatchString(cmd) && (reLoop.MatchString(cmd) || sleepSeconds(cmd) >= sleepWaitSecs))
	}, text: "`wp pr wait N` blocks on the server and prints failing logs; `wp ship` hands the wait to Waypoint."},
	{kind: "forge-api", match: func(cmd, _ string) bool {
		return reFetcher.MatchString(cmd) && forgeAPIPulls(cmd)
	}, text: "Use `wp pr show|check|log N`."},
	{kind: "gh-on-forgejo", needsRemote: reGhPR, match: func(cmd, host string) bool {
		return reGhPR.MatchString(cmd) && host != "" && host != "github.com"
	}, text: "This repo is on Forgejo: use `wp pr`."},
	{kind: "branch-sweep", match: func(cmd, _ string) bool {
		if reBranchDel.MatchString(cmd) || reBranchLoop.MatchString(cmd) {
			return true
		}
		return reWorktreeRm.MatchString(cmd) &&
			(len(reWorktreeRm.FindAllString(cmd, -1)) > 1 || reLoop.MatchString(cmd) || strings.Contains(cmd, "xargs"))
	}, text: "`wp tidy` shows what's safe to remove."},
	{kind: "sleep-wait", match: func(cmd, _ string) bool { return sleepWait(cmd) },
		text: "Sleeping before a check wastes turns: run the work in the background (you're told when it exits), or wait in one call (`wp pr wait N`, Monitor with an until-loop)."},
}

// forgeAPIPulls: a URL of a forge API for pull requests, statuses or commits.
func forgeAPIPulls(cmd string) bool {
	for _, u := range reForgeAPI.FindAllString(cmd, -1) {
		if reForgeThing.MatchString(u) {
			return true
		}
	}
	return false
}

// nudgeFor returns the rule a Bash command matches (the first), or nil.
// remote looks up the host of the repo's origin when a rule needs it.
func nudgeFor(cmd string, remote func() string) *nudgeRule {
	if len(cmd) > 20000 { // a heredoc of code, not a polling command
		return nil
	}
	host, looked := "", false
	for i := range nudgeRules {
		r := &nudgeRules[i]
		h := ""
		if r.needsRemote != nil {
			if !r.needsRemote.MatchString(cmd) {
				continue
			}
			if !looked {
				host, looked = remote(), true
			}
			h = host
		}
		if r.match(cmd, h) {
			return r
		}
	}
	return nil
}

// originHost is the host of the origin remote of the repo at dir; empty when
// there is none.
func originHost(dir string) string {
	cmd := exec.Command("git", "-C", dir, "remote", "get-url", "origin") // #nosec G204 -- fixed arguments; dir is the hook's cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return remoteHost(strings.TrimSpace(string(out)))
}

// remoteHost is the host of a git remote URL (https, ssh or scp-like).
func remoteHost(remote string) string {
	r := remote
	if i := strings.Index(r, "://"); i >= 0 {
		r = r[i+3:]
	}
	if i := strings.Index(r, "@"); i >= 0 {
		r = r[i+1:]
	}
	if i := strings.IndexAny(r, "/:"); i >= 0 {
		r = r[:i]
	}
	return strings.ToLower(r)
}

// nudgeOutput is the PreToolUse reply: the note as context, no decision.
func nudgeOutput(text string) []byte {
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": "PreToolUse", "additionalContext": text}})
	return append(b, '\n')
}

// nudgeStat counts a nudge in the state file: how often each kind was shown.
// The server doesn't take these yet; WP-47's "how often it is followed"
// compares them with the later use of wp pr wait in the usage ledger.
type nudgeStat struct {
	Shown int       `json:"shown"`
	Last  time.Time `json:"last"`
}

// hookClaudeNudge handles one PreToolUse(Bash|Read) event. Silent unless the
// call matches a rule; never fails and never refuses (WP-1411): a call the
// guard hook refuses (hookRefusal) gets no note here.
func hookClaudeNudge(c *cli, stdin io.Reader, stdout io.Writer) {
	var in hookEventInput
	if err := json.NewDecoder(io.LimitReader(stdin, 4<<20)).Decode(&in); err != nil || (in.ToolName != "Bash" && in.ToolName != "Read") {
		return
	}
	var ti struct {
		Command  string  `json:"command"`
		FilePath string  `json:"file_path"`
		Limit    int     `json:"limit"`
		Bg       bool    `json:"run_in_background"`
		Timeout  float64 `json:"timeout"`
	}
	if json.Unmarshal(in.ToolInput, &ti) != nil || (ti.Command == "" && ti.FilePath == "") {
		return
	}
	cwd := in.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	var r *nudgeRule
	// A call the guard hook refuses (WP-1411) gets no note here, and does not
	// use up a pause tell: the refusal is what the agent must read. Its
	// search step is still counted below.
	// The task check scans /proc: do it once for the refusal and the note.
	type taskResult struct {
		kind, text string
		deny       bool
	}
	var taskRes *taskResult
	task := func(cmd string) (string, string, bool) {
		if taskRes == nil {
			k, text, deny := taskNudge(in.ToolName, cmd, ti.FilePath, in.AgentID != "")
			taskRes = &taskResult{k, text, deny}
		}
		return taskRes.kind, taskRes.text, taskRes.deny
	}
	rk, _ := hookRefusalWith(in, task)
	refused := rk != ""
	// A person's pause (WP-146): told on the first call it reaches, then
	// every few minutes, ahead of any other nudge.
	// Not a subagent's call: it would use up the parent's slot, and the
	// hand-off instruction is the parent's to follow.
	if in.AgentID == "" && !refused {
		if text := c.pauseTell(in.SessionID, hookLateAfter(hookNudgeTimeout)); text != "" {
			r = &nudgeRule{kind: "paused", text: text}
		}
	}
	if r == nil && !refused && !ti.Bg {
		if kind, text, _ := task(ti.Command); kind != "" {
			r = &nudgeRule{kind: kind, text: text}
		}
	}
	if r == nil && !refused && in.ToolName == "Bash" {
		r = nudgeFor(ti.Command, func() string { return originHost(cwd) })
		if r != nil && ti.Bg && (r.kind == "sleep-poll" || r.kind == "sleep-wait") {
			r = nil // a waiter in the background costs no turns
		}
	}
	if r == nil && !refused {
		if kind, text := bigReadFor(in.ToolName, cwd, ti.FilePath, ti.Limit, ti.Command); kind != "" {
			r = &nudgeRule{kind: kind, text: text}
		}
	}
	// Counted on every search or slice, nudged or not, so the chain is whole;
	// told only when nothing else was (WP-183).
	// The search_nudge experiment's "off" arm counts and never tells (WP-254).
	canTell := r == nil && !refused && c.sessionArm(cwd, in.SessionID, expSearchNudge) != "off"
	if !ti.Bg && c.chainNudge(&in, cwd, chainStep(in.ToolName, ti.Command, ti.FilePath), canTell) {
		r = &nudgeRule{kind: chainKind, text: chainText}
	}
	if r == nil {
		return
	}
	_, _ = stdout.Write(nudgeOutput(r.text))
	c.countNudge(r.kind, hookCountWait(hookNudgeTimeout))
}
