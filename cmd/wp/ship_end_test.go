package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestIsShipCall(t *testing.T) {
	for cmd, want := range map[string]bool{
		`wp ship -m "x" --summary "y"`:                true,
		`make check && wp ship --summary "done"`:      true,
		`WAYPOINT_AGENT=x /usr/local/bin/wp done K-1`: true,
		`wp finish`:              true,
		`wp ship -m x; echo hi`:  false,
		`wp ship -m x | tail -3`: false,
		`wp show WP-1`:           false,
		`echo wp ship`:           false,
		`wp`:                     false,
		``:                       false,
	} {
		if got := isShipCall(cmd); got != want {
			t.Errorf("isShipCall(%q) = %v", cmd, got)
		}
	}
	if shipStopReason != core.StopEndedByShip {
		t.Error("the CLI's ship Stop reason differs from core.StopEndedByShip")
	}
}

func TestShipHookInputs(t *testing.T) {
	if got := bashCommand(json.RawMessage(`{"command":"wp ship","description":"Ship"}`)); got != "wp ship" {
		t.Errorf("bashCommand: %q", got)
	}
	if got := bashBackground(json.RawMessage(`{"stdout":"","backgroundTaskId":"b7x"}`)); got != "b7x" {
		t.Errorf("bashBackground: %q", got)
	}
	if got := bashBackground(json.RawMessage(`{"stdout":"PR 4 armed"}`)); got != "" {
		t.Errorf("bashBackground of a foreground call: %q", got)
	}
	ok := "<task-notification>\n<task-id>b7x</task-id>\n<status>completed</status>\n<summary>Background command \"Ship\" completed (exit code 0)</summary>\n</task-notification>"
	for prompt, want := range map[string]string{
		ok: "b7x",
		strings.Replace(ok, "exit code 0", "exit code 1", 1):            "",
		strings.Replace(ok, "completed</status>", "failed</status>", 1): "",
		"Please ship it": "",
	} {
		if got := finishedTask(prompt); got != want {
			t.Errorf("finishedTask(%q) = %q, want %q", prompt, got, want)
		}
	}
}

func TestShipSummaryText(t *testing.T) {
	d := &prDetail{Number: 41, URL: "https://git.example.com/a/b/pulls/41", Title: "Fix it"}
	if got := fillSummary("{task} shipped in PR #{pr}.", "WP-9", d); got != "WP-9 shipped in PR #41.\n\nPR #41: https://git.example.com/a/b/pulls/41" {
		t.Errorf("fillSummary: %q", got)
	}
	if got := fillSummary("See {url}.", "", d); got != "See https://git.example.com/a/b/pulls/41." {
		t.Errorf("fillSummary with the URL: %q", got)
	}
	if got := heldSummary("{task} goes to PR #{pr}."); !strings.HasPrefix(got, "this task goes to PR #(after the review).") || !strings.Contains(got, "opens after the adversarial review") {
		t.Errorf("heldSummary: %q", got)
	}
	for cmd, want := range map[string][]string{
		"make check":                 {"make", "check"},
		"go test ./...":              {"go", "test", "./..."},
		"make check && npm run lint": {"sh", "-c", "make check && npm run lint"},
		`go test -run 'TestA|TestB'`: {"sh", "-c", `go test -run 'TestA|TestB'`},
	} {
		if got := checkArgv(cmd); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("checkArgv(%q) = %q", cmd, got)
		}
	}
}

func TestShipEndState(t *testing.T) {
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	c, _ := newCLI(new(bytes.Buffer), new(bytes.Buffer))
	// Kept summaries are per task and survive a new CLI.
	c.keepSummary("wp-9", "Kept.")
	c, _ = newCLI(new(bytes.Buffer), new(bytes.Buffer))
	if got, kept := c.shipSummary("WP-9", "", false); got != "Kept." || !kept {
		t.Errorf("kept summary: %q %v", got, kept)
	}
	if got, kept := c.shipSummary("WP-9", "New.", true); got != "New." || kept {
		t.Errorf("a given summary: %q %v", got, kept)
	}
	if got, _ := c.shipSummary("WP-9", " ", true); got != "" {
		t.Errorf("--summary \"\": %q", got)
	}
	c, _ = newCLI(new(bytes.Buffer), new(bytes.Buffer))
	if got := c.keptSummaryOf("WP-9"); got != "" {
		t.Errorf("--summary \"\" left %q kept", got)
	}

	// A marker is taken once, and only while fresh.
	c.shipSession("s1", func(e *shipEnd) { e.Ready, e.Text = time.Now(), "Done." })
	if e := c.takeShipEnd("s1", ""); e == nil || e.Text != "Done." {
		t.Errorf("fresh marker: %+v", e)
	}
	if e := c.takeShipEnd("s1", ""); e != nil {
		t.Errorf("taken twice: %+v", e)
	}
	c.shipSession("s1", func(e *shipEnd) { e.Ready, e.Text = time.Now().Add(-3*time.Minute), "Old." })
	if e := c.takeShipEnd("s1", ""); e != nil {
		t.Errorf("stale marker taken: %+v", e)
	}

	// A background wp ship: its report takes the marker it wrote since it
	// started, even after the 2 minutes, and nothing else does.
	c.noteShipBackground("s1", "b1")
	c.shipSession("s1", func(e *shipEnd) {
		e.Bg["b1"] = time.Now().Add(-10 * time.Minute)
		e.Ready, e.Text = time.Now().Add(-5*time.Minute), "Shipped in the background."
	})
	if e := c.takeShipEnd("s1", "other"); e != nil {
		t.Errorf("another task's report took it: %+v", e)
	}
	if e := c.takeShipEnd("s1", "b1"); e == nil || e.Text != "Shipped in the background." {
		t.Errorf("background marker: %+v", e)
	}
	c.noteShipBackground("s1", "b2")
	if e := c.takeShipEnd("s1", "b2"); e != nil {
		t.Errorf("a background ship that wrote no marker ended the turn: %+v", e)
	}

	// The Stop a hook sent: the Stop hook skips it once; a new prompt
	// forgets it.
	c.shipSession("s1", func(e *shipEnd) { e.Stopped = time.Now() })
	c, _ = newCLI(new(bytes.Buffer), new(bytes.Buffer))
	if !c.stopAlreadySent("s1") {
		t.Error("the sent Stop wasn't seen")
	}
	c, _ = newCLI(new(bytes.Buffer), new(bytes.Buffer))
	if c.stopAlreadySent("s1") {
		t.Error("the sent Stop was skipped twice")
	}
	c.shipSession("s1", func(e *shipEnd) { e.Stopped = time.Now() })
	c, _ = newCLI(new(bytes.Buffer), new(bytes.Buffer))
	c.forgetStopSent("s1")
	c, _ = newCLI(new(bytes.Buffer), new(bytes.Buffer))
	if c.stopAlreadySent("s1") || len(c.state.ShipEnds) != 0 {
		t.Errorf("after a new prompt: %+v", c.state.ShipEnds)
	}
}

// The hooks end the turn wp ship marked: PostToolUse prints the stop with
// the summary and sends the session's Stop, and the Stop hook after it
// sends nothing more.
func TestShipEndsTheTurnThroughTheHooks(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	s.hook(in("sess-s", "SessionStart", s.repo, map[string]any{"source": "startup"}))
	s.hook(in("sess-s", "UserPromptSubmit", s.repo, map[string]any{"prompt": "Fix it"}))
	t.Setenv("WAYPOINT_SESSION", "sess-s")
	burn := func(cmd string, resp string) string {
		b, _ := json.Marshal(in("sess-s", "PostToolUse", s.repo, map[string]any{"tool_name": "Bash",
			"tool_input": map[string]any{"command": cmd}, "tool_response": json.RawMessage(resp)}))
		var out bytes.Buffer
		c, _ := newCLI(&out, &out)
		hookClaudeBurn(c, bytes.NewReader(b), &out)
		return out.String()
	}
	c, _ := newCLI(new(bytes.Buffer), new(bytes.Buffer))
	c.keepSummary("WP-1", "{task} shipped")
	if !c.endTurnAfterShip("WP-1", "WP-1 shipped in PR #4.") {
		t.Fatal("no marker in an agent session")
	}
	out := burn(`make check && wp ship -m "Fix" --summary "{task} shipped"`, `{"stdout":"PR 4 armed"}`)
	var o struct {
		Continue   *bool  `json:"continue"`
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal([]byte(out), &o); err != nil || o.Continue == nil || *o.Continue || o.StopReason != "WP-1 shipped in PR #4." {
		t.Fatalf("PostToolUse after wp ship: %q (%v)", out, err)
	}
	// The summary was kept until the hook's Stop went: it is shown now.
	if c, _ := newCLI(new(bytes.Buffer), new(bytes.Buffer)); c.keptSummaryOf("WP-1") != "" {
		t.Error("the summary the turn ended with is still kept")
	}
	list := s.sessions()
	if len(list) != 1 || list[0].LastUpdate != "WP-1 shipped in PR #4." {
		t.Fatalf("sessions after the stop: %+v", list)
	}
	// Nothing said to run in the background: the session is finished, as
	// after a Stop with nothing pending.
	if x := list[0]; x.State == "working" || x.FinishedAt == nil || x.Pending != "" {
		t.Errorf("after the hook's stop: state %q pending %q finished %v", x.State, x.Pending, x.FinishedAt)
	}
	// The Stop hook after it says only what still runs: the summary stays
	// the last update.
	running := []map[string]any{{"id": "b1", "type": "shell", "status": "running", "description": "dev server"}}
	if out := s.hook(in("sess-s", "Stop", s.repo, map[string]any{"last_assistant_message": "Another reply", "background_tasks": running, "session_crons": []any{}})); strings.Contains(out, "Another reply") {
		t.Errorf("the Stop hook after the hook's stop printed %q", out)
	}
	if x := s.sessions()[0]; x.LastUpdate != "WP-1 shipped in PR #4." || x.FinishedAt != nil || x.Pending == "" {
		t.Errorf("after the Stop hook: update %q pending %q finished %v", x.LastUpdate, x.Pending, x.FinishedAt)
	}
	// A second ship call has no marker: the turn goes on.
	if out := burn("wp ship", `{"stdout":""}`); strings.Contains(out, `"continue":false`) {
		t.Errorf("a ship without a marker ended the turn: %s", out)
	}
	// A marker the ship's own call didn't take (its output piped on) is
	// dropped by the next call, so a later ship can't end the turn with it.
	c, _ = newCLI(new(bytes.Buffer), new(bytes.Buffer))
	c.endTurnAfterShip("WP-1", "Stale.")
	burn("wp ship --summary x | tail -3", `{"stdout":""}`)
	burn("git log -1", `{"stdout":""}`)
	if out := burn("wp ship --help", `{"stdout":""}`); strings.Contains(out, `"continue":false`) {
		t.Errorf("a later call took the ship's marker: %s", out)
	}
	// The server can't be reached: the turn goes on, and the summary stays
	// kept for the next wp ship (audit 1010.3).
	server := os.Getenv("WAYPOINT_SERVER")
	t.Setenv("WAYPOINT_SERVER", "http://127.0.0.1:1")
	c, _ = newCLI(new(bytes.Buffer), new(bytes.Buffer))
	c.keepSummary("WP-1", "Kept.")
	c.endTurnAfterShip("WP-1", "Kept.")
	if out := burn("wp ship", `{"stdout":""}`); strings.Contains(out, `"continue":false`) {
		t.Errorf("the turn ended with no Stop sent: %s", out)
	}
	if c, _ := newCLI(new(bytes.Buffer), new(bytes.Buffer)); c.keptSummaryOf("WP-1") != "Kept." {
		t.Error("the summary went with a Stop that wasn't sent")
	}
	// The next prompt starts another turn: the summary written for this one
	// goes (audit 1010.1).
	t.Setenv("WAYPOINT_SERVER", server)
	s.hook(in("sess-s", "UserPromptSubmit", s.repo, map[string]any{"prompt": "Now the review's findings"}))
	if c, _ := newCLI(new(bytes.Buffer), new(bytes.Buffer)); c.keptSummaryOf("WP-1") != "" {
		t.Error("the summary outlived its turn")
	}
}

// shipEndsTurn: wp ship --summary ends an agent's turn when the ship goes as
// expected, and keeps the summary for the task's next wp ship when it
// doesn't. Run by TestCLIPROpenAndShip on its armed pull request #41, whose
// task has a step put off.
func shipEndsTurn(t *testing.T, e *env, me *core.Actor, key string) {
	t.Helper()
	ctx := context.Background()
	t.Setenv("WAYPOINT_SESSION", "sess-ship")
	// The test's untracked pr-checks.md would put the check's tree off the
	// commit's.
	if err := os.WriteFile(filepath.Join(".git", "info", "exclude"), []byte(".waypoint/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := func() State {
		c, err := newCLI(new(bytes.Buffer), new(bytes.Buffer))
		if err != nil {
			t.Fatal(err)
		}
		return c.state
	}
	// A failed check: nothing ships, the summary waits.
	out, code := e.run("ship", "--check", "false", "--summary", "{task} is in PR #{pr}.")
	if code == 0 || !strings.Contains(out, "Check failed: false exited 1") || !strings.Contains(out, "nothing was committed or shipped") || strings.Contains(out, "lines of its output") {
		t.Errorf("failed check: %d\n%s", code, out)
	}
	if k := state().Summaries[key]; k == nil || k.Text != "{task} is in PR #{pr}." {
		t.Errorf("summary not kept after a failed check: %+v", k)
	}
	// A failure whose error is above the lines shown: the whole output is
	// kept in a file the message names (WP-1812).
	out, code = e.run("ship", "--check", "printf 'a lint %s\\n' finding; seq 40; exit 3", "--summary", "{task} is in PR #{pr}.")
	m := regexp.MustCompile(`All 41 lines of its output \(kept until the next long failure here, or until this checkout is removed\): (.+)`).FindStringSubmatch(out)
	if code == 0 || strings.Contains(out, "a lint finding") || m == nil {
		t.Fatalf("long failed check: %d\n%s", code, out)
	}
	want := "a lint finding\n"
	for i := 1; i <= 40; i++ {
		want += strconv.Itoa(i) + "\n"
	}
	if kept, err := os.ReadFile(m[1]); err != nil || string(kept) != want {
		t.Errorf("kept output: %v\n%s", err, kept)
	}
	// The file is the checkout's git directory's, not a temp file.
	gp, gerr := exec.Command("git", "rev-parse", "--git-path", "wp-failed-output.log").Output()
	if gerr != nil {
		t.Fatal(gerr)
	}
	wantPath, _ := filepath.Abs(strings.TrimSpace(string(gp)))
	if gotPath, _ := filepath.EvalSymlinks(filepath.Dir(m[1])); gotPath != "" {
		gotPath = filepath.Join(gotPath, filepath.Base(m[1]))
		wantDir, _ := filepath.EvalSymlinks(filepath.Dir(wantPath))
		if gotPath != filepath.Join(wantDir, filepath.Base(wantPath)) {
			t.Errorf("kept at %s, want %s", m[1], wantPath)
		}
	} else {
		t.Errorf("kept at %s, want %s", m[1], wantPath)
	}
	// A step put off needs the agent: the kept summary is used and kept
	// again, and the usual reply follows. A marker this ship didn't write
	// (a subagent's wp ship in this session) goes when it starts, so the
	// call can't end the turn with it (audit 1010.7).
	c, _ := newCLI(new(bytes.Buffer), new(bytes.Buffer))
	c.shipSession("sess-ship", func(e *shipEnd) { e.Ready, e.Text = time.Now(), "A subagent's." })
	out = e.wp("ship", "--check", "true")
	if !strings.Contains(out, "Check passed: true") || !strings.Contains(out, "Using the --summary you gave this task's last wp ship") ||
		strings.Contains(out, "kept, not shown") || !strings.Contains(out, "Done: "+key) {
		// (WP-1370: the line saying the summary was kept is gone.)
		t.Errorf("ship with a reason to go on:\n%s", out)
	}
	if s := state(); s.Summaries[key] == nil || s.ShipEnds["sess-ship"] != nil {
		t.Errorf("a ship that needs the agent: kept %+v, ends %+v", s.Summaries[key], s.ShipEnds["sess-ship"])
	}
	cs, err := e.svc.ListComments(ctx, me, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, cm := range cs {
		if strings.Contains(cm.Body, "Step 2 only if") {
			if err := e.svc.DeleteComment(ctx, me, cm.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Nothing needs the agent: the turn ends with the summary, filled in,
	// even when Waypoint tells it which tasks' recent work the change
	// replaces (WP-1370: information, not a reason to go on).
	oldFn := rewrittenTasksFn
	rewrittenTasksFn = func(*cli, string, string) []rewrittenTask {
		return []rewrittenTask{{Key: "WP-2", Title: "Earlier", PR: 3, Lines: 20, Share: 0.5}}
	}
	out = e.wp("ship", "--check", "true", "--summary", "{task} is in PR #{pr}. Tests: true.")
	rewrittenTasksFn = oldFn
	if !strings.Contains(out, "WP-2") || !strings.Contains(out, "Earlier") {
		t.Errorf("the rewrite line wasn't printed:\n%s", out)
	}
	if !strings.Contains(out, "Check: true already passed on this tree; not run again.") || !strings.Contains(out, "Waypoint ends the turn here") ||
		strings.Contains(out, "Done: ") || strings.Contains(out, "Reply template") {
		t.Errorf("ship that ends the turn: %q", out)
	}
	s := state()
	end := s.ShipEnds["sess-ship"]
	if end == nil || end.Key != key || !strings.HasPrefix(end.Text, key+" is in PR #41. Tests: true.\n\nPR #41: https://") || time.Since(end.Ready) > time.Minute {
		t.Errorf("turn-end marker: %+v", end)
	}
	if k := s.Summaries[key]; k == nil || k.Text != "{task} is in PR #{pr}. Tests: true." {
		t.Errorf("the summary isn't kept until the hook ends the turn with it: %+v", k)
	}
	// An agent with no session the hooks know: the turn can't end, so the
	// usual reply follows and the summary waits.
	t.Setenv("WAYPOINT_SESSION", "")
	out = e.wp("ship", "--summary", "Later.")
	if !strings.Contains(out, "--summary can't end the turn here") || strings.Contains(out, "Waypoint ends the turn") || !strings.Contains(out, "Done: "+key) {
		t.Errorf("agent ship outside a known session:\n%s", out)
	}
	if k := state().Summaries[key]; k == nil || k.Text != "Later." {
		t.Errorf("summary not kept when the turn can't end: %+v", k)
	}
	// A person's ship: the summary has no turn to end.
	t.Setenv("WAYPOINT_AGENT", "none")
	if out = e.wp("ship", "--summary", "x"); strings.Contains(out, "Waypoint ends the turn") {
		t.Errorf("a person's ship ended a turn:\n%s", out)
	}
}

// --check runs a command with an environment prefix through the shell:
// "CI=1" isn't a program (audit 1010.2).
func TestCheckArgv(t *testing.T) {
	for cmd, want := range map[string]string{
		"make check":         "make|check",
		"  go test ./... ":   "go|test|./...",
		"CI=1 make check":    "sh|-c|CI=1 make check",
		"make check && wp x": "sh|-c|make check && wp x",
		"make check ARGS=-v": "sh|-c|make check ARGS=-v",
		"":                   "",
	} {
		if got := strings.Join(checkArgv(cmd), "|"); got != want {
			t.Errorf("checkArgv(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// A summary kept by a session waits only for that session's turn: a new
// prompt (a person's message, a review's findings) drops it, so a later
// wp ship can't end the turn with a reply written before it (audit 1010.1).
func TestKeptSummaryEndsWithTheTurn(t *testing.T) {
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SESSION", "s1")
	c, _ := newCLI(new(bytes.Buffer), new(bytes.Buffer))
	c.keepSummary("WP-1", "One.")
	t.Setenv("WAYPOINT_SESSION", "s2")
	c.keepSummary("WP-2", "Two.")
	c, _ = newCLI(new(bytes.Buffer), new(bytes.Buffer))
	c.dropSessionSummaries("s1")
	c, _ = newCLI(new(bytes.Buffer), new(bytes.Buffer))
	if got := c.keptSummaryOf("WP-1"); got != "" {
		t.Errorf("s1's summary outlived its turn: %q", got)
	}
	if got := c.keptSummaryOf("WP-2"); got != "Two." {
		t.Errorf("s2's summary went with s1's turn: %q", got)
	}
}

// With no code to ship, --check doesn't run, with --json or without
// (audit 1010.4): the task finishes either way.
func TestNoCodeShipSkipsCheck(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Chdir(t.TempDir())
	ctx := context.Background()
	p, _ := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Checks")})
	for _, json := range []bool{false, true} {
		issue, _ := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "No code"})
		args := []string{"done", issue.Key, "-m", "Answered", "--check", "false"}
		if json {
			args = append(args, "--json")
		}
		if out, code := e.run(args...); code != 0 || strings.Contains(out, "Check failed") {
			t.Errorf("json %v: --check ran with no code to ship: %d\n%s", json, code, out)
		}
	}
}

// A wp ship that waited an hour on its review round and was moved to the
// background writes its marker at the end: the report of that background
// call still takes it (WP-1669).
func TestShipEndMarkerAfterALongBackgroundWait(t *testing.T) {
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_AGENT", "claude")
	t.Setenv("WAYPOINT_SESSION", "sess-long")
	t.Setenv("WAYPOINT_SERVER", "http://127.0.0.1:1")
	t.Setenv("WAYPOINT_TOKEN", "x")
	c, _ := newCLI(new(bytes.Buffer), new(bytes.Buffer))
	c.noteShipBackground("sess-long", "b1")
	c.shipSession("sess-long", func(e *shipEnd) { e.Bg["b1"] = time.Now().Add(-60 * time.Minute) })
	if !c.endTurnAfterShip("WP-1", "Shipped.") {
		t.Fatal("no marker was written for an agent's ship in a session with a server and a token")
	}
	if e := c.takeShipEnd("sess-long", "b1"); e == nil || e.Text != "Shipped." {
		t.Fatalf("the marker written at the end of an hour's wait was refused: %+v", e)
	}
}

func TestSlotWaitNote(t *testing.T) {
	for w, want := range map[time.Duration]string{
		0:                                     "",
		59 * time.Second:                      "",
		59*time.Second + 400*time.Millisecond: "",
		59*time.Second + 500*time.Millisecond: ", and 1m0s waiting for a slot",
		60 * time.Second:                      ", and 1m0s waiting for a slot",
		44 * time.Minute:                      ", and 44m0s waiting for a slot",
	} {
		if got := slotWaitNote(w); got != want {
			t.Errorf("slotWaitNote(%v) = %q, want %q", w, got, want)
		}
	}
}

func TestCheckLines(t *testing.T) {
	wait := 43*time.Minute + 46*time.Second
	for got, want := range map[string]string{
		checkPassedLine("make check", 25*time.Minute+time.Second, 0):            "Check passed: make check (25m1s)",
		checkPassedLine("make check", 25*time.Minute+time.Second, wait):         "Check passed: make check (25m1s, and 43m46s waiting for a slot)",
		checkFailedHead("make check", 3, 25*time.Minute+time.Second, 0, nil):    "Check failed: make check exited 3 after 25m1s; last lines:\n",
		checkFailedHead("make check", 3, 25*time.Minute+time.Second, wait, nil): "Check failed: make check exited 3 after 25m1s, and 43m46s waiting for a slot; last lines:\n",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// A check that queued behind another reports its own time, not the wait
// (WP-1812): the slots are held for about 2s, the check itself takes none.
func TestShipCheckTimeLeavesOutTheSlotWait(t *testing.T) {
	slotEnv(t)
	t.Chdir(t.TempDir())
	var holds []*slotHold
	for range defaultSlots() {
		h, err := acquireSlot(t.Context(), "tests", defaultSlots(), nil)
		if err != nil {
			t.Fatal(err)
		}
		holds = append(holds, h)
	}
	go func() {
		time.Sleep(2 * time.Second)
		for _, h := range holds {
			h.release()
		}
	}()
	var out bytes.Buffer
	c, err := newCLI(&out, new(bytes.Buffer))
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if err := c.runShipCheck("true"); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(t0); waited < 1500*time.Millisecond {
		t.Fatalf("the check didn't wait for the slots: %v", waited)
	}
	// 1s allows for a loaded machine; the wait alone would make it 2s.
	if !strings.Contains(out.String(), "Check passed: true (0s)") && !strings.Contains(out.String(), "Check passed: true (1s)") {
		t.Errorf("the wait was counted as the check's time:\n%s", out.String())
	}
	if strings.Contains(out.String(), "waiting for a slot") {
		t.Errorf("a wait below the note threshold was named:\n%s", out.String())
	}
}

// The wait is named when it passes the threshold, for a passing check and a
// failing one; the threshold is lowered so the test stays short.
func TestShipCheckNamesTheSlotWait(t *testing.T) {
	slotEnv(t)
	t.Chdir(t.TempDir())
	old := slotWaitNoteMin
	slotWaitNoteMin = time.Second
	t.Cleanup(func() { slotWaitNoteMin = old })
	var holds []*slotHold
	for range defaultSlots() {
		h, err := acquireSlot(t.Context(), "tests", defaultSlots(), nil)
		if err != nil {
			t.Fatal(err)
		}
		holds = append(holds, h)
	}
	go func() {
		time.Sleep(2 * time.Second)
		for _, h := range holds {
			h.release()
		}
	}()
	var out bytes.Buffer
	c, err := newCLI(&out, new(bytes.Buffer))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.runShipCheck("true"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Check passed: true") || !strings.Contains(out.String(), "waiting for a slot") {
		t.Errorf("passing check:\n%s", out.String())
	}
}

func TestShipCheckFailureNamesTheSlotWait(t *testing.T) {
	slotEnv(t)
	t.Chdir(t.TempDir())
	old := slotWaitNoteMin
	slotWaitNoteMin = time.Second
	t.Cleanup(func() { slotWaitNoteMin = old })
	var holds []*slotHold
	for range defaultSlots() {
		h, err := acquireSlot(t.Context(), "tests", defaultSlots(), nil)
		if err != nil {
			t.Fatal(err)
		}
		holds = append(holds, h)
	}
	go func() {
		time.Sleep(2 * time.Second)
		for _, h := range holds {
			h.release()
		}
	}()
	var out bytes.Buffer
	c, err := newCLI(&out, new(bytes.Buffer))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.runShipCheck("false"); err == nil {
		t.Fatal("a failing check passed")
	}
	if !strings.Contains(out.String(), "Check failed: false exited 1") || !strings.Contains(out.String(), "waiting for a slot") {
		t.Errorf("failing check:\n%s", out.String())
	}
}

// A failed check shows 30 lines of output; a pointer to the file comes only
// with more.
func TestShipCheckPointerPastThirtyLines(t *testing.T) {
	slotEnv(t)
	t.Setenv("TMPDIR", t.TempDir())
	t.Chdir(t.TempDir())
	for n, want := range map[int]bool{30: false, 31: true} {
		var out bytes.Buffer
		c, err := newCLI(&out, new(bytes.Buffer))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.runShipCheck("seq " + strconv.Itoa(n) + "; exit 3"); err == nil {
			t.Fatalf("%d lines: the check passed", n)
		}
		if got := strings.Contains(out.String(), "lines of its output"); got != want {
			t.Errorf("%d lines: pointer %v, want %v:\n%s", n, got, want, out.String())
		}
	}
}

func TestCheckFailedHeadNamesTheLoad(t *testing.T) {
	old := cpuCount
	cpuCount = func() int { return 8 }
	t.Cleanup(func() { cpuCount = old })
	got := checkFailedHead("make check", 3, time.Minute, 0, []float64{15.4, 19.2})
	want := "Check failed: make check exited 3 after 1m0s; load 15.4 at the start, 19.2 at the end, on 8 CPUs; last lines:\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// loadReader returns the readings in order, then none.
func loadReader(vals ...float64) func() (float64, bool) {
	return func() (float64, bool) {
		if len(vals) == 0 {
			return 0, false
		}
		v := vals[0]
		vals = vals[1:]
		return v, true
	}
}

func shipCheckWithLoad(t *testing.T, cmd string, read func() (float64, bool)) string {
	t.Helper()
	slotEnv(t)
	t.Chdir(t.TempDir())
	oldLoad, oldCPU := readHostLoad, cpuCount
	readHostLoad, cpuCount = read, func() int { return 8 }
	t.Cleanup(func() { readHostLoad, cpuCount = oldLoad, oldCPU })
	var out bytes.Buffer
	c, err := newCLI(&out, new(bytes.Buffer))
	if err != nil {
		t.Fatal(err)
	}
	_ = c.runShipCheck(cmd)
	return out.String()
}

func TestShipCheckFailureNamesTheLoad(t *testing.T) {
	out := shipCheckWithLoad(t, "false", loadReader(15.4, 19.2))
	if !strings.Contains(out, "load 15.4 at the start, 19.2 at the end, on 8 CPUs") {
		t.Errorf("no load in:\n%s", out)
	}
}

func TestShipCheckFailureWithoutLoadSaysNothingOfIt(t *testing.T) {
	for name, read := range map[string]func() (float64, bool){"none": loadReader(), "one": loadReader(3)} {
		if out := shipCheckWithLoad(t, "false", read); !strings.Contains(out, "Check failed: false exited 1") || strings.Contains(out, "load") {
			t.Errorf("%s:\n%s", name, out)
		}
	}
}

func TestShipCheckPassLineLeavesOutTheLoad(t *testing.T) {
	out := shipCheckWithLoad(t, "true", loadReader(1, 2))
	if !strings.Contains(out, "Check passed: true") || strings.Contains(out, "load") {
		t.Errorf("passing check:\n%s", out)
	}
}
