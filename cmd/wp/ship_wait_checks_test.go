package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// checksEnv is a ship that opened pull request 5 (id 9) and babysits it, and a
// server answering the wait for its checks with each of answers in turn (the
// last repeats). Everything else the ship calls is answered {}.
type checksEnv struct {
	c    *cli
	out  *bytes.Buffer
	mu   sync.Mutex
	reqs []map[string]any
}

func (e *checksEnv) waits() (n int, acked []any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.reqs {
		if a, ok := r["ack"]; ok {
			acked = append(acked, a)
		} else if _, ok := r["wait"]; ok {
			n++
		}
	}
	return n, acked
}

func newChecksEnv(t *testing.T, answers ...func(w http.ResponseWriter)) *checksEnv {
	t.Helper()
	waitEnvReqs(t, waitingAnswer()) // the environment variables and the fast clock
	e := &checksEnv{}
	var n int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions/hook/ship-news" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		e.mu.Lock()
		e.reqs = append(e.reqs, body)
		e.mu.Unlock()
		if _, ack := body["ack"]; ack {
			_ = json.NewEncoder(w).Encode(map[string]any{"outcome": "acked"})
			return
		}
		e.mu.Lock()
		i := min(n, len(answers)-1)
		n++
		e.mu.Unlock()
		answers[i](w)
	}))
	t.Cleanup(ts.Close)
	t.Setenv("WAYPOINT_SERVER", ts.URL)
	e.out = new(bytes.Buffer)
	c, err := newCLI(e.out, e.out)
	if err != nil {
		t.Fatal(err)
	}
	e.c = c
	return e
}

func (e *checksEnv) ship(t *testing.T, args ...string) error {
	t.Helper()
	dir := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"config", "user.email", "t@t"}, {"config", "user.name", "t"}} {
		cmd := exec.Command("git", a...)
		cmd.Dir = dir
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", a, err, b)
		}
	}
	t.Chdir(dir)
	old := reopenPR
	reopenPR = func(*cli, prOpenFlags) (*openedPR, []byte, error) {
		return &openedPR{key: "WP-9", Pull: &prDetail{ID: 9, Number: 5, Repo: "a/b", URL: "https://git.example/a/b/pulls/5", State: "open"}}, []byte(`{}`), nil
	}
	t.Cleanup(func() { reopenPR = old })
	fs := flag.NewFlagSet("ship", flag.ContinueOnError)
	f := addPROpenFlags(fs)
	so := addShipFlags(fs)
	if err := fs.Parse(append([]string{"--no-arm", "--no-move"}, args...)); err != nil {
		t.Fatal(err)
	}
	fs.Visit(func(fl *flag.Flag) { so.summaryGiven = so.summaryGiven || fl.Name == "summary" })
	_, err := e.c.shipWork(f, so, "", false, []string{"WP-9"}, true)
	return err
}

func checksAnswer(outcome, text string, ids ...int64) func(w http.ResponseWriter) {
	return say(map[string]any{"outcome": outcome, "text": text, "ids": ids, "checks": true})
}

// After "opened", the wait goes on: the opened line comes at once, a failure
// is printed as findings are (acked, ship over, no marker), and the wait named
// the pull request.
func TestShipWaitsOnChecksAfterOpened(t *testing.T) {
	e := newChecksEnv(t, checksAnswer("waiting", "a/b#5: checks running"), checksAnswer("checks_failed", "a/b#5 failed its checks\n  wp pr log 5 --check \"ci / test\"", 41))
	if err := e.ship(t, "--wait-timeout=5s"); err != nil {
		t.Fatal(err)
	}
	s := e.out.String()
	opened, waiting, failed := strings.Index(s, "PR 5"), strings.Index(s, "Waiting for PR 5's checks"), strings.Index(s, "a/b#5 failed its checks")
	if opened < 0 || waiting < opened || failed < waiting {
		t.Fatalf("output:\n%s", s)
	}
	if n, acked := e.waits(); n != 2 || len(acked) != 1 {
		t.Errorf("%d waits, acks %v", n, acked)
	}
	if r := e.reqs[0]; r["pull_id"] != float64(9) || r["task"] != "WP-9" {
		t.Errorf("request %v", r)
	}
	if strings.Contains(s, "ends the turn") {
		t.Errorf("a marker was announced after a failure:\n%s", s)
	}
}

// Checks that pass are printed and the ship ends as it always did.
func TestShipChecksPassedEndsAsBefore(t *testing.T) {
	e := newChecksEnv(t, checksAnswer("waiting", "running"), checksAnswer("checks_passed", "a/b#5: its checks passed. It is ready and armed."))
	if err := e.ship(t, "--wait-timeout=5s"); err != nil {
		t.Fatal(err)
	}
	s := e.out.String()
	opened, passed := strings.Index(s, "PR 5"), strings.Index(s, "a/b#5: its checks passed. It is ready and armed.")
	if opened < 0 || passed < opened {
		t.Fatalf("the passed line must name PR 5 and come after the opened line:\n%s", s)
	}
	if _, acked := e.waits(); len(acked) != 0 {
		t.Errorf("acked %v", acked)
	}
}

// A server that doesn't know the wait answers as the review wait does, with no
// "checks": the ship stops at "opened" quietly, as it did.
func TestShipChecksAgainstAnOlderServer(t *testing.T) {
	for name, answer := range map[string]func(w http.ResponseWriter){
		"none":    say(map[string]any{"outcome": "none"}),
		"waiting": waitingAnswer(),
		"404":     func(w http.ResponseWriter) { http.Error(w, "404 page not found", http.StatusNotFound) },
		// What the real older server answers: it decodes the request strictly and
		// refuses the field it doesn't know.
		"400 unknown pull_id": func(w http.ResponseWriter) {
			http.Error(w, `{"error":"json: unknown field \"pull_id\""}`, http.StatusBadRequest)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newChecksEnv(t, answer)
			if err := e.ship(t, "--wait-timeout=5s"); err != nil {
				t.Fatal(err)
			}
			if n, _ := e.waits(); n != 1 || strings.Contains(e.out.String(), "Waiting for") || strings.Contains(e.out.String(), "Stopped waiting") {
				t.Fatalf("%d waits; output:\n%s", n, e.out.String())
			}
		})
	}
}

// A request that fails before the wait was announced is said, not silent.
func TestShipChecksFailedRequestIsSaid(t *testing.T) {
	e := newChecksEnv(t, func(w http.ResponseWriter) {
		http.Error(w, `{"error":"you may not see this pull request"}`, http.StatusForbidden)
	})
	if err := e.ship(t, "--wait-timeout=5s"); err != nil {
		t.Fatal(err)
	}
	if s := e.out.String(); !strings.Contains(s, "Couldn't ask Waypoint to wait for PR 5's checks (you may not see this pull request)") || strings.Contains(s, "Waiting for PR") {
		t.Fatalf("output:\n%s", s)
	}
}

// At the limit, what is still running is said, and babysitting takes over.
func TestShipChecksStopsAtTheLimit(t *testing.T) {
	e := newChecksEnv(t, checksAnswer("waiting", "a/b#5: 2 checks still running"))
	if err := e.ship(t, "--wait-timeout=1500ms"); err != nil {
		t.Fatal(err)
	}
	if s := e.out.String(); !strings.Contains(s, "a/b#5: 2 checks still running Stopped waiting here; "+checksAfterWait) {
		t.Fatalf("output:\n%s", s)
	}
}

// --no-wait and a ship nobody babysits ask nothing.
func TestShipChecksNotAskedWithoutAWait(t *testing.T) {
	for _, args := range [][]string{{"--no-wait"}, {"--no-babysit"}} {
		e := newChecksEnv(t, checksAnswer("checks_failed", "x", 1))
		if err := e.ship(t, args...); err != nil {
			t.Fatal(err)
		}
		if n, _ := e.waits(); n != 0 {
			t.Errorf("%v: %d waits", args, n)
		}
	}
}

// The wait's bound is the time left of the ship's, which stays inside what the
// background call counts (shipBgFresh): it is what shipLeft says.
func TestShipChecksBoundIsWhatIsLeft(t *testing.T) {
	e := newChecksEnv(t, checksAnswer("waiting", "still running"))
	shipLeftIs(t, 1500*time.Millisecond)
	if err := e.ship(t, "--wait-timeout=5h"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.out.String(), "Stopped waiting here") {
		t.Fatalf("output:\n%s", e.out.String())
	}
}

// A wait that ends on failed checks keeps the --summary for the next wp ship,
// as the other ways out of a ship do.
func TestShipChecksFailureKeepsTheSummary(t *testing.T) {
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	e := newChecksEnv(t, checksAnswer("checks_failed", "a/b#5 failed its checks", 41))
	if err := e.ship(t, "--wait-timeout=5s", "--summary", "Rows export now."); err != nil {
		t.Fatal(err)
	}
	if got := e.c.keptSummaryOf("WP-9"); got != "Rows export now." {
		t.Fatalf("kept summary %q\n%s", got, e.out.String())
	}
}

// The checks wait is asked only when nothing else wants the agent's turn
// (why), the ship isn't --json, and the pull request is babysat and waited on.
func TestCanWaitChecks(t *testing.T) {
	e := newChecksEnv(t, checksAnswer("waiting", "running"))
	fs := flag.NewFlagSet("ship", flag.ContinueOnError)
	so := addShipFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if !e.c.canWaitChecks(so, nil, true, "") {
		t.Fatal("the plain agent ship does not wait")
	}
	if e.c.canWaitChecks(so, nil, true, "there is a warning") {
		t.Error("waits with something that needs the agent's turn")
	}
	if e.c.canWaitChecks(so, nil, false, "") {
		t.Error("waits on a pull request nobody babysits")
	}
	e.c.asJSON = true
	if e.c.canWaitChecks(so, nil, true, "") {
		t.Error("waits under --json")
	}
}

// One --wait-timeout bounds the whole ship: the checks wait gets what the
// review wait left.
func TestShipWaitLeftIsOneDeadline(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	old := shipNow
	t.Cleanup(func() { shipNow = old })
	shipNow = func() time.Time { return now }
	shipLeftIs(t, time.Hour)
	d := 30 * time.Minute
	so := shipOpts{waitTimeout: &d}
	so.deadline = shipNow().Add(d)
	if got := so.waitLeft(); got != 30*time.Minute {
		t.Fatalf("at the start: %v", got)
	}
	now = now.Add(20 * time.Minute) // the review wait ran 20 minutes
	if got := so.waitLeft(); got != 10*time.Minute {
		t.Errorf("after the review wait: %v, want what is left of 30m", got)
	}
	now = now.Add(time.Hour)
	if got := so.waitLeft(); got > 0 {
		t.Errorf("past the deadline: %v", got)
	}
	// shipLeft still caps it.
	now = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	shipLeftIs(t, 5*time.Minute)
	if got := so.waitLeft(); got != 5*time.Minute {
		t.Errorf("capped by shipLeft: %v", got)
	}
	// A ship whose deadline was never taken waits as the flag says.
	if got := (shipOpts{waitTimeout: &d}).waitLeft(); got != 5*time.Minute {
		t.Errorf("no deadline: %v", got)
	}
}
