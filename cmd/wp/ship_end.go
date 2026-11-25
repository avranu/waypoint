package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"git.jmann.me/claude/waypoint/internal/lazyre"
)

// shipStopReason marks the Stop event a hook sent for a turn wp ship ended:
// Reason is a field older servers already take, so the event reaches them.
const shipStopReason = "ship"

const (
	// shipEndFresh: how long after wp ship wrote it the hook takes its
	// marker; a marker the hook never saw (wp ship run by hand, a hook
	// missing) mustn't end a later turn.
	shipEndFresh = 2 * time.Minute
	// shipStopFresh: how long the Stop hook skips the stop a hook already
	// reported.
	shipStopFresh = 2 * time.Minute
	// shipBgFresh: how long a wp ship running in the background may take
	// to report back and still end the turn.
	// It outlasts wp ship's longest wait (shipWaitMax, counted from the start
	// of the wp process, so the check and push before the call went to the
	// background are in it), so the marker the call writes at its end finds
	// its background entry still there (WP-1669).
	shipBgFresh = 3 * time.Hour
	// keptSummaryFor: how long a summary waits for the task's next wp ship.
	keptSummaryFor = 48 * time.Hour
)

// shipEnd is one session's state of it.
type shipEnd struct {
	// Ready: wp ship went as expected at this time, and Text is what the
	// person is shown when the turn ends.
	Ready time.Time `json:"ready,omitzero"`
	Text  string    `json:"text,omitempty"`
	Key   string    `json:"key,omitempty"`
	// Bg are the background task ids of wp ship calls Claude Code moved to
	// the background, with when: their report ends the turn instead.
	Bg map[string]time.Time `json:"bg,omitempty"`
	// Stopped: a hook sent this turn's Stop, so the Stop hook doesn't.
	Stopped time.Time `json:"stopped,omitzero"`
}

// keptSummary is a --summary waiting for the task's next wp ship in the
// same turn: Session's next prompt drops it.
type keptSummary struct {
	Text    string    `json:"text"`
	At      time.Time `json:"at"`
	Session string    `json:"session,omitempty"`
}

// pruneShipEnds drops what is too old to matter, so the state file doesn't
// grow with every session.
func pruneShipEnds(s *State, now time.Time) {
	for id, e := range s.ShipEnds {
		for t, at := range e.Bg {
			if now.Sub(at) > shipBgFresh {
				delete(e.Bg, t)
			}
		}
		if age := now.Sub(e.Ready); age > shipBgFresh || (age > shipEndFresh && len(e.Bg) == 0) {
			e.Ready, e.Text, e.Key = time.Time{}, "", ""
		}
		if now.Sub(e.Stopped) > shipStopFresh {
			e.Stopped = time.Time{}
		}
		if e.Ready.IsZero() && e.Stopped.IsZero() && len(e.Bg) == 0 {
			delete(s.ShipEnds, id)
		}
	}
	for k, v := range s.Summaries {
		if now.Sub(v.At) > keptSummaryFor {
			delete(s.Summaries, k)
		}
	}
}

// shipSession changes this session's shipEnd ("" session: nothing).
func (c *cli) shipSession(session string, change func(*shipEnd)) {
	if session == "" {
		return
	}
	_ = c.updateState(func(s *State) {
		now := time.Now()
		pruneShipEnds(s, now)
		if s.ShipEnds == nil {
			s.ShipEnds = map[string]*shipEnd{}
		}
		e := s.ShipEnds[session]
		if e == nil {
			e = &shipEnd{}
			s.ShipEnds[session] = e
		}
		change(e)
		if e.Ready.IsZero() && e.Stopped.IsZero() && len(e.Bg) == 0 {
			delete(s.ShipEnds, session)
		}
	})
}

// keepSummary keeps text for key's next wp ship; "" drops what was kept.
func (c *cli) keepSummary(key, text string) {
	key = strings.ToUpper(strings.TrimSpace(key))
	if key == "" {
		return
	}
	_ = c.updateState(func(s *State) {
		pruneShipEnds(s, time.Now())
		if strings.TrimSpace(text) == "" {
			delete(s.Summaries, key)
			return
		}
		if s.Summaries == nil {
			s.Summaries = map[string]*keptSummary{}
		}
		s.Summaries[key] = &keptSummary{Text: text, At: time.Now(), Session: c.thisSession()}
	})
}

// dropSessionSummaries drops the summaries session kept: a new prompt
// started another turn, and a reply written before it mustn't end that one.
func (c *cli) dropSessionSummaries(session string) {
	if session == "" {
		return
	}
	for _, k := range c.state.Summaries {
		if k.Session == session {
			_ = c.updateState(func(s *State) {
				for key, k := range s.Summaries {
					if k.Session == session {
						delete(s.Summaries, key)
					}
				}
			})
			return
		}
	}
}

// keptSummaryOf is the summary kept for key, if it is still fresh.
func (c *cli) keptSummaryOf(key string) string {
	k := c.state.Summaries[strings.ToUpper(strings.TrimSpace(key))]
	if k == nil || time.Since(k.At) > keptSummaryFor {
		return ""
	}
	return k.Text
}

// shipSummary is the summary for this wp ship: --summary when given ("" from
// --summary "" drops a kept one), else the one kept for the task. kept says
// it came from an earlier wp ship.
func (c *cli) shipSummary(key, given string, isGiven bool) (text string, kept bool) {
	if isGiven {
		if strings.TrimSpace(given) == "" {
			c.keepSummary(key, "")
		}
		return strings.TrimSpace(given), false
	}
	if key == "" {
		return "", false
	}
	text = c.keptSummaryOf(key)
	return text, text != ""
}

// fillSummary puts the pull request in a summary: {task}, {pr} and {url} as
// in the reply template, and the pull request's line at the end when the
// summary doesn't give its URL.
func fillSummary(text, key string, d *prDetail) string {
	if d == nil {
		return strings.TrimSpace(text)
	}
	what := cmpStr(key, d.Title)
	s := strings.NewReplacer("{task}", what, "{pr}", strconv.Itoa(d.Number), "{url}", d.URL).Replace(strings.TrimSpace(text))
	if d.URL != "" && !strings.Contains(s, d.URL) {
		s += fmt.Sprintf("\n\nPR #%d: %s", d.Number, d.URL)
	}
	return s
}

// heldSummary is what the person sees when a ship held for its review ends
// the turn: the summary, with the pull request still to come.
func heldSummary(text string) string {
	s := strings.NewReplacer("{pr}", "(after the review)", "{url}", "(after the review)", "{task}", "this task").Replace(strings.TrimSpace(text))
	return s + "\n\n(The pull request opens after the adversarial review, by Waypoint when the review leaves nothing for the session to do; findings that need the session come back to it.)"
}

// endTurnAfterShip marks this session's turn as one the hooks may end, with
// text shown to the person, and says whether it did: only a Claude Code
// session whose hooks reach the server can have its turn ended.
func (c *cli) endTurnAfterShip(key, text string) bool {
	if c.agentHeader() == "" {
		return false
	}
	session := c.thisSession()
	if session == "" || c.server() == "" || c.token() == "" {
		if !c.asJSON {
			_, _ = fmt.Fprintln(c.err, "Notice: --summary can't end the turn here (no Claude Code session with Waypoint's hooks), so it is kept for the task's next wp ship; reply as usual.")
		}
		return false
	}
	c.shipSession(session, func(e *shipEnd) {
		e.Ready, e.Text, e.Key = time.Now(), text, key
	})
	return true
}

// clearShipEnd forgets, as an agent's wp ship starts, a marker it didn't
// write, so only its own can end the turn after it.
func (c *cli) clearShipEnd() {
	if c.agentHeader() == "" {
		return
	}
	if session := c.thisSession(); session != "" {
		if e := c.state.ShipEnds[session]; e != nil && !e.Ready.IsZero() {
			c.shipSession(session, func(e *shipEnd) { e.Ready, e.Text, e.Key = time.Time{}, "", "" })
		}
	}
}

// dropUnusedShipEnd forgets a marker the call after wp ship didn't take:
// the turn went on past the ship, so a later call mustn't end it with that
// summary. One a background wp ship may still report stays.
func (c *cli) dropUnusedShipEnd(session string) {
	if e := c.state.ShipEnds[session]; e != nil && !e.Ready.IsZero() && len(e.Bg) == 0 {
		c.shipSession(session, func(e *shipEnd) { e.Ready, e.Text, e.Key = time.Time{}, "", "" })
	}
}

// isShipCall says whether a Bash command ends in wp ship (or its aliases):
// its result is wp ship's. "make check && wp ship …" is one; "wp ship …;
// echo" or "wp ship … | tail" isn't, since something ran after it.
func isShipCall(command string) bool {
	segs := shellWords(command)
	if len(segs) == 0 {
		return false
	}
	w := segs[len(segs)-1]
	for len(w) > 0 && strings.Contains(w[0], "=") && !strings.HasPrefix(w[0], "-") {
		w = w[1:]
	}
	if len(w) < 2 || filepath.Base(w[0]) != "wp" {
		return false
	}
	switch w[1] {
	case "ship", "done", "finish":
		return true
	}
	return false
}

// bashCommand is a Bash call's command.
func bashCommand(input json.RawMessage) string {
	var b struct {
		Command string `json:"command"`
	}
	if len(input) == 0 || json.Unmarshal(input, &b) != nil {
		return ""
	}
	return b.Command
}

// bashBackground is the background task id of a Bash call Claude Code
// moved to the background ("": it finished in the foreground).
func bashBackground(resp json.RawMessage) string {
	var r struct {
		ID string `json:"backgroundTaskId"`
	}
	if len(resp) == 0 || json.Unmarshal(resp, &r) != nil {
		return ""
	}
	return r.ID
}

var (
	taskIDRE     = lazyre.New(`<task-id>([^<]+)</task-id>`)
	taskStatusRE = lazyre.New(`<status>([^<]+)</status>`)
	taskExitRE   = lazyre.New(`\(exit code (\d+)\)`)
)

// finishedTask is the background task a task notification says finished
// well (exit 0); "" for any other prompt.
func finishedTask(prompt string) string {
	p := strings.TrimSpace(prompt)
	if !strings.HasPrefix(p, "<task-notification>") {
		return ""
	}
	id, st, ex := taskIDRE.FindStringSubmatch(p), taskStatusRE.FindStringSubmatch(p), taskExitRE.FindStringSubmatch(p)
	if id == nil || st == nil || strings.TrimSpace(st[1]) != "completed" || (ex != nil && ex[1] != "0") {
		return ""
	}
	return strings.TrimSpace(id[1])
}

// takeShipEnd takes the session's marker for a turn wp ship may end: one
// written moments ago, or (bg set) one written since that background wp
// ship started. Nil: the turn goes on.
func (c *cli) takeShipEnd(session, bg string) *shipEnd {
	var got *shipEnd
	c.shipSession(session, func(e *shipEnd) {
		if bg != "" {
			started, ok := e.Bg[bg]
			if !ok {
				return
			}
			delete(e.Bg, bg)
			if e.Ready.IsZero() || e.Ready.Before(started) {
				return
			}
		} else if time.Since(e.Ready) > shipEndFresh {
			return
		}
		if e.Ready.IsZero() {
			return
		}
		got = &shipEnd{Ready: e.Ready, Text: e.Text, Key: e.Key}
		e.Ready, e.Text, e.Key = time.Time{}, "", ""
	})
	return got
}

// noteShipBackground remembers a wp ship call Claude Code moved to the
// background, so its report can end the turn.
func (c *cli) noteShipBackground(session, id string) {
	c.shipSession(session, func(e *shipEnd) {
		if e.Bg == nil {
			e.Bg = map[string]time.Time{}
		}
		e.Bg[id] = time.Now()
		e.Ready, e.Text, e.Key = time.Time{}, "", ""
	})
}

// stopAlreadySent says (and forgets) that a hook already sent this turn's
// Stop: the Stop hook, if Claude Code runs it after a hook ended the turn,
// sends nothing.
func (c *cli) stopAlreadySent(session string) bool {
	e := c.state.ShipEnds[session]
	if e == nil || e.Stopped.IsZero() {
		return false
	}
	stopped := e.Stopped
	c.shipSession(session, func(e *shipEnd) { e.Stopped = time.Time{} })
	return time.Since(stopped) <= shipStopFresh
}

// forgetStopSent: a new turn started, so the next Stop is its own.
func (c *cli) forgetStopSent(session string) {
	if e := c.state.ShipEnds[session]; e != nil && !e.Stopped.IsZero() {
		c.shipSession(session, func(e *shipEnd) { e.Stopped = time.Time{} })
	}
}

// shipStop ends the turn wp ship finished, if it may: it sends the session's
// Stop as the Stop hook would, with the summary as the last message, and
// prints what ends the turn. Anything that would have continued the turn at
// that Stop (a message, a follow-up's answer) continues it here instead, as
// context for the agent, and an unreachable server leaves the turn to go on
// as before. Neither: the turn goes on.
func (c *cli) shipStop(in hookEventInput, bg string) (stop []byte, context string) {
	if in.AgentID != "" || c.server() == "" || c.token() == "" {
		return nil, ""
	}
	end := c.takeShipEnd(in.SessionID, bg)
	if end == nil {
		return nil, ""
	}
	ev := sessionEvent{Event: "Stop", At: time.Now().UnixMilli(), SessionID: in.SessionID, Machine: machineName(), CLI: "claude", Cwd: in.Cwd,
		Task: taskBinding(), Reason: shipStopReason, LastMessage: end.Text}
	if filepath.IsAbs(in.Cwd) {
		ev.Repo, ev.Branch, ev.Worktree = gitInfo(in.Cwd)
	}
	ev.InputTokens, ev.OutputTokens, ev.ContextTokens, ev.CacheTTL = sessionTotals(c, in)
	ev.Pending = &hookPending{Tasks: []pendingTask{}, Crons: []pendingCron{}}
	ev.Checkouts = hookCheckouts(in.Cwd, c.state.sessionClaims(in.SessionID))
	c.hookTimeout = hookHTTPTimeout
	b, err := c.call("POST", "/api/v1/sessions/hook", map[string]any{"events": []sessionEvent{ev}})
	if err != nil {
		return nil, ""
	}
	var res hookReply
	_ = json.Unmarshal(b, &res)
	c.savePause(res.Pause)
	c.saveGuard(res.Guard)
	text := res.Text
	if text == "" && res.Await > 0 {
		text = c.awaitMessage(ev, res.Await)
	}
	if text != "" {
		return nil, text
	}
	c.shipSession(in.SessionID, func(e *shipEnd) { e.Stopped = time.Now() })
	c.keepSummary(end.Key, "")
	if over, spent := c.fanoutSessionOver(in.SessionID); over {
		endBackgroundSession(in.SessionID)
		fanoutEnded("budget", in.TranscriptPath, spent)
	} else if res.End {
		endBackgroundSession(in.SessionID)
	}
	return promptStop(joinParts(end.Text, res.Note)), ""
}

// checkArgv is how --check runs CMD: as words when it is plain words, else
// through sh -c ("=" too: "CI=1 make check" sets a variable).
func checkArgv(cmd string) []string {
	cmd = strings.TrimSpace(cmd)
	if strings.ContainsAny(cmd, "|&;<>()$`\\\"'*?[]{}~#=\n") {
		return []string{"sh", "-c", cmd}
	}
	return strings.Fields(cmd)
}

// runShipCheck runs --check CMD in the tests slot, as wp testsum does, and
// records it against the working tree; a pass already recorded on this
// tree for the same command isn't run again. An error: the check failed or
// couldn't run, and nothing ships.
func (c *cli) runShipCheck(cmd string) error {
	argv := checkArgv(cmd)
	if len(argv) == 0 {
		return c.usage("--check needs a command, as --check \"make check\"")
	}
	recorded := truncRunes(redactCommand(shellJoin(argv)), testRunCmdMax)
	if tree := workingTree(); tree != "" {
		on, _ := runsOnTree(readTestRuns(testRunsPath()), tree)
		for _, r := range on {
			if r.Command == recorded && r.Outcome == runPass {
				c.println("Check: " + cmd + " already passed on this tree; not run again.")
				return nil
			}
		}
	}
	var buf bytes.Buffer
	rec := newTestRunRecorder(argv)
	start := time.Now()
	var nothing bool
	code, err := c.runInSlotWith("tests", defaultSlots(), goTestJSON(argv), &buf, &buf, rec.started, func(env []string) []string {
		env, _ = testEnv(env, nil)
		return env
	}, slotOpts{who: "wp ship", allCmd: "wp testsum --all --", rec: rec, nothing: &nothing})
	if err != nil {
		return fmt.Errorf("--check couldn't run %s: %w (nothing shipped)", cmd, err)
	}
	if nothing {
		c.println("Check: " + cmd + " has nothing to run: no changed file is in the packages it asks for; not recorded as a pass.")
		return nil
	}
	res, _, _ := parseTestOutput(buf.Bytes(), "auto")
	if res == nil {
		res = &testResult{}
	}
	failed := code != 0 || res.Failed > 0 || len(res.Failures) > 0
	if failed && code != 0 && res.Failed == 0 && len(res.Failures) == 0 {
		rec.failed(code)
	} else {
		rec.finish(res, code)
	}
	took := time.Since(start).Round(time.Second)
	if !failed {
		line := fmt.Sprintf("Check passed: %s (%s)", cmd, took)
		if res.Passed+res.Failed+res.Skipped > 0 {
			line += "; " + strings.TrimSpace(res.render())
		}
		if rec.compileOnly {
			line += "; only compiled, no test ran, so it is not recorded as a pass"
		}
		c.println(line)
		return nil
	}
	if res.Passed+res.Failed+res.Skipped > 0 {
		_, _ = fmt.Fprint(c.out, res.renderSaving(buf.Bytes()))
	}
	_, _ = fmt.Fprintf(c.out, "Check failed: %s exited %d after %s; last lines:\n%s\n", cmd, code, took, lastLines(buf.Bytes(), 30))
	return fmt.Errorf("the check failed, so nothing was committed or shipped: fix it and run wp ship again")
}

// shipScopedCheck is how wp ship runs the repository's own scoped check when
// --check wasn't given (WP-ADR161): wp test, run as this binary and not
// whichever wp is on PATH. It takes the tests slot and records itself.
var shipScopedCheck = func(c *cli) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(exe, "test") // #nosec G204 -- this binary, a fixed argument
	cmd.Stdout, cmd.Stderr = c.err, c.err
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		return 0, err
	}
	return 0, nil
}

// runDefaultShipCheck runs wp test before a ship in a repository with a
// .waypoint/test, unless it already passed on this tree. CI still runs the
// whole suite on the pull request, so this is the quick check.
func (c *cli) runDefaultShipCheck() error {
	root, err := localGit("rev-parse", "--show-toplevel")
	if err != nil || !hasTestScript(root) {
		return nil
	}
	recorded := "wp test" + scopedMark
	if tree := workingTree(); tree != "" {
		on, _ := runsOnTree(readTestRuns(testRunsPath()), tree)
		for _, r := range on {
			if r.Command == recorded && r.Outcome == runPass {
				c.println("Check: wp test already passed on this tree; not run again.")
				return nil
			}
		}
	}
	c.println("Check: wp test (this repository's " + testScriptFile + "; --check CMD runs another, --no-check skips; CI still runs the whole suite on the pull request).")
	code, err := shipScopedCheck(c)
	if err != nil {
		return fmt.Errorf("wp test couldn't run: %w (nothing shipped; --no-check ships without it)", err)
	}
	if code != 0 {
		return fmt.Errorf("wp test failed (exit %d), so nothing was committed or shipped: fix what it names and run wp ship again, or wp ship --no-check to ship without it", code)
	}
	return nil
}
