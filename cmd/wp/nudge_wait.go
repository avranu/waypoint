package main

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

const sleepWaitSecs = 30

// sleepLoopMaxSecs is the bound of a waiting loop that is the one-call form
// the notes recommend (`timeout 590 bash -c 'until CHECK; do sleep 5; done'`
// with the Bash timeout at 600000). A loop that gives up sooner than this is
// called again by the agent, a turn each time; one that never gives up is cut
// off by the Bash timeout, then called again.
const sleepLoopMaxSecs = 540

var (
	reSleepFor   = lazyre.New(`\bsleep\s+(\d+(?:\.\d+)?)([smhd]?)(?:[\s;&|)]|$)`)
	reTaskOutput = lazyre.New(`(/[^\s'"]*/tasks/)([A-Za-z0-9]+)\.output\b`)

	reTimeoutN  = lazyre.New(`(?:^|[\s;&|(])timeout\s+(?:-\S+\s+)*(\d+(?:\.\d+)?)([smhd]?)\b`)
	reLoopCount = lazyre.New(`\bseq\s+(?:\d+\s+)?(\d+)\b|\{\d+\.\.(\d+)\}|for\s*\(\(\s*\w+\s*=\s*\d+\s*;\s*\w+\s*<=?\s*(\d+)`)
	reForList   = lazyre.New(`\bfor\s+\w+\s+in\b`)
	reWhileRead = lazyre.New(`\bwhile\s+(?:IFS=\S*\s+)?read\b`)
	reUntilHead = lazyre.New(`\b(until|while)\s+([^;\n]+?)\s*;\s*do\b`)
	rePgrepWait = lazyre.New(`\bpgrep\s+(?:-[a-zA-Z]+\s+)*('[^']+'|"[^"$]+"|[^\s;&|)'"]+)`)
	reKill0     = lazyre.New(`\bkill\s+-0\s+(\d+)|/proc/(\d+)\b`)
	reLocalHost = lazyre.New(`(?:localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1\])`)
	reTestsumWd = lazyre.New(`\bpgrep\b[^\n;|&]*(?:\bwp|\[w\]p)\s+(?:testsum|slot)\b|\bwp\s+slot\s+ls\b`)
)

// stripHeredocs drops the bodies of here-documents: a script being written
// isn't run by the command, so a loop in it isn't one the command waits in.
func stripHeredocs(cmd string) string {
	if !strings.Contains(cmd, "<<") {
		return cmd
	}
	var out []string
	end := ""
	for _, line := range strings.Split(cmd, "\n") {
		if end != "" {
			if strings.TrimSpace(line) == end {
				end = ""
			}
			continue
		}
		out = append(out, line)
		if i := strings.Index(line, "<<"); i >= 0 && !strings.HasPrefix(line[i:], "<<<") {
			w := strings.TrimLeft(line[i+2:], "-~ ")
			if j := strings.IndexAny(w, " ;|&)"); j >= 0 {
				w = w[:j]
			}
			end = strings.Trim(w, `'"`)
		}
	}
	return strings.Join(out, "\n")
}

// loopBoundSecs is how long the loops in cmd run at most: the shorter of an
// outer `timeout N` (one whose own command holds the loop and its sleep, not
// one that limits a single check) and the iteration count times the sleeps; +Inf for a
// while or until that nothing bounds; 0 for a loop that isn't a wait (over a
// list of things, or reading lines).
func loopBoundSecs(cmd string) float64 {
	if reWhileRead.MatchString(cmd) {
		return 0
	}
	bound := math.Inf(1)
	for _, m := range reTimeoutN.FindAllStringSubmatchIndex(cmd, -1) {
		end, _ := shellSpan(cmd[m[1]:], 0)
		if run := cmd[m[1] : m[1]+end]; !reLoop.MatchString(run) || !reSleep.MatchString(run) {
			continue
		}
		n, _ := strconv.ParseFloat(cmd[m[2]:m[3]], 64)
		bound = math.Min(bound, n*map[string]float64{"": 1, "s": 1, "m": 60, "h": 3600, "d": 86400}[cmd[m[4]:m[5]]])
	}
	if m := reLoopCount.FindStringSubmatch(cmd); m != nil {
		for _, g := range m[1:] {
			if g != "" {
				n, _ := strconv.ParseFloat(g, 64)
				bound = math.Min(bound, n*sleepSeconds(cmd))
			}
		}
	} else if reForList.MatchString(cmd) && !reUntilHead.MatchString(cmd) {
		return 0
	}
	return bound
}

// sleepLoop: a loop that sleeps and checks again, and waits for something
// for longer than a quick answer: it never gives up, or gives up before the
// 590 seconds one call can wait, or waits on the test slots by their
// processes. A loop that is the one-call form (`timeout 590 ...`), one that
// ends within sleepWaitSecs (a local server a script starts), and one that
// goes through a list (a pause between requests) are left alone.
func sleepLoop(cmd string) bool {
	cmd = stripHeredocs(cmd)
	if !reLoop.MatchString(cmd) || sleepSeconds(cmd) <= 0 || reWPWait.MatchString(cmd) {
		return false
	}
	if reTestsumWd.MatchString(cmd) {
		return true
	}
	b := loopBoundSecs(cmd)
	return b > sleepWaitSecs && (b < sleepLoopMaxSecs || math.IsInf(b, 1))
}

// loopCheck is the until-condition of the command's loop as one that can be
// run by itself ("" when it reads a variable the command set or the loop
// isn't a plain until or while).
func loopCheck(cmd string) string {
	m := reUntilHead.FindStringSubmatch(stripHeredocs(cmd))
	if m == nil || strings.Contains(m[2], "$") || strings.Contains(m[2], "`") {
		return ""
	}
	c := strings.TrimSpace(m[2])
	switch {
	case m[1] == "until":
	case strings.HasPrefix(c, "! "):
		c = strings.TrimPrefix(c, "! ")
	case strings.HasPrefix(c, "[ ! ") && strings.HasSuffix(c, " ]"):
		c = "[ " + strings.TrimPrefix(c, "[ ! ")
	default:
		c = "! " + c
	}
	return strings.ReplaceAll(c, "'", `'\''`)
}

// pidWait names the one call that blocks until a process the loop waits on
// has exited, "" when the loop doesn't wait on one.
func pidWait(cmd string) string {
	cmd = stripHeredocs(cmd)
	if m := reKill0.FindStringSubmatch(cmd); m != nil {
		return "tail --pid=" + m[1] + m[2] + " -f /dev/null"
	}
	if m := rePgrepWait.FindStringSubmatch(cmd); m != nil {
		return "tail --pid=$(pgrep -of " + m[1] + ") -f /dev/null"
	}
	return ""
}

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
// who: the call's maker. A subagent started in the background is refused too
// (WP-1443): one with a background command of its own running is kept open
// and woken when that exits, so its texts say to end the turn. One started in
// the foreground (or not known to be a background one) is only noted: its
// shells are reaped when it answers, so ending the turn would stop the task,
// and it is told to wait in one foreground call instead.
func taskNudge(tool, cmd, path string, who caller) (kind, text string, deny bool) {
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
			continue
		}
		switch who {
		case callerBg:
			if tool == "Bash" && reSleep.MatchString(cmd) {
				return "task-wait", "Background task " + id + " is still running; sleeping on it re-reads your whole context for nothing. End your turn: you are woken when it exits.", true
			}
			return "task-poll", "Background task " + id + " is still running; reading its output again is polling. End your turn: you are woken when it exits.", false
		case callerFg:
			kind := "task-poll"
			if tool == "Bash" && reSleep.MatchString(cmd) {
				kind = "task-wait"
			}
			return kind, "Background task " + id + " is still running, and reading or sleeping on it again re-reads your whole context. Wait for it in ONE foreground Bash call with the timeout set to 600000 (`timeout 590 bash -c 'until CHECK; do sleep 5; done'`); ending your turn instead ends your run and stops the task.", false
		}
		if tool == "Bash" && reSleep.MatchString(cmd) {
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
			continue
		}
		for _, fd := range fds {
			if l, err := os.Readlink(dir + fd.Name()); err == nil && l == want {
				return true
			}
		}
	}
	return false
}
