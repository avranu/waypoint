package main

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// Reading a session's screen for what the registry can't say (WP-1325).
//
// Claude Code's registry (sessions/PID.json) keeps status "busy" for as long
// as the session has background agents or tasks, though its main thread sits
// at the empty prompt and takes input: a session that delegates and waits
// was never idle to the runner, so a review typed beside its work was held
// for hours. The screen tells the two apart: while a turn runs Claude Code
// shows its "esc to interrupt" hint (with the spinner above the input box
// and in the footer), and at rest it doesn't, whatever list of agents or
// tasks hangs under the footer.

// turnShown says whether the screen shows a turn in progress: the hint
// "esc to interrupt" in the footer under the input box, or the spinner line
// among the last few lines above it (a narrow pane cuts the footer short).
// Only Claude Code's own lines count. The transcript above the box, what is
// typed in it and the list of agents under the footer are other people's
// words, and a session whose last message quoted the hint would otherwise
// never read as idle. A screen with no input box shows a turn too, since
// the box can't be told from a dialog.
func turnShown(screen string) bool {
	lines := strings.Split(strings.ReplaceAll(screen, "\r", ""), "\n")
	plain := make([]string, len(lines))
	for i, l := range lines {
		lines[i] = stripEscapes(l)
		plain[i] = strings.ToLower(strings.TrimSpace(lines[i]))
	}
	top := -1
	for i := len(lines) - 1; i > 0; i-- {
		if strings.HasPrefix(plain[i], "❯") && isTopBorder(strings.TrimSpace(lines[i-1])) {
			top = i - 1
			break
		}
	}
	if top < 0 {
		return true
	}
	// Above the box: the spinner, as captured, from column 0. A task list
	// or a tip may sit between it and the box.
	for i, seen := top-1, 0; i >= 0 && seen < spinnerLook; i-- {
		if plain[i] == "" {
			continue
		}
		seen++
		if spinnerLine.MatchString(lines[i]) {
			return true
		}
	}
	// Under the box (past its bottom edge): the footer's hint.
	foot := top + 2
	for i := top + 2; i < len(lines); i++ {
		if isRule(strings.TrimSpace(lines[i])) {
			foot = i + 1
			break
		}
	}
	for i := foot; i < len(plain); i++ {
		if strings.ContainsAny(firstRune(plain[i]), agentMarks) {
			continue // an agent's line: its words, not Claude Code's
		}
		if strings.Contains(plain[i], "esc to interrupt") {
			return true
		}
	}
	return false
}

// spinnerLook is how many lines above the input box are read for the
// spinner, and agentMarks what starts a line of the agents list under the
// footer ("● main", "◯ reviewer  Reading the diff").
const (
	spinnerLook = 6
	agentMarks  = "●◯○◉⏺"
)

func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return ""
}

// spinnerLine matches the line Claude Code shows above the input box while
// a turn runs: one symbol at column 0, a word ending in an ellipsis, then
// its timing ("* Embellishing… (5m 37s"). The rest line after a turn
// ("✻ Baked for 10h 21m") has no ellipsis and doesn't match, nor does an
// indented transcript or tool-output line, nor the assistant's own bullet.
var spinnerLine = regexp.MustCompile(`^[^\s\p{L}\p{N}●⏺] \S+… \(`)

// mainAtPrompt says whether the session's main thread waits at its prompt
// on this screen: the input box is on it, no turn is shown, and it is the
// same on a second look after settle (a turn that has just ended or begun
// shows within it). draft is what is typed in the box.
func mainAtPrompt(ctx context.Context, socket, pane string, settle time.Duration) (draft string, ok bool) {
	look := func() (string, bool) {
		screen, err := runTmux(ctx, socket, nil, "capture-pane", "-p", "-e", "-t", pane)
		if err != nil || turnShown(screen) {
			return "", false
		}
		return promptInput(screen)
	}
	d, found := look()
	if !found {
		return "", false
	}
	if !sleepCtx(ctx, settle) {
		return "", false
	}
	if d2, found := look(); !found || d2 != d {
		return "", false
	}
	return d, true
}

// waitTook waits for the session to take the prompt just sent into pane.
// Where the registry said the session was at its prompt, its status turning
// to busy shows that. Where it said busy before (wasBusy), the status can't
// change, so the screen does: the input box emptied and a turn began.
func waitTook(ctx context.Context, dir string, pid int, socket, pane string, wasBusy bool, limit time.Duration) bool {
	if !wasBusy {
		return waitBusy(ctx, dir, pid, limit)
	}
	took := func() bool {
		screen, err := runTmux(ctx, socket, nil, "capture-pane", "-p", "-e", "-t", pane)
		if err != nil {
			return false
		}
		draft, found := promptInput(screen)
		return found && draft == "" && turnShown(screen)
	}
	return waitUntil(ctx, limit, took)
}
