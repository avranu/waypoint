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
	_, err := e.c.shipWork(f, so, "", false, "WP-9", true)
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
	if !strings.Contains(s, "its checks passed. It is ready and armed.") || !strings.Contains(s, "5") {
		t.Fatalf("output:\n%s", s)
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

// At the limit, what is still running is said, and babysitting takes over.
func TestShipChecksStopsAtTheLimit(t *testing.T) {
	e := newChecksEnv(t, checksAnswer("waiting", "a/b#5: 2 checks still running"))
	if err := e.ship(t, "--wait-timeout=1500ms"); err != nil {
		t.Fatal(err)
	}
	if s := e.out.String(); !strings.Contains(s, "a/b#5: 2 checks still running Stopped waiting here; Waypoint's babysitting takes any failure from here.") {
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
