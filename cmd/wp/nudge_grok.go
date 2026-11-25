package main

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"git.jmann.me/claude/waypoint/internal/core"
)

const (
	grokProbeSecs = 300
	grokRunSecs   = 1800
	bashCapSecs   = 600
	bashDefSecs   = 120
)

// grokRun is a headless Grok run through Cursor found in a command.
type grokRun struct {
	secs     float64
	detached bool
}

// findGrokRuns looks through the commands, substitutions included. A `wait`
// in the same list holds the call open, so what it waits for isn't detached.
func findGrokRuns(cmds []*guardCmd, detached bool) []grokRun {
	waits := slices.ContainsFunc(cmds, func(c *guardCmd) bool { return len(c.Words) > 0 && c.Words[0] == "wait" })
	var runs []grokRun
	for _, c := range cmds {
		d := detached || (c.Detached && !waits)
		if found, secs := grokRunWords(c.Words); found {
			runs = append(runs, grokRun{secs, d})
		}
		for _, sub := range c.Subs {
			runs = append(runs, findGrokRuns(sub, d)...)
		}
	}
	return runs
}

// grokRunWords takes the wrappers off one command, keeping any timeout's
// duration, and says whether what is left runs Grok headless.
func grokRunWords(words []string) (found bool, secs float64) {
	for range 16 {
		for len(words) > 0 && isAssignment(words[0]) {
			words = words[1:]
		}
		if len(words) == 0 {
			return false, 0
		}
		args := words[1:]
		switch path.Base(words[0]) {
		case "nohup", "setsid", "time", "command", "exec":
			words = dropFlags(args)
		case "nice":
			words = dropFlags(args, "-n")
		case "stdbuf":
			words = dropFlags(args, "-i", "-o", "-e")
		case "env":
			words = dropFlags(args, "-u", "--unset", "-C", "--chdir")
		case "timeout":
			words = dropFlags(args, "-s", "-k", "--signal", "--kill-after")
			if len(words) == 0 {
				return false, 0
			}
			if s := sleepSeconds("sleep " + words[0]); s > 0 && (secs == 0 || s < secs) {
				secs = s
			}
			words = words[1:]
		case "cursor-agent", "agent":
			return isGrokPrint(args), secs
		default:
			return false, 0
		}
	}
	return false, 0
}

// isGrokPrint: cursor-agent's arguments ask for print mode and a Grok model.
func isGrokPrint(args []string) bool {
	if !slices.Contains(args, "-p") && !slices.Contains(args, "--print") {
		return false
	}
	for i, a := range args {
		if strings.HasPrefix(a, "--model=grok") || ((a == "--model" || a == "-m") && i+1 < len(args) && strings.HasPrefix(args[i+1], "grok")) {
			return true
		}
	}
	return false
}

// stopsAt is when the run is stopped, in seconds (0: never), and whether a
// limit was set short on purpose, for a quick check. bg is run_in_background;
// toolMs the call's timeout (0 if none). Bash's limits apply only to what the
// foreground call waits for, and the default one is nobody's choice.
func (run grokRun) stopsAt(bg bool, toolMs float64) (limit float64, quick bool) {
	limit, quick = run.secs, run.secs > 0 && run.secs <= grokProbeSecs
	if bg || run.detached {
		return limit, quick
	}
	bash := float64(bashDefSecs)
	if toolMs > 0 {
		bash = min(toolMs/1000, bashCapSecs)
		quick = quick || (run.secs == 0 && bash <= grokProbeSecs)
	}
	if limit == 0 || bash < limit {
		limit = bash
	}
	return limit, quick
}

// grokCutShort is the refusal for a Bash call that would stop a Grok review
// before it can finish, or "".
func grokCutShort(cmd string, bg bool, toolMs float64) string {
	if !strings.Contains(cmd, "grok") || !strings.Contains(cmd, "agent") {
		return ""
	}
	for _, run := range findGrokRuns(parseShell(cmd), false) {
		limit, quick := run.stopsAt(bg, toolMs)
		if limit == 0 || limit >= grokRunSecs || quick {
			continue
		}
		return fmt.Sprintf("Not run: Grok reviews often take over 25 minutes and Cursor prints only at the end, so this one would stop at %.0f minutes with nothing kept. "+
			"`wp audit pr|branch|commit|range -m WHAT` runs it on your runner, keeps the findings and tells you the result when it ends, a clean one too. "+
			"Its limit is %d minutes for a model run by a CLI like Cursor, and %d for the gateway's agent (the default), plus time for any helpers. "+
			"By hand: run_in_background with `timeout %d`. A quick check: `timeout %d` or less.",
			max(limit/60, 1), core.AuditCLITimeoutSecs/60, core.AuditGatewayTimeoutSecs/60, core.AuditCLITimeoutSecs, grokProbeSecs)
	}
	return ""
}
