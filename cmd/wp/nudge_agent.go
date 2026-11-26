package main

import (
	"strconv"
	"strings"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const (
	noteAgentWait = "agent-wait"
	noteMonitor   = "monitor"

	sleepWaitMainText = "Sleeping before a check wastes turns: run the work in the background (you're told when it exits), or wait in one call (`wp pr wait N`, Monitor with an until-loop)."
	sleepWaitBgText   = "Sleeping before a check wastes turns: run the work with run_in_background: true and end your turn (you are woken when it exits), or run the wait itself as a background Bash (`timeout N bash -c 'until CHECK; do sleep 5; done'`) and end your turn."
	sleepWaitFgText   = "Sleeping before a check wastes turns: wait in ONE foreground Bash call with the timeout set to 600000 (`timeout 590 bash -c 'until CHECK; do sleep 5; done'`). Ending your turn instead ends your run and stops what you started."

	// The once-per-run note for a subagent that starts a background agent.
	agentWaitBgText = "To wait for the agents you start, run `wp agents wait` with run_in_background: true, then end your turn; it wakes you when they have all finished."
	agentWaitFgText = "To wait for the agents you start, run `wp agents wait` in the foreground with the Bash timeout set to 600000: it blocks until they have all finished, and nothing is re-read while it blocks. Ending your turn instead ends your run and stops what you started."

	monitorBgText = "A Monitor does not keep a subagent's run open. To wait, run the same check as a background Bash (`timeout N bash -c 'until CHECK; do sleep 5; done'` with run_in_background: true) and end your turn; you are woken when it exits."
	monitorFgText = "A Monitor does not keep a subagent's run open, and ending your turn ends your run. To wait, run the same check as ONE foreground Bash call with the timeout set to 600000 (`timeout 590 bash -c 'until CHECK; do sleep 5; done'`)."
)

// noteAgentWaitText is the note for a subagent's first background Agent start.
func noteAgentWaitText(who caller) string {
	if who == callerBg {
		return agentWaitBgText
	}
	return agentWaitFgText
}

// noteMonitorText is the note for a subagent's first Monitor call.
func noteMonitorText(who caller) string {
	if who == callerBg {
		return monitorBgText
	}
	return monitorFgText
}

var (
	reWaitTestRun = lazyre.New(`(?:\bwp|\[w\]p)\s+(testsum|slot\s+(?:run|ls))\b`)
	reWaitRequest = lazyre.New(`\bwp\s+(request|req)\b`)
	reWaitAgents  = lazyre.New(`\bwp\s+agents\b`)
	reTestsumCmd  = lazyre.New(`\bwp\s+testsum\s+--\s+[^;&|>)\n]+`)
)

// waitHint names the wp command that waits for what the command polls, ""
// when it shows nothing we know.
func waitHint(cmd string, who caller) string {
	switch {
	case reWaitTestRun.MatchString(cmd):
		hint := "`wp testsum` and `wp slot run` queue for the slot themselves, so there is nothing to wait for before running them."
		if run := reTestsumCmd.FindString(cmd); run != "" {
			run = strings.TrimSpace(run)
			if n := len(run); n > 2 && run[n-2] == ' ' && run[n-1] >= '0' && run[n-1] <= '9' { // the 2 of 2>&1
				run = strings.TrimSpace(run[:n-2])
			}
			hint += " Run it as `" + run + "` and read its summary."
		}
		if who == callerFg {
			return hint + " To wait for your own run, wait on that run's exit, never on `pgrep -f 'wp testsum'`, which waits for every run on the machine, other repos' included, and matches itself."
		}
		return hint + " To wait for your own run, wait on that run's exit (run it with run_in_background and end your turn), never on `pgrep -f 'wp testsum'`, which waits for every run on the machine, other repos' included, and matches itself."
	case reWaitRequest.MatchString(cmd):
		return "`wp request wait R-N` blocks until a person decides the request."
	case reWaitAgents.MatchString(cmd):
		if who == callerBg {
			return "`wp agents wait` blocks until your agents have stopped; run it with run_in_background: true and end your turn."
		}
		return "`wp agents wait` blocks until your agents have stopped; " + agentWaitFgHint(who)
	case reLoop.MatchString(cmd) && pidWait(cmd) != "":
		call := "`timeout 590 " + pidWait(cmd) + "`"
		switch who {
		case callerBg:
			return "Waiting on a process by polling it re-reads your context each time. Start what it waits on with run_in_background: true and end your turn (you are woken when it exits), or run " + call + " with run_in_background: true."
		case callerFg:
			return "Waiting on a process by polling it re-reads your context each time. Block until it exits in ONE foreground call with the Bash timeout set to 600000: " + call + "."
		}
		return "Waiting on a process by polling it re-reads your context each time. Run what it waits on with run_in_background: true and you are told when it exits; to block on one already running: " + call + "."
	case polledForge(cmd):
		return "`wp pr wait N` blocks on the server and prints failing logs; `wp ship` hands the wait to Waypoint."
	}
	return ""
}

// agentWaitFgHint ends a sentence about `wp agents wait` for a caller that
// doesn't wait in the background: the main thread may use either.
func agentWaitFgHint(who caller) string {
	if who == callerMain {
		return "run it in the background to be told when it exits."
	}
	return "run it in the foreground with the Bash timeout set to 600000."
}

// sleepWaitText is the note for a sleep before a check: the wp wait that
// fits what the command polls when it shows it, else the general advice for
// the kind of caller, which for a subagent never names Monitor.
func sleepWaitText(cmd string, who caller) string {
	const lead = "Sleeping before a check wastes turns"
	text, giveUp := "", ""
	if b := loopBoundSecs(stripHeredocs(cmd)); reLoop.MatchString(cmd) && b > 0 && b < sleepLoopMaxSecs {
		giveUp = "That loop gives up after " + strconv.Itoa(int(b)) + "s; when the wait is longer you call it again, a turn each time"
	}
	if h := waitHint(cmd, who); h != "" {
		text = lead + ". " + h
	} else {
		text = sleepWaitMainText
		switch who {
		case callerBg:
			text = sleepWaitBgText
		case callerFg:
			text = sleepWaitFgText
		}
		if check := loopCheck(cmd); check != "" {
			text = strings.ReplaceAll(strings.ReplaceAll(text, "timeout N", "timeout 590"), "CHECK", check)
			if who == callerMain {
				text += " Monitor command: `until " + check + "; do sleep 5; done`."
			}
		}
	}
	if giveUp != "" {
		text = giveUp + strings.TrimPrefix(text, lead)
	}
	return text
}
