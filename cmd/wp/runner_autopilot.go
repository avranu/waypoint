package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Autopilot jobs (WP-ADR9): the session finished its task, so start the next
// one fresh in its place. Claude Code gives no way to restart a running
// session from outside, but a session in tmux reads what is typed into its
// pane: type /exit there and start Claude Code again, in a worktree of its
// own (runner_autopilot_restart.go, WP-ADR57). Where that can't be done the
// session is left alone and the person hears why.
// Then wait until the new session is ready, run wp next for it as the agent,
// and paste the answer as its prompt, the way the person would. Nothing is typed over a
// draft, into a busy session, or a dialog.

type autopilotJob struct {
	CLIID string `json:"cli_id"`
	Dir   string `json:"dir"`
	Key   string `json:"key"`
	Title string `json:"title"`
	Done  string `json:"done"`
	Agent string `json:"agent"`
	Step  int64  `json:"step"`
	// Prompt is the template to paste (WP-308), core.AutopilotJob's; empty
	// from a server that sends none: defaultAutopilotPrompt.
	Prompt string `json:"prompt,omitempty"`
	// ReviewOf, with Review the prompt (WP-285): compact the session and
	// type the adversarial review of the task it finished, instead of
	// moving it on. Key is empty then.
	ReviewOf string `json:"review_of,omitempty"`
	Review   string `json:"review,omitempty"`
	// Compacted: compacted already, while the review waited for its audit
	// (WP-761).
	Compacted bool `json:"compacted,omitempty"`
	// Held is the work other sessions on this machine still have, which keeps
	// their worktrees from the sweep (WP-ADR57).
	Held []autopilotHeld `json:"held,omitempty"`
	// HeldUnknown: Held couldn't be read in full; the sweep removes nothing.
	HeldUnknown bool `json:"held_unknown,omitempty"`
}

// autopilotHeld is core.AutopilotHeld.
type autopilotHeld struct {
	Key string `json:"key"`
	Cwd string `json:"cwd,omitempty"`
}

// autopilotResult is core.AutopilotJobResult.
type autopilotResult struct {
	Outcome string `json:"outcome"` // started, reviewing, held, gone, unwanted, cleared
	Reason  string `json:"reason,omitempty"`
	Detail  string `json:"detail,omitempty"`
	NewCLI  string `json:"new_cli_id,omitempty"`
	Warning string `json:"warning,omitempty"`
	Via     string `json:"via,omitempty"` // restart
	// Kept: worktrees that stayed because they still hold work, not reported
	// before (WP-ADR57).
	Kept []string `json:"kept,omitempty"`
	// Unshipped: other work the repository holds that nothing has shipped,
	// not reported before (WP-ADR74).
	Unshipped []string `json:"unshipped,omitempty"`
}

// autopilotPromptMark starts every prompt autopilot pastes (core's
// AutopilotPromptMark): the server never counts it as the person's.
const autopilotPromptMark = "Waypoint autopilot"

// Seams for tests: tmux, wp next, the clock's waits.
var (
	runTmux = func(ctx context.Context, socket string, stdin []byte, args ...string) (string, error) {
		bin, err := exec.LookPath("tmux")
		if err != nil {
			return "", errNoTmux
		}
		if socket != "" {
			args = append([]string{"-S", socket}, args...)
		}
		cmd := exec.CommandContext(ctx, bin, args...) // #nosec G204 -- tmux with fixed subcommands; the pane and text come from Claude Code's own session file
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return string(out), fmt.Errorf("tmux %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
		}
		return string(out), nil
	}
	// runWpNext takes task key for a new session and returns the answer:
	// who handed it over (how: --assigned for autopilot, --started for a
	// task session), the task, how to finish it and where to start.
	runWpNext = func(ctx context.Context, dir string, env []string, key, how string) (string, error) {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		cmd := exec.CommandContext(ctx, exe, "next", key, "-p", claimProject(key), how) // #nosec G204 -- this wp, with a validated key and one of two flags
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	autopilotPoll = 500 * time.Millisecond
	// autopilotSettle is how long a new session must stay ready before its
	// prompt is pasted: one that has just come up may still be loading.
	autopilotSettle = 2 * time.Second
	// autopilotReadyWait is how long a new session has to become ready for
	// its prompt.
	autopilotReadyWait = 90 * time.Second
	// autopilotCompactWait is how long /compact has before a review's
	// prompt: compacting a full context takes minutes.
	autopilotCompactWait = 6 * time.Minute
	// autopilotCompactStart is how long /compact has to show as busy.
	autopilotCompactStart = 10 * time.Second
	// autopilotIdleWait is how long a review job waits for a busy session to
	// go idle before it is held. With autopilotCompactWait it stays under
	// the server's reviewJobTimeout (10 minutes).
	autopilotIdleWait = 3 * time.Minute
	// reviewTookWait is how long a session has to show it took the review
	// prompt (its status turning busy, or its screen, WP-1325).
	reviewTookWait = 15 * time.Second
)

var errNoTmux = errors.New("tmux isn't installed on this machine")

func (p *runnerProc) execAutopilot(ctx context.Context, run *jobRun, in autopilotJob) outcome {
	say := func(format string, a ...any) { fmt.Fprintf(run.out, format+"\n", a...) }
	held := func(reason, format string, a ...any) outcome {
		d := fmt.Sprintf(format, a...)
		say("held: %s", d)
		return outcome{State: "done", Result: autopilotResult{Outcome: "held", Reason: reason, Detail: d}}
	}
	dir, err := resolveDir(in.Dir, p.allowed)
	if err != nil {
		return failed("%v", err)
	}
	if in.ReviewOf != "" {
		if !keyRE.MatchString(in.ReviewOf) || strings.TrimSpace(in.Review) == "" {
			return failed("a review job needs the task's key and its prompt")
		}
	} else if !keyRE.MatchString(in.Key) {
		return failed("%q isn't an issue key", in.Key)
	}
	ccDir := claudeConfigDir()
	s, err := findCCSession(ccDir, in.CLIID)
	if err != nil {
		say("%v", err)
		return outcome{State: "done", Result: autopilotResult{Outcome: "gone", Detail: err.Error()}}
	}
	// The tmux pane of the session, where it runs in one.
	pane, socket := "", ""
	if s.Tmux != "" {
		if pn := s.Tmux[strings.LastIndex(s.Tmux, ".")+1:]; strings.HasPrefix(pn, "%") {
			pane, socket = pn, tmuxSocketOf(s.PID)
		}
	}
	// wasBusy: the registry says busy while the screen shows the main
	// thread at its prompt (background agents or tasks keep the status
	// "busy": WP-1325), so what the registry says after typing proves
	// nothing about whether the session took the prompt.
	wasBusy := false
	// A task's start clears or restarts the session, which would end a shell
	// it still runs, so only a review is typed beside one.
	if s.Status != "idle" && (in.ReviewOf == "" || !atPrompt(s.Status)) {
		// A review waits out the turn (a background task reporting back, a
		// compaction finishing) and goes on the moment the session is idle,
		// rather than a retry minutes later (WP-816). A person's turn isn't
		// waited out here: their prompt took the job off the step, which
		// brings the review after their Stop.
		if in.ReviewOf != "" && !p.stepWanted(ctx, in.Step, run.id) {
			say("the step no longer wants this job: nothing typed")
			return outcome{State: "done", Result: autopilotResult{Outcome: "unwanted"}}
		}
		if in.ReviewOf == "" && s.Status == "shell" {
			return held("busy", "the session still runs a background shell, which starting the task would end: autopilot looks again later")
		}
		if in.ReviewOf == "" {
			return held("busy", "the session is %s, not idle", cmpStr(s.Status, "in an unknown state"))
		}
		// Typing a review beside running work is as safe as at a prompt:
		// the registry's "busy" isn't the last word when the screen shows
		// the main thread at its prompt.
		if !waitUntil(ctx, autopilotIdleWait, func() bool {
			if cur, err := readCCSession(ccDir, s.PID); err == nil && atPrompt(cur.Status) {
				return true
			}
			if pane == "" {
				return false
			}
			_, ok := mainAtPrompt(ctx, socket, pane, autopilotSettle)
			wasBusy = ok
			return ok
		}) {
			return held("busy", "the session is %s, not idle", cmpStr(s.Status, "in an unknown state"))
		}
		if wasBusy {
			say("session %s is %s but its main thread is at its prompt: going on with the review of %s", in.CLIID, cmpStr(s.Status, "busy"), in.ReviewOf)
		} else {
			say("session %s is idle again: going on with the review of %s", in.CLIID, in.ReviewOf)
		}
	}
	if s.Tmux == "" {
		return held("no_tmux", "it isn't running inside tmux, so it can't be restarted from outside: start Claude Code in tmux to use autopilot (tmux new -s NAME claude)")
	}
	if pane == "" {
		return held("no_tmux", "Claude Code names no tmux pane for it (%q)", s.Tmux)
	}
	screen, err := runTmux(ctx, socket, nil, "capture-pane", "-p", "-e", "-t", pane)
	if errors.Is(err, errNoTmux) {
		return held("no_tmux", "%v", err)
	}
	if err != nil {
		return held("screen", "can't read its tmux pane: %v", err)
	}
	if draft, found := promptInput(screen); !found {
		return held("screen", "its input box isn't on the screen (a dialog or a scrolled pane?)")
	} else if draft != "" {
		return held("draft", "someone has typed into its input box (%q): it waits until that is sent or cleared", truncRunes(draft, 40))
	}
	var rl *relaunch
	var why string
	if in.ReviewOf == "" {
		rl, why = planRelaunch(ctx, socket, pane, s.PID)
	}
	if rl != nil {
		// The person's session settings now, in place of what the old
		// session was started with (WP-1265).
		set := run.launch.forDir(rl.dir)
		rl.args, rl.env = set.applyTo(rl.args), set.env()
	}
	// The last word before typing: a person who prompted the session, pressed
	// Stop or turned autopilot off since the job was queued keeps it.
	if !p.stepWanted(ctx, in.Step, run.id) {
		say("the step no longer wants this job: nothing typed")
		return outcome{State: "done", Result: autopilotResult{Outcome: "unwanted"}}
	}
	if in.ReviewOf != "" {
		return p.reviewSession(ctx, run, in, socket, pane, s.PID, wasBusy)
	}
	if rl != nil {
		return p.restartSession(ctx, run, in, dir, s, socket, pane, rl)
	}
	// No /clear instead: the session would stay in this folder, and every
	// task is to start in its own worktree (WP-ADR57).
	return held("no_tmux", "it can't be restarted from outside (%s): start it as plain claude in tmux (tmux new -s NAME claude) to use autopilot", why)
}

// reviewPasteSettle is how long to let the session take in a pasted review
// prompt before Enter: a prompt carries an audit's every finding (WP-769),
// so it can be far longer than the 2 KB this was 400 ms for.
func reviewPasteSettle(n int) time.Duration {
	return 400*time.Millisecond + time.Duration(n/1024)*10*time.Millisecond
}

// reviewSession (WP-285): the session finished in.ReviewOf; compact it
// (unless its transcript shows that was done while Grok audited it, and
// nothing since: WP-816), so the review starts from a short context, then
// type the review prompt. The
// session keeps its id: the review is part of its work on the task.
// Outcome reviewing once the prompt went in; held when it never got that far
// (the step then moves on without the review).
func (p *runnerProc) reviewSession(ctx context.Context, run *jobRun, in autopilotJob, socket, pane string, pid int, wasBusy bool) outcome {
	say := func(format string, a ...any) { fmt.Fprintf(run.out, format+"\n", a...) }
	held := func(reason, format string, a ...any) outcome {
		d := fmt.Sprintf(format, a...)
		say("held: %s", d)
		return outcome{State: "done", Result: autopilotResult{Outcome: "held", Reason: reason, Detail: d}}
	}
	ccDir := claudeConfigDir()
	// The session's transcript has the last word on whether it is still
	// compacted (WP-816), whatever the server saw.
	compact, why := reviewCompaction(transcriptOfSession(in.CLIID), in.Compacted)
	if !compact {
		say("session %s isn't compacted again before the review of %s: %s", in.CLIID, in.ReviewOf, why)
	} else {
		say("compacting session %s in tmux pane %s before the review of %s: %s", in.CLIID, pane, in.ReviewOf, why)
		if _, err := runTmux(ctx, socket, nil, "send-keys", "-t", pane, "-l", "/compact"); err != nil {
			return failed("%v", err)
		}
		if _, err := runTmux(ctx, socket, nil, "send-keys", "-t", pane, "Enter"); err != nil {
			return failed("%v", err)
		}
		// Compaction shows as busy; a session too small to compact may never
		// look busy, which is fine: ready is what counts.
		waitTook(ctx, ccDir, pid, socket, pane, wasBusy, autopilotCompactStart)
		if !waitReady(ctx, ccDir, socket, pane, pid, autopilotCompactWait, wasBusy) {
			return held("screen", "it wasn't ready for the review within %s of /compact (still compacting, a dialog, or someone typing there?)", autopilotCompactWait)
		}
		// A person may have taken the session while it compacted.
		if !p.stepWanted(ctx, in.Step, run.id) {
			say("the step no longer wants this job: compacted, review not typed")
			return outcome{State: "done", Result: autopilotResult{Outcome: "unwanted"}}
		}
	}
	buf := fmt.Sprintf("wp-autopilot-%d", run.id)
	if _, err := runTmux(ctx, socket, []byte(in.Review), "load-buffer", "-b", buf, "-"); err != nil {
		return held("screen", "the review prompt couldn't be loaded: %v", err)
	}
	if _, err := runTmux(ctx, socket, nil, "paste-buffer", "-p", "-d", "-b", buf, "-t", pane); err != nil {
		return held("screen", "the review prompt couldn't be pasted: %v", err)
	}
	sleepCtx(ctx, reviewPasteSettle(len(in.Review)))
	if _, err := runTmux(ctx, socket, nil, "send-keys", "-t", pane, "Enter"); err != nil {
		// Pasted and not sent: the box now holds a draft, which a person
		// sees; the round reads as running and ends at the next Stop.
		return outcome{State: "done", Result: autopilotResult{Outcome: "reviewing", Warning: "the review prompt was pasted and not sent (press Enter there): " + err.Error()}}
	}
	res := autopilotResult{Outcome: "reviewing"}
	if !waitTook(ctx, ccDir, pid, socket, pane, wasBusy, reviewTookWait) {
		res.Warning = "the session didn't start the review within 15 s; check it"
	}
	say("asked session %s for the adversarial review of %s", in.CLIID, in.ReviewOf)
	return outcome{State: "done", Result: res}
}

// promptSession gives the fresh session newID (process pid, in pane) the
// task's wp next answer as its prompt, once it is ready for one. base carries
// what the restart already has to report; dir is where wp next runs.
func (p *runnerProc) promptSession(ctx context.Context, run *jobRun, in autopilotJob, dir, socket, pane string, pid int, newID string, base autopilotResult) outcome {
	say := func(format string, a ...any) { fmt.Fprintf(run.out, format+"\n", a...) }
	// A failure leaves a fresh session without its prompt: say so plainly
	// ("cleared"), so the person gives it the task.
	cleared := func(format string, a ...any) outcome {
		d := fmt.Sprintf(format, a...)
		say("fresh, but %s", d)
		r := base
		r.Outcome, r.NewCLI, r.Detail = "cleared", newID, d
		return outcome{State: "done", Result: r}
	}
	ccDir := claudeConfigDir()
	if !waitReady(ctx, ccDir, socket, pane, pid, autopilotReadyWait, false) {
		return cleared("it wasn't ready for its prompt within %s (a dialog on its screen, or someone typing there?)", autopilotReadyWait)
	}
	p.waitRegistered(ctx, newID, 20*time.Second)
	if !p.stepWanted(ctx, in.Step, run.id) { // the wait above can be minutes: a person may have pressed Stop
		say("the step no longer wants this job: the new session gets no task")
		r := base
		r.Outcome = "unwanted"
		return outcome{State: "done", Result: r}
	}
	// wp next runs as the job's own token (WP-418): the runner's is a
	// person's and never reaches a job. From a server that sends none, wp
	// next reads its token from this machine's config as before.
	env := autopilotEnv(os.Environ(), newID, in.Agent, p.c.server(), run.token)
	nctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	answer, nextErr := runWpNext(nctx, dir, env, in.Key, "--assigned")
	cancel()
	res := base
	res.Outcome, res.NewCLI = "started", newID
	if nextErr != nil {
		w := "wp next " + in.Key + " failed: " + firstLine(strings.TrimSpace(answer)+" "+nextErr.Error())
		res.Warning = strings.TrimSpace(res.Warning + " " + w)
		say("%s", w)
	}
	prompt := autopilotPrompt(in, answer, nextErr)
	// The session's hook knows this prompt for autopilot's own, whatever its
	// wording (WP-308), so it never counts as the person's.
	p.c.notePastedPrompt(newID, in.Key, prompt)
	buf := fmt.Sprintf("wp-autopilot-%d", run.id) // one per job: two jobs never paste each other's
	if _, err := runTmux(ctx, socket, []byte(prompt), "load-buffer", "-b", buf, "-"); err != nil {
		return cleared("its prompt couldn't be loaded: %v", err)
	}
	if _, err := runTmux(ctx, socket, nil, "paste-buffer", "-p", "-d", "-b", buf, "-t", pane); err != nil {
		return cleared("its prompt couldn't be pasted: %v", err)
	}
	sleepCtx(ctx, 400*time.Millisecond) // let it take the paste in before Enter
	if _, err := runTmux(ctx, socket, nil, "send-keys", "-t", pane, "Enter"); err != nil {
		return cleared("its prompt was pasted and not sent (press Enter there): %v", err)
	}
	if !waitBusy(ctx, ccDir, pid, 15*time.Second) {
		res.Warning = strings.TrimSpace(res.Warning + " the session didn't start working within 15 s; check it")
	}
	say("started %s in session %s", in.Key, newID)
	return outcome{State: "done", Result: res}
}

// waitReady waits until the session process pid is ready for a prompt: idle,
// with an empty input box on its screen, and still so after autopilotSettle,
// since one that has just started may still be loading or show a dialog.
// With screenOK, a registry that says busy doesn't decide: a screen with no
// turn on it does (WP-1325).
func waitReady(ctx context.Context, dir, socket, pane string, pid int, limit time.Duration, screenOK bool) bool {
	ready := func() bool {
		s, err := readCCSession(dir, pid)
		if err != nil {
			return false
		}
		screen, err := runTmux(ctx, socket, nil, "capture-pane", "-p", "-e", "-t", pane)
		if err != nil || (!atPrompt(s.Status) && (!screenOK || turnShown(screen))) {
			return false
		}
		draft, found := promptInput(screen)
		return found && draft == ""
	}
	end := time.Now().Add(limit)
	for waitUntil(ctx, time.Until(end), ready) {
		if !sleepCtx(ctx, autopilotSettle) {
			return false
		}
		if ready() {
			return true
		}
	}
	return false
}

// defaultAutopilotPrompt is core.DefaultAutopilotPrompt, for a server that
// sends no template.
const defaultAutopilotPrompt = "{next}"

// autopilotPrompt is what autopilot pastes: the job's template (WP-308) with
// the task's wp next answer as {next}, or, when running wp next failed, what
// happened.
func autopilotPrompt(in autopilotJob, answer string, nextErr error) string {
	return kickoffPrompt(kickoff{key: in.Key, title: in.Title, done: in.Done, template: in.Prompt,
		opener: "Autopilot assigned " + in.Key + " to you"}, answer, nextErr)
}

// kickoff is what a session Waypoint starts on a task gets as its first
// prompt: autopilot's, and a task session's (WP-1037), made by the same
// code so they say the same things. opener names who handed the task over,
// for when wp next couldn't.
type kickoff struct {
	key, title, done, template, opener string
}

// kickoffPrompt fills the template ({key}, {title}, {done}, {next}) with
// the task's wp next answer, which carries the task, how to finish it, and
// how to ask about it or give it back. When wp next failed the session hears
// that instead, never an order to take the task anyway.
func kickoffPrompt(k kickoff, answer string, nextErr error) string {
	next := strings.TrimSpace(answer)
	if nextErr != nil {
		why := firstLine(next)
		if why == "" {
			why = nextErr.Error()
		}
		next = fmt.Sprintf("%s, but running `wp next %s` for you failed (%s), so this session doesn't hold it yet. `wp next %s` shows the task, or why it can't be taken; if it can't, or shouldn't be done as written, say so rather than working around it.",
			k.opener, k.key, why, k.key)
	}
	tmpl := strings.TrimSpace(k.template)
	if tmpl == "" {
		tmpl = defaultAutopilotPrompt
	}
	r := strings.NewReplacer("{key}", k.key, "{title}", k.title, "{done}", k.done, "{next}", next)
	return strings.TrimSpace(r.Replace(tmpl))
}

// promptInput finds Claude Code's input box on a captured screen (with
// escapes, capture-pane -e) and returns what is typed into it, without the
// dimmed placeholder. The box is the last line that starts with ❯ right
// under the box's top border, so a ❯ in a status line or the transcript isn't
// taken for it. The top border is a rule (──) that may carry a label at its
// right end (a named session shows "── WP-1320 - Title ─"); the bottom border
// is always a plain rule. found is false when no input box is on the screen.
func promptInput(screen string) (text string, found bool) {
	lines := strings.Split(strings.ReplaceAll(screen, "\r", ""), "\n")
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = strings.TrimSpace(stripEscapes(l))
	}
	at := -1
	for i := len(lines) - 1; i > 0; i-- {
		if strings.HasPrefix(plain[i], "❯") && isTopBorder(plain[i-1]) {
			at = i
			break
		}
	}
	if at < 0 {
		return "", false
	}
	_, rest, _ := strings.Cut(lines[at], "❯")
	parts := []string{undimmed(rest)}
	closed := false
	for i := at + 1; i < len(lines); i++ {
		if isRule(plain[i]) || strings.HasPrefix(plain[i], "╰") {
			closed = true // the box's bottom edge
			break
		}
		parts = append(parts, strings.TrimSpace(undimmed(lines[i])))
	}
	if !closed {
		return "", false
	}
	text = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\u00a0' {
			return ' '
		}
		return r
	}, strings.Join(parts, " ")))
	return text, true
}

// isTopBorder: a rule, or a rule with a label in it: it starts with ── and
// ends with ─ (Claude Code writes the session's name into the right end).
func isTopBorder(s string) bool {
	return isRule(s) || (strings.HasPrefix(s, "──") && strings.HasSuffix(s, "─"))
}

// isRule: a line of box-drawing dashes, with nothing in it (the box's
// bottom edge).
func isRule(s string) bool {
	return strings.HasPrefix(s, "──") && strings.Trim(s, "─ ") == ""
}

// stripEscapes is s without its escape sequences.
func stripEscapes(s string) string {
	return sgrWalk(s, false)
}

// undimmed is s without its escape sequences and without the text drawn
// dim (SGR 2), which is how Claude Code draws a placeholder.
func undimmed(s string) string {
	return sgrWalk(s, true)
}

// sgrWalk copies s without escape sequences; with dropDim, also without the
// text drawn while SGR 2 (dim) is on. Color parameters (38/48;5;n and
// 38/48;2;r;g;b) are read as such, so their numbers are never taken for 2.
func sgrWalk(s string, dropDim bool) string {
	var b strings.Builder
	dim := false
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			if !dropDim || !dim {
				b.WriteByte(s[i])
			}
			continue
		}
		if i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			if j < len(s) && s[j] == 'm' {
				ps := strings.Split(s[i+2:j], ";")
				for k := 0; k < len(ps); k++ {
					switch ps[k] {
					case "38", "48", "58":
						if k+1 < len(ps) && ps[k+1] == "5" {
							k += 2
						} else if k+1 < len(ps) && ps[k+1] == "2" {
							k += 4
						}
					case "2":
						dim = true
					case "", "0", "22":
						dim = false
					}
				}
			}
			i = j
			continue
		}
		i++ // a two-byte escape
	}
	return b.String()
}

// tmuxSocketOf is the tmux server the Claude Code process runs under, from
// its TMUX variable ("socket,pid,session"); "" for tmux's default.
func tmuxSocketOf(pid int) string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ")) // #nosec G304 -- the session's own process, found in Claude Code's registry
	if err != nil {
		return ""
	}
	for _, kv := range bytes.Split(b, []byte{0}) {
		if v, ok := bytes.CutPrefix(kv, []byte("TMUX=")); ok {
			sock, _, _ := strings.Cut(string(v), ",")
			if filepath.IsAbs(sock) {
				return sock
			}
		}
	}
	return ""
}

// readCCSession reads Claude Code's file about the session process pid.
func readCCSession(dir string, pid int) (*ccSession, error) {
	b, err := os.ReadFile(filepath.Join(dir, "sessions", strconv.Itoa(pid)+".json")) // #nosec G304 -- Claude Code's own session registry
	if err != nil {
		return nil, err
	}
	var s ccSession
	return &s, json.Unmarshal(b, &s)
}

// atPrompt: Claude Code's status says the session waits at its prompt.
// "shell" is idle with a background shell still running (a waiter on CI, a
// long test run): a prompt or /compact typed there starts a turn as from
// idle, and the shell goes on (WP-1306). Before, such a session held every
// review, ping and compaction for as long as its shell ran, hours for a
// waiter, and the review round failed after heldScreenWait.
func atPrompt(status string) bool {
	return status == "idle" || status == "shell"
}

// waitBusy waits for the session to start working on the prompt.
func waitBusy(ctx context.Context, dir string, pid int, limit time.Duration) bool {
	end := time.Now().Add(limit)
	for time.Now().Before(end) {
		if s, err := readCCSession(dir, pid); err == nil && s.Status != "" && !atPrompt(s.Status) {
			return true
		}
		if !sleepCtx(ctx, autopilotPoll) {
			break
		}
	}
	return false
}

// waitRegistered waits (briefly) for the new session's SessionStart hook to
// have told Waypoint about it, so wp next records the task on it.
func (p *runnerProc) waitRegistered(ctx context.Context, cliID string, limit time.Duration) {
	if p.c.server() == "" {
		return
	}
	end := time.Now().Add(limit)
	q := "/api/v1/sessions/current?cli_id=" + url.QueryEscape(cliID) + "&machine=" + url.QueryEscape(machineName())
	for time.Now().Before(end) {
		var x struct {
			ID int64 `json:"id"`
		}
		if err := p.c.callInto(ctx, "GET", q, nil, &x); err == nil && x.ID != 0 {
			return
		}
		if !sleepCtx(ctx, autopilotPoll) {
			return
		}
	}
}

// stepWanted asks Waypoint whether the step still waits for this job. An
// answer it can't get counts as yes: the draft and idle checks still hold.
func (p *runnerProc) stepWanted(ctx context.Context, step, job int64) bool {
	if step == 0 || p.c.server() == "" {
		return true
	}
	var r struct {
		Wanted bool `json:"wanted"`
	}
	if err := p.c.callInto(ctx, "GET", fmt.Sprintf("/api/v1/autopilot/steps/%d/wanted?job=%d", step, job), nil, &r); err != nil {
		return true
	}
	return r.Wanted
}

// autopilotEnv is the environment wp next runs with: the new session's,
// as the agent, without what ties this process to another session.
func autopilotEnv(base []string, session, agent, server, token string) []string {
	drop := map[string]bool{"WAYPOINT_SESSION": true, "WAYPOINT_FOLLOWUP": true, "WAYPOINT_TRANSCRIPT": true, "WP_TASK": true, "CLAUDECODE": true,
		"WAYPOINT_AGENT": true, "CLAUDE_ENV_FILE": true, "CLAUDE_PROJECT_DIR": true, "WAYPOINT_SERVER": true, "WAYPOINT_TOKEN": true}
	var out []string
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if k = strings.ToUpper(k); drop[k] || !jobEnvKeeps(k, false) || strings.HasPrefix(k, "CLAUDE_CODE_") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "WAYPOINT_UNATTENDED=1", "WAYPOINT_SESSION="+session, "WAYPOINT_MACHINE="+machineName())
	if agent != "" {
		out = append(out, "WAYPOINT_AGENT="+agent)
	}
	if server != "" && token != "" {
		out = append(out, "WAYPOINT_SERVER="+server, "WAYPOINT_TOKEN="+token)
	}
	return out
}
