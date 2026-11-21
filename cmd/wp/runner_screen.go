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

// inputBox is where Claude Code's input box sits on a captured screen:
// lines as captured (escapes removed), plain the same trimmed, top the
// index of the box's top border, at the line of its "❯", and bottom the
// index of its bottom edge (a rule, or a line starting with ╰), -1 when
// the box isn't closed on the screen (WP-1325).
type inputBox struct {
	lines, plain    []string
	top, at, bottom int
}

// findInputBox locates the input box: the last line that starts with ❯
// right under a top border, so a ❯ in a status line or the transcript isn't
// taken for it. found is false when there is no such line. promptInput and
// turnShown both read the box through this, so they agree on its edges.
func findInputBox(screen string) (b inputBox, found bool) {
	raw := strings.Split(strings.ReplaceAll(screen, "\r", ""), "\n")
	b.lines = make([]string, len(raw))
	b.plain = make([]string, len(raw))
	for i, l := range raw {
		b.lines[i] = stripEscapes(l)
		b.plain[i] = strings.TrimSpace(b.lines[i])
	}
	b.at, b.bottom = -1, -1
	for i := len(raw) - 1; i > 0; i-- {
		if strings.HasPrefix(b.plain[i], "❯") && isTopBorder(b.plain[i-1]) {
			b.at, b.top = i, i-1
			break
		}
	}
	if b.at < 0 {
		return b, false
	}
	for i := b.at + 1; i < len(raw); i++ {
		if isRule(b.plain[i]) || strings.HasPrefix(b.plain[i], "╰") {
			b.bottom = i
			break
		}
	}
	return b, true
}

// turnShown says whether the screen shows a turn in progress: the spinner
// line above the input box, or the hint "esc to interrupt" in the footer
// under it (a narrow pane cuts the footer short, so the spinner is read
// first). Only Claude Code's own lines count. The transcript above the box,
// what is typed in it and the list of agents under the footer are other
// people's words, and a session whose last message quoted the hint would
// otherwise never read as idle. A screen with no input box, or one whose
// box isn't closed, shows a turn too, since it can't be told from a dialog.
//
// The spinner is found by walking up from the box's top border, with no
// count of lines: a blank line or an indented one (the spinner's own block:
// a tip, a task list of any length) is passed over; a column-0 spinner line
// is the turn; a column-0 bullet (the assistant's message or a tool call) or
// a person's prompt (>, ›, ❯) is where the transcript starts, and the walk
// stops; any other column-0 line (a notice such as "✔ Update installed", the
// rest line, a recap) is passed over, at most four of them.
func turnShown(screen string) bool {
	b, found := findInputBox(screen)
	if !found || b.bottom < 0 {
		return true
	}
	passed := 0
	for i, n := b.top-1, 0; i >= 0 && n < spinnerWalk; i, n = i-1, n+1 {
		if b.plain[i] == "" {
			continue
		}
		first := firstRune(b.lines[i])
		if strings.TrimSpace(first) == "" {
			continue // indented: the spinner's block, or a message's continuation
		}
		if spinnerLine.MatchString(b.lines[i]) {
			return true
		}
		if strings.Contains(transcriptMarks, first) {
			break
		}
		if passed++; passed > maxNotices {
			break
		}
	}
	// Under the box (past its bottom edge): the footer's hint.
	for i := b.bottom + 1; i < len(b.plain); i++ {
		low := strings.ToLower(b.plain[i])
		if strings.ContainsAny(firstRune(low), agentMarks) {
			continue // an agent's line: its words, not Claude Code's
		}
		if strings.Contains(low, "esc to interrupt") {
			return true
		}
	}
	return false
}

// spinnerWalk bounds the walk above the box, maxNotices is how many column-0
// lines that aren't the spinner it passes over, transcriptMarks what starts
// the transcript (the assistant's bullet and a person's prompt), and
// agentMarks what starts a line of the agents list under the footer
// ("● main", "◯ reviewer  Reading the diff").
const (
	spinnerWalk     = 200
	maxNotices      = 4
	transcriptMarks = "●⏺>›❯"
	agentMarks      = "●◯○◉⏺"
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
// indented transcript or tool-output line, nor the assistant's own bullet,
// nor a person's prompt ("> Fixing… (it broke)": >, › and ❯ aren't symbols
// here).
var spinnerLine = regexp.MustCompile(`^[^\s\p{L}\p{N}●⏺>›❯] \S+… \(`)

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
// to busy shows that. Where it said busy before (byScreen), the status can't
// change, so the screen does: the input box emptied and a turn began.
func waitTook(ctx context.Context, dir string, pid int, socket, pane string, byScreen bool, limit time.Duration) bool {
	if !byScreen {
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
