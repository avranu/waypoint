package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// waitEnv is an agent's session whose ship was held, and a server answering
// the wait with each of answers in turn (the last repeats).
func waitEnv(t *testing.T, answers ...func(w http.ResponseWriter)) (*cli, *bytes.Buffer, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions/hook/ship-news" {
			http.NotFound(w, r)
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
	oldSleep, oldChunk := shipSleep, shipChunk
	shipSleep, shipChunk = func(time.Duration) {}, 2*time.Second
	t.Cleanup(func() { shipSleep, shipChunk = oldSleep, oldChunk })
	var out bytes.Buffer
	c, err := newCLI(&out, &out)
	if err != nil {
		t.Fatal(err)
	}
	return c, &out, &calls
}

func say(v any) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) { _ = json.NewEncoder(w).Encode(v) }
}

func waitingAnswer() func(w http.ResponseWriter) { return say(map[string]any{"outcome": "waiting"}) }

func heldFlags() prOpenFlags { return prOpenFlags{review: new(bool), force: new(bool)} }

func heldShip(t *testing.T, c *cli, noWait bool) (*openedPR, []byte, bool, error) {
	t.Helper()
	wait := 5 * time.Second
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
