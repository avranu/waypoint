package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// Waiting nudges (WP-191, WP-ADR16): an agent that sleeps and then checks
// pays a turn per check, its whole context read again each time. Claude Code
// tells a session when its background command exits, and `wp pr wait` or
// Monitor block without turns, so the note names those. One case is refused
// instead of noted: sleeping on a background command of the session's that is
// still running (a process still holds its output file open), since the
// notice that it exited is certain to come.

const sleepWaitSecs = 30 // a sleep this long before a check is a wait

var (
	reSleepFor   = lazyre.New(`\bsleep\s+(\d+(?:\.\d+)?)([smhd]?)(?:[\s;&|)]|$)`)
	reTaskOutput = lazyre.New(`(/[^\s'"]*/tasks/)([A-Za-z0-9]+)\.output\b`)
)

// sleepSeconds is how long the command's sleeps last together.
func sleepSeconds(cmd string) float64 {
	total := 0.0
	for _, m := range reSleepFor.FindAllStringSubmatch(cmd, -1) {
		n, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		switch m[2] {
		case "m":
			n *= 60
		case "h":
			n *= 3600
		case "d":
			n *= 86400
		}
		total += n
	}
	return total
}

// sleepWait: a long sleep before a check, outside a loop (a loop that polls
// is sleep-poll's; one that waits on a file blocks without turns).
func sleepWait(cmd string) bool {
	return !reLoop.MatchString(cmd) && sleepSeconds(cmd) >= sleepWaitSecs
}

// taskNudge is the note, or refusal (deny), for a Bash command or Read that
// looks at a background task's output file; kind is "" when there is none.
// subagent: the call is a sub-agent's, which isn't refused.
func taskNudge(tool, cmd, path string, subagent bool) (kind, text string, deny bool) {
	src := cmd
	if tool == "Read" {
		src = path
	}
	if len(src) > 20000 {
		return
	}
	for _, m := range reTaskOutput.FindAllStringSubmatch(src, -1) {
		file, id := m[1]+m[2]+".output", m[2]
		if fi, err := os.Lstat(file); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return "agent-output", "That file is sub-agent " + id + "'s whole transcript. Its report comes when it finishes; reading the transcript fills your context.", false
		}
		if !heldOpen(file) {
			continue // finished, or not knowable here: reading the output is fine
		}
		if tool == "Bash" && !subagent && reSleep.MatchString(cmd) {
			return "task-wait", "Background task " + id + " is still running; Claude Code tells you when it exits.", true
		}
		return "task-poll", "Background task " + id + " is still running; Claude Code tells you when it exits, so reading its output again is polling.", false
	}
	return
}

// heldOpen says whether a process of this user has file open, from the fds
// in /proc. False off Linux, and when looking takes longer than a hook may.
var heldOpen = func(file string) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	want := filepath.Clean(file)
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for _, p := range procs {
		name := p.Name()
		if name[0] < '0' || name[0] > '9' {
			continue
		}
		if time.Now().After(deadline) {
			return false
		}
		dir := "/proc/" + name + "/fd/"
		fds, err := os.ReadDir(dir)
		if err != nil {
			continue // another user's process
		}
		for _, fd := range fds {
			if l, err := os.Readlink(dir + fd.Name()); err == nil && l == want {
				return true
			}
		}
	}
	return false
}
