package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitEnv is an agent's session whose ship was held, and a server answering
// the wait with each of answers in turn (the last repeats).
func waitEnv(t *testing.T, answers ...func(w http.ResponseWriter)) (*cli, *bytes.Buffer, *atomic.Int32) {
	t.Helper()
	c, out, calls, _ := waitEnvReqs(t, answers...)
	return c, out, calls
}

// waitReqs are the requests the fake server saw (acks included, though they
// are not counted as calls and are always answered "acked").
type waitReqs struct {
	mu   sync.Mutex
	list []map[string]any
}

func (r *waitReqs) all() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.list...)
}

func waitEnvReqs(t *testing.T, answers ...func(w http.ResponseWriter)) (*cli, *bytes.Buffer, *atomic.Int32, *waitReqs) {
	t.Helper()
	var calls atomic.Int32
	reqs := &waitReqs{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions/hook/ship-news" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		reqs.mu.Lock()
		reqs.list = append(reqs.list, body)
		reqs.mu.Unlock()
		if _, ack := body["ack"]; ack {
			_ = json.NewEncoder(w).Encode(map[string]any{"outcome": "acked"})
			return
		}
		n := int(calls.Add(1)) - 1
		answers[min(n, len(answers)-1)](w)
	}))
	t.Cleanup(ts.Close)
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", ts.URL)
	t.Setenv("WAYPOINT_TOKEN", "wpt_x")
	t.Setenv("WAYPOINT_AGENT", "claude")
	t.Setenv("WAYPOINT_SESSION", "sess-wait")
	oldSleep, oldChunk := pollSleep, shipChunk
	pollSleep, shipChunk = func(time.Duration) {}, 2*time.Second
	t.Cleanup(func() { pollSleep, shipChunk = oldSleep, oldChunk })
	var out bytes.Buffer
	c, err := newCLI(&out, &out)
	if err != nil {
		t.Fatal(err)
	}
	return c, &out, &calls, reqs
}

func say(v any) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) { _ = json.NewEncoder(w).Encode(v) }
}

func waitingAnswer() func(w http.ResponseWriter) { return say(map[string]any{"outcome": "waiting"}) }

func heldFlags() prOpenFlags { return prOpenFlags{review: new(bool), force: new(bool)} }

func heldShip(t *testing.T, c *cli, noWait bool) (*openedPR, []byte, bool, error) {
	t.Helper()
	wait := time.Hour // room for every backoff: a failure with none left ends the wait instead
	so := shipOpts{noWait: &noWait, waitTimeout: &wait}
	o := &openedPR{Held: "Pushed. Nothing more to run: end your turn.", HeldNeedsAgent: new(bool)}
	if !c.canWaitShip(so, o) {
		return o, nil, false, nil
	}
	return c.waitHeldShip(heldFlags(), so, o, []byte(`{}`), "WP-9", "Shipped {task}.")
}

// Findings come back: they are printed in full, the ship is over, nothing ends
// the turn, and the summary stays kept for the task's next ship.
func TestHeldShipPrintsTheFindings(t *testing.T) {
	c, out, calls := waitEnv(t, waitingAnswer(), waitingAnswer(), say(map[string]any{"outcome": "findings", "text": "Audit of the branch: 2 findings\nfix the nil check"}))
	_, _, done, err := heldShip(t, c, false)
	if err != nil || !done {
		t.Fatalf("done %v, err %v", done, err)
	}
	s := out.String()
	if !strings.Contains(s, "waiting for WP-9's review") || !strings.Contains(s, "fix the nil check") || strings.Contains(s, "end your turn") || calls.Load() != 3 {
		t.Errorf("output after %d calls:\n%s", calls.Load(), s)
	}
	c2, _ := newCLI(new(bytes.Buffer), new(bytes.Buffer))
	if got := c2.keptSummaryOf("WP-9"); got != "Shipped {task}." {
		t.Errorf("kept summary %q", got)
	}
	if e := c2.state.ShipEnds["sess-wait"]; e != nil && !e.Ready.IsZero() {
		t.Errorf("a marker was written: %+v", e)
	}
}

// A transient error is retried.
func TestHeldShipRetriesAfterATransientError(t *testing.T) {
	c, out, _ := waitEnv(t, func(w http.ResponseWriter) { http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway) },
		say(map[string]any{"outcome": "ended", "text": "The review round for this task ended (failed)."}))
	if _, _, done, err := heldShip(t, c, false); err != nil || !done || !strings.Contains(out.String(), "ended (failed)") {
		t.Fatalf("done %v, err %v:\n%s", done, err, out.String())
	}
}

// The pull request opened: the ship goes on as one that opened it, with the
// pull request the server found.
func TestHeldShipContinuesWhenThePullRequestOpens(t *testing.T) {
	c, out, _ := waitEnv(t, waitingAnswer(), say(map[string]any{"outcome": "pull", "pull": map[string]any{"repo": "a/b", "number": 5, "url": "https://git.example/a/b/pulls/5", "state": "open"}}))
	old := reopenPR
	reopenPR = func(*cli, prOpenFlags) (*openedPR, []byte, error) {
		return &openedPR{Existing: true, Pull: &prDetail{Number: 5, URL: "https://git.example/a/b/pulls/5"}}, []byte(`{"pull":{}}`), nil
	}
	t.Cleanup(func() { reopenPR = old })
	o, _, done, err := heldShip(t, c, false)
	if err != nil || done || o.Held != "" || o.Pull == nil || o.Pull.Number != 5 {
		t.Fatalf("o %+v, done %v, err %v", o, done, err)
	}
	if !strings.Contains(out.String(), "PR 5 (open): https://git.example/a/b/pulls/5") {
		t.Errorf("output:\n%s", out.String())
	}
}

// An older server has no wait: nothing is printed and the ship ends as before.
func TestHeldShipAgainstAnOlderServer(t *testing.T) {
	c, out, calls := waitEnv(t, func(w http.ResponseWriter) { http.Error(w, "404 page not found", http.StatusNotFound) })
	o, _, done, err := heldShip(t, c, false)
	if err != nil || done || o.Held == "" || out.Len() != 0 || calls.Load() != 1 {
		t.Fatalf("o %+v done %v err %v after %d calls; output %q", o, done, err, calls.Load(), out.String())
	}
}

// --no-wait is today's behaviour: the server isn't asked.
func TestHeldShipNoWait(t *testing.T) {
	c, out, calls := waitEnv(t, say(map[string]any{"outcome": "findings", "text": "x"}))
	o, _, done, err := heldShip(t, c, true)
	if err != nil || done || o.Held == "" || out.Len() != 0 || calls.Load() != 0 {
		t.Fatalf("done %v err %v after %d calls; output %q", done, err, calls.Load(), out.String())
	}
}

// A held answer that asks the agent for something (findings to mark, its own
// report), or doesn't say whether it does, is the agent's now: no wait.
func TestHeldShipThatNeedsTheAgentDoesNotWait(t *testing.T) {
	c, _, _ := waitEnv(t, say(map[string]any{"outcome": "findings", "text": "x"}))
	no, yes := false, true
	so := shipOpts{noWait: &no}
	for _, o := range []*openedPR{{Held: "mark these", HeldNeedsAgent: &yes}, {Held: "older server"}} {
		if c.canWaitShip(so, o) {
			t.Fatalf("a ship held with %+v waits", o)
		}
	}
	if !c.canWaitShip(so, &openedPR{Held: "end your turn", HeldNeedsAgent: &no}) {
		t.Fatal("a ship held with nothing for the agent doesn't wait")
	}
}

// Past the bound the ship says so and ends as before.
func TestHeldShipGivesUpAtTheBound(t *testing.T) {
	c, out, _ := waitEnv(t, waitingAnswer())
	so := shipOpts{noWait: new(bool), waitTimeout: new(time.Duration)}
	*so.waitTimeout = 1500 * time.Millisecond
	o := &openedPR{Held: "held"}
	o2, _, done, err := c.waitHeldShip(heldFlags(), so, o, nil, "WP-9", "")
	if err != nil || done || o2 != o || !strings.Contains(out.String(), "stopped waiting here") {
		t.Fatalf("done %v err %v:\n%s", done, err, out.String())
	}
}

// recordSleeps records the backoffs a wait would have slept.
func recordSleeps(t *testing.T) *[]time.Duration {
	t.Helper()
	old := pollSleep
	var slept []time.Duration
	pollSleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { pollSleep = old })
	return &slept
}

func badGateway(w http.ResponseWriter) {
	http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
}

// A held request that fails with no room before the bound to ask again: the
// ship says it lost touch and why, not that the review is still going.
func TestHeldShipSaysItLostTouchAtTheBound(t *testing.T) {
	c, out, calls := waitEnv(t, waitingAnswer(), badGateway)
	slept := recordSleeps(t)
	so := shipOpts{noWait: new(bool), waitTimeout: new(time.Duration)}
	*so.waitTimeout = 1500 * time.Millisecond
	o := &openedPR{Held: "held"}
	o2, _, done, err := c.waitHeldShip(heldFlags(), so, o, nil, "WP-9", "")
	s := out.String()
	if err != nil || done || o2 != o || calls.Load() != 2 || len(*slept) != 0 || !strings.Contains(s, "Lost touch with Waypoint while waiting") || !strings.Contains(s, "bad gateway") || strings.Contains(s, "still going") {
		t.Fatalf("done %v err %v after %d calls, slept %v:\n%s", done, err, calls.Load(), *slept, s)
	}
}

// A first request that keeps failing up to the bound: the ship ends as it
// did before the wait existed, silently, and never backs off past the bound.
func TestHeldShipProbeFailingAtTheBoundIsQuiet(t *testing.T) {
	c, out, _ := waitEnv(t, badGateway)
	slept := recordSleeps(t)
	so := shipOpts{noWait: new(bool), waitTimeout: new(time.Duration)}
	*so.waitTimeout = 1500 * time.Millisecond
	o := &openedPR{Held: "held"}
	o2, _, done, err := c.waitHeldShip(heldFlags(), so, o, nil, "WP-9", "")
	if err != nil || done || o2 != o || out.String() != "" {
		t.Fatalf("done %v err %v:\n%s", done, err, out.String())
	}
	for _, d := range *slept {
		if d > *so.waitTimeout {
			t.Errorf("backed off %v with a bound of %v", d, *so.waitTimeout)
		}
	}
}

func shipLeftIs(t *testing.T, d time.Duration) {
	t.Helper()
	old := shipLeft
	shipLeft = func() time.Duration { return d }
	t.Cleanup(func() { shipLeft = old })
}

// The server is asked for this task and session; the probe holds nothing and
// the later requests do.
func TestHeldShipAsksWithTheTaskAndSession(t *testing.T) {
	c, _, _, reqs := waitEnvReqs(t, waitingAnswer(), say(map[string]any{"outcome": "ended", "text": "over"}))
	if _, _, done, err := heldShip(t, c, false); err != nil || !done {
		t.Fatalf("done %v err %v", done, err)
	}
	got := reqs.all()
	if len(got) != 2 {
		t.Fatalf("%d requests", len(got))
	}
	for _, r := range got {
		if r["task"] != "WP-9" || r["session_id"] != "sess-wait" {
			t.Errorf("request %v", r)
		}
	}
	if got[0]["wait"].(float64) != 0 || got[1]["wait"].(float64) <= 0 {
		t.Errorf("waits %v, %v", got[0]["wait"], got[1]["wait"])
	}
}

// Findings already in the probe: nothing is announced, and the ids are acked.
func TestHeldShipAcksFindingsAndDoesNotAnnounceThem(t *testing.T) {
	c, out, _, reqs := waitEnvReqs(t, say(map[string]any{"outcome": "findings", "text": "fix it", "ids": []int{4, 7}}))
	if _, _, done, err := heldShip(t, c, false); err != nil || !done {
		t.Fatalf("done %v err %v", done, err)
	}
	if strings.Contains(out.String(), "waiting for") || !strings.Contains(out.String(), "fix it") {
		t.Errorf("output:\n%s", out.String())
	}
	got := reqs.all()
	ack, _ := got[len(got)-1]["ack"].([]any)
	if len(got) != 2 || len(ack) != 2 || ack[0].(float64) != 4 || ack[1].(float64) != 7 || got[1]["task"] != "WP-9" {
		t.Errorf("requests %v", got)
	}
}

// An outcome the client doesn't know stops the wait after the one call.
func TestHeldShipUnknownOutcomeStops(t *testing.T) {
	c, out, calls := waitEnv(t, say(map[string]any{"outcome": "bogus"}))
	o, _, done, err := heldShip(t, c, false)
	if err != nil || done || o.Held == "" || calls.Load() != 1 || out.Len() != 0 {
		t.Fatalf("done %v err %v after %d calls; output %q", done, err, calls.Load(), out.String())
	}
}

// "none": nothing printed, the ship ends as before.
func TestHeldShipNoneIsSilent(t *testing.T) {
	c, out, calls := waitEnv(t, say(map[string]any{"outcome": "none"}))
	o, _, done, err := heldShip(t, c, false)
	if err != nil || done || o.Held == "" || calls.Load() != 1 || out.Len() != 0 {
		t.Fatalf("done %v err %v after %d calls; output %q", done, err, calls.Load(), out.String())
	}
}

// The pull outcome whose reopen is still held returns the reopened answer,
// not the one from before the wait.
func TestHeldShipPullStillHeldReturnsTheReopenedAnswer(t *testing.T) {
	c, out, _ := waitEnv(t, say(map[string]any{"outcome": "pull", "text": "Waypoint shipped it: PR 5", "pull": map[string]any{"number": 5}}))
	reopened := &openedPR{Held: "held again"}
	old := reopenPR
	reopenPR = func(*cli, prOpenFlags) (*openedPR, []byte, error) { return reopened, []byte(`{"r":1}`), nil }
	t.Cleanup(func() { reopenPR = old })
	o, raw, done, err := heldShip(t, c, false)
	if err != nil || done || o != reopened || string(raw) != `{"r":1}` || !strings.Contains(out.String(), "Waypoint shipped it: PR 5") {
		t.Fatalf("o %+v raw %s done %v err %v:\n%s", o, raw, done, err, out.String())
	}
}

// When the ship ends on the round's news, the held answer's notes are printed
// before it.
func TestHeldShipPrintsTheHeldNotesWhenDone(t *testing.T) {
	c, out, _ := waitEnv(t, say(map[string]any{"outcome": "findings", "text": "the findings"}))
	so := shipOpts{noWait: new(bool), waitTimeout: new(time.Duration)}
	*so.waitTimeout = 5 * time.Second
	o := &openedPR{Held: "Pushed. Nothing more.", Notice: "a notice", Warning: "a warning", ClaimWarning: "a claim", UrgentPR: 12}
	if _, _, done, err := c.waitHeldShip(heldFlags(), so, o, nil, "WP-9", ""); err != nil || !done {
		t.Fatalf("done %v err %v", done, err)
	}
	s := out.String()
	for _, want := range []string{"Notice: a notice", "Warning: a warning", "Warning: a claim", "wp pr urgent 12 --off", "the findings"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "Nothing more.") || strings.Index(s, "Notice: a notice") > strings.Index(s, "the findings") {
		t.Errorf("output:\n%s", s)
	}
}

// Retries used up after the wait was announced: the ship says it lost touch.
func TestHeldShipSaysItLostTouch(t *testing.T) {
	c, out, _ := waitEnv(t, waitingAnswer(), func(w http.ResponseWriter) { http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway) })
	o, _, done, err := heldShip(t, c, false)
	if err != nil || done || o.Held == "" || !strings.Contains(out.String(), "Lost touch with Waypoint while waiting") {
		t.Fatalf("done %v err %v:\n%s", done, err, out.String())
	}
}

// What the process already spent comes off the bound.
func TestHeldShipBoundShrinksByTimeSpent(t *testing.T) {
	c, out, calls := waitEnv(t, waitingAnswer())
	shipLeftIs(t, 500*time.Millisecond)
	o, _, done, err := heldShip(t, c, false)
	if err != nil || done || o.Held == "" || calls.Load() != 1 || !strings.Contains(out.String(), "stopped waiting here") {
		t.Fatalf("done %v err %v after %d calls:\n%s", done, err, calls.Load(), out.String())
	}
}

// shipWorkHeld drives shipWork for a held ship: the first reopenPR answer is
// the held one, later ones are what the wait's "pull" outcome finds.
func shipWorkHeld(t *testing.T, c *cli, answers ...*openedPR) (other bool, err error) {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, b)
		}
	}
	t.Chdir(dir)
	var n int
	old := reopenPR
	reopenPR = func(*cli, prOpenFlags) (*openedPR, []byte, error) {
		o := answers[min(n, len(answers)-1)]
		n++
		return o, []byte(`{}`), nil
	}
	t.Cleanup(func() { reopenPR = old })
	fs := flag.NewFlagSet("ship", flag.ContinueOnError)
	f := addPROpenFlags(fs)
	so := addShipFlags(fs)
	if err := fs.Parse([]string{"--no-arm", "--no-babysit", "--no-move", "--wait-timeout=5s"}); err != nil {
		t.Fatal(err)
	}
	return c.shipWork(f, so, "", false, []string{"WP-9"}, true)
}

// The wait ends with findings: shipWork returns after printing them, without
// the held path's text or marker.
func TestShipWorkStopsWhenTheHeldWaitPrintsFindings(t *testing.T) {
	c, out, calls := waitEnv(t, waitingAnswer(), say(map[string]any{"outcome": "findings", "text": "Audit: 1 finding\nfix the nil check"}))
	held := &openedPR{Held: "HELD-TEXT end your turn", HeldNeedsAgent: new(bool)}
	if _, err := shipWorkHeld(t, c, held); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "fix the nil check") || strings.Contains(s, "HELD-TEXT") || calls.Load() != 2 {
		t.Errorf("output after %d calls:\n%s", calls.Load(), s)
	}
	c2, _ := newCLI(new(bytes.Buffer), new(bytes.Buffer))
	if e := c2.state.ShipEnds["sess-wait"]; e != nil && !e.Ready.IsZero() {
		t.Errorf("a marker was written: %+v", e)
	}
}

// The wait ends with "pull": shipWork carries on with the reopened pull
// request, not the held answer.
func TestShipWorkContinuesWithTheReopenedPullRequest(t *testing.T) {
	c, out, _ := waitEnv(t, waitingAnswer(), say(map[string]any{"outcome": "pull", "text": "Waypoint shipped it: PR 5", "pull": map[string]any{"number": 5}}))
	held := &openedPR{Held: "HELD-TEXT end your turn", HeldNeedsAgent: new(bool)}
	opened := &openedPR{Pull: &prDetail{Number: 5, Repo: "a/b", URL: "https://git.example/a/b/pulls/5"}}
	if _, err := shipWorkHeld(t, c, held, opened); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if strings.Contains(s, "HELD-TEXT") || !strings.Contains(s, "https://git.example/a/b/pulls/5") {
		t.Errorf("output:\n%s", s)
	}
}

// A held answer with a notice to act on: shipWork doesn't wait, and prints
// the notice and the held answer at once.
func TestShipWorkDoesNotWaitWithANoticeToActOn(t *testing.T) {
	c, out, calls := waitEnv(t, waitingAnswer())
	yes := true
	held := &openedPR{Held: "HELD-TEXT end your turn", HeldNeedsAgent: new(bool), Notice: "act on this", NoticeNeedsAgent: &yes}
	if _, err := shipWorkHeld(t, c, held); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); calls.Load() != 0 || !strings.Contains(s, "HELD-TEXT") || !strings.Contains(s, "Notice: act on this") || strings.Contains(s, "waiting for") {
		t.Errorf("output after %d calls:\n%s", calls.Load(), s)
	}
}

// printWatch is a stdout that fails the write holding bad, and notes whether
// the server had been acked by the time anything was written.
type printWatch struct {
	bad   string
	reqs  *waitReqs
	acked bool
}

func (w *printWatch) Write(p []byte) (int, error) {
	for _, r := range w.reqs.all() {
		if _, ack := r["ack"]; ack {
			w.acked = true
		}
	}
	if w.bad != "" && strings.Contains(string(p), w.bad) {
		return 0, errors.New("broken pipe")
	}
	return len(p), nil
}

// The findings are acked only after they were printed, and not at all when
// printing them failed: they stay queued and are typed as before.
func TestHeldShipAcksOnlyWhatItPrinted(t *testing.T) {
	findings := say(map[string]any{"outcome": "findings", "text": "fix it", "ids": []int{4}})
	acks := func(reqs *waitReqs) (n int) {
		for _, r := range reqs.all() {
			if _, ack := r["ack"]; ack {
				n++
			}
		}
		return n
	}
	c, _, _, reqs := waitEnvReqs(t, findings)
	w := &printWatch{reqs: reqs}
	c.out = w
	if _, _, done, err := heldShip(t, c, false); err != nil || !done {
		t.Fatalf("done %v err %v", done, err)
	}
	if w.acked || acks(reqs) != 1 {
		t.Errorf("acked before the print: %v; acks %d", w.acked, acks(reqs))
	}
	c, _, _, reqs = waitEnvReqs(t, findings)
	c.out = &printWatch{reqs: reqs, bad: "fix it"}
	if _, _, done, err := heldShip(t, c, false); err == nil || !done {
		t.Fatalf("done %v err %v", done, err)
	}
	if acks(reqs) != 0 {
		t.Errorf("findings that weren't printed were acked: %v", reqs.all())
	}
}

// A held answer with a notice that asks the agent to act doesn't wait: the
// notice is the agent's to act on now.
func TestHeldShipWithANoticeToActOnDoesNotWait(t *testing.T) {
	c, _, _ := waitEnv(t, waitingAnswer())
	no, yes, wait := false, true, 5*time.Second
	so := shipOpts{noWait: &no, waitTimeout: &wait}
	for name, tc := range map[string]struct {
		o    openedPR
		want bool
	}{
		"no notice":              {openedPR{Held: "h", HeldNeedsAgent: &no}, true},
		"a notice that is news":  {openedPR{Held: "h", HeldNeedsAgent: &no, Notice: "n", NoticeNeedsAgent: &no}, true},
		"a notice to act on":     {openedPR{Held: "h", HeldNeedsAgent: &no, Notice: "n", NoticeNeedsAgent: &yes}, false},
		"no notice, flag true":   {openedPR{Held: "h", HeldNeedsAgent: &no, NoticeNeedsAgent: &yes}, true},
		"an older server's note": {openedPR{Held: "h", HeldNeedsAgent: &no, Notice: "n"}, false},
	} {
		if got := c.canWaitShip(so, &tc.o); got != tc.want {
			t.Errorf("%s: canWaitShip = %v", name, got)
		}
	}
}
