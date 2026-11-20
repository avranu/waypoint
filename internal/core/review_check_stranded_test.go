package core

import (
	"strings"
	"testing"
	"time"
)

// stranded is a round shipped by the session, its marks checked, the check
// disagreeing with one: the news of it can't reach the session now.
func strandedCheck(t *testing.T, offline bool) (*loopEnv, int64) {
	t.Helper()
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.mark(r1, "rejected")
	if res, err := e.ship(e.task.Key, ShipAuto); err != nil || !strings.Contains(res.Held, "checking your marks") {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	e.stopWith("Waiting for the check of my marks.")
	if offline {
		if _, err := e.s.db.W.Exec(`UPDATE runners SET last_seen_at = 0 WHERE name = 'box'`); err != nil {
			t.Fatal(err)
		}
	}
	e.endCheck(e.box, "done", "[x.1] Merge loses an update")
	e.s.Wait()
	return e, r1
}

// queuedNews counts the session's check news still waiting to be posted.
func (e *loopEnv) queuedNews() int {
	e.t.Helper()
	var n int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM session_messages WHERE session_id = ? AND step_key LIKE 'check:%' AND state = 'queued'`, e.ag.Session).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// News of a check that the first post couldn't deliver (no runner online on
// the session's machine) is posted again once it can: a round waits on it.
func TestCheckNewsPostedAgain(t *testing.T) {
	e, _ := strandedCheck(t, true)
	if n := e.told("check:"); n != 1 {
		t.Fatalf("told %d times", n)
	}
	if e.queuedNews() != 1 {
		t.Fatalf("the news wasn't left queued for the offline runner")
	}
	e.tickAll(3 * time.Minute)
	if _, err := e.s.db.W.Exec(`UPDATE runners SET last_seen_at = ? WHERE name = 'box'`, e.clockNow().UnixMilli()); err != nil { // the runner polls again
		t.Fatal(err)
	}
	e.tickAll(time.Minute)
	e.s.Wait()
	if e.queuedNews() != 0 {
		t.Fatalf("the queued news wasn't posted again")
	}
}

// The message names the task, once, and says all of what the check said.
func TestCheckNewsNamesTheTask(t *testing.T) {
	e, _ := strandedCheck(t, false)
	var body string
	if err := e.s.db.R.QueryRow(`SELECT body FROM session_messages WHERE session_id = ? AND step_key LIKE 'check:%'`, e.ag.Session).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, e.task.Key) {
		t.Errorf("the message doesn't name %s:\n%s", e.task.Key, body)
	}
	if strings.Count(body, "Waypoint's check") != 1 || strings.Contains(body, "Waypoint's Waypoint") {
		t.Errorf("head says Waypoint more than once:\n%s", body)
	}
}

// A check the session never takes up doesn't strand the round for good:
// after heldScreenWait Waypoint opens the pull request, with the check's
// findings carried to the review gate as undecided, and tells the person.
func TestCheckNeverTakenUpOpensThePR(t *testing.T) {
	e, r1 := strandedCheck(t, true)
	e.tickAll(heldScreenWait - 10*time.Minute)
	e.s.Wait()
	if len(e.g.created) != 0 {
		t.Fatalf("opened %d before the deadline", len(e.g.created))
	}
	e.tickAll(20 * time.Minute)
	e.s.Wait()
	if len(e.g.created) != 1 {
		t.Fatalf("opened %d pull requests; round %s", len(e.g.created), e.round(r1).state)
	}
	if n := e.lastNotice(); !strings.Contains(n, e.task.Key) || !strings.Contains(n, "check") {
		t.Errorf("the person's notice doesn't name the task and the check: %s", n)
	}
	var carried int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM code_audits WHERE request_key = ? AND purpose = '' AND pr_id IS NOT NULL AND pr_id != 0`, "review-round:"+itoa64(r1)+":check").Scan(&carried); err != nil {
		t.Fatal(err)
	}
	if carried != 1 {
		t.Errorf("the check wasn't carried to the pull request's gate")
	}
	if got := e.told("check:"); got > 1 {
		t.Errorf("told %d times", got)
	}
}

// A session that ended while its check ran doesn't have its round
// interrupted by the sweep: the round waits on Waypoint, which opens its
// pull request at the deadline.
func TestCheckSettlesAfterTheSessionEnded(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.mark(r1, "rejected")
	if res, err := e.ship(e.task.Key, ShipAuto); err != nil || !strings.Contains(res.Held, "checking your marks") {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	e.stopWith("Waiting for the check of my marks.")
	if _, err := e.s.db.W.Exec(`UPDATE agent_sessions SET ended_at = ? WHERE id = ?`, e.clockNow().UnixMilli(), e.session); err != nil {
		t.Fatal(err)
	}
	e.tickAll(time.Minute)
	if r := e.round(r1); r.state != RoundRunning {
		t.Fatalf("the sweep ended a round whose check is running: %s (%s)", r.state, e.roundReason(r1))
	}
	e.endCheck(e.box, "done", "[x.1] Merge loses an update")
	e.s.Wait()
	if n := e.told("check:"); n != 0 {
		t.Errorf("an ended session was told %d times", n)
	}
	e.tickAll(heldScreenWait + 10*time.Minute)
	e.s.Wait()
	if len(e.g.created) != 1 {
		t.Fatalf("opened %d; round %s (%s)", len(e.g.created), e.round(r1).state, e.roundReason(r1))
	}
}

// A check of the marks that failed or was cancelled didn't check them: the
// first round's rule asks for another review rather than opening the pull
// request, and says why; the reason a ship that does open one gives is
// "the check of the marks didn't run" (checkDidntRun), never "nothing left
// for you to do".
func TestCheckDidntRunIsSaidSo(t *testing.T) {
	for _, state := range []string{"failed", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
			r1 := e.review("Merge loses an update")
			e.mark(r1, "rejected")
			if res, err := e.ship(e.task.Key, ShipAuto); err != nil || !strings.Contains(res.Held, "checking your marks") {
				t.Fatalf("ship = %+v, %v", res, err)
			}
			e.stopWith("Waiting for the check of my marks.")
			e.endCheck(e.box, state)
			e.s.Wait()
			if got := e.s.checkDidntRun(e.ctx, r1); !strings.HasPrefix(got, "the check of the marks didn't run: ") {
				t.Errorf("checkDidntRun = %q", got)
			}
			if len(e.g.created) != 0 {
				t.Errorf("opened %d pull requests around the unchecked marks", len(e.g.created))
			}
		})
	}
}

// What a round ran is told from its recorded facts in one function, whatever
// the release (WP-1370 R4): the audit, and the check of the marks as it
// stands.
func TestRoundFactsTable(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(e *loopEnv, r1 int64)
		want       []string
		wantRan    bool
		wantAbsent []string
	}{
		{"no check asked", func(e *loopEnv, r1 int64) {}, []string{"its audit found 1 thing, 1 undecided"}, true, []string{"check of its marks"}},
		{"check queued", func(e *loopEnv, r1 int64) {
			e.mark(r1, "rejected")
			if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
				t.Fatal(err)
			}
		}, []string{"the check of its marks hadn't finished"}, true, []string{"agreed", "disagreed"}},
		{"check failed", func(e *loopEnv, r1 int64) {
			e.mark(r1, "rejected")
			if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
				t.Fatal(err)
			}
			e.stopWith("Waiting.")
			e.endCheck(e.box, "failed")
		}, []string{"the check of its marks didn't run (failed)"}, true, []string{"agreed", "disagreed"}},
		{"check disagreed", func(e *loopEnv, r1 int64) {
			e.mark(r1, "rejected")
			if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
				t.Fatal(err)
			}
			e.stopWith("Waiting.")
			e.endCheck(e.box, "done", "[x.1] Merge loses an update")
		}, []string{"the check of its marks disagreed with 1 mark"}, true, nil},
		{"check agreed", func(e *loopEnv, r1 int64) {
			e.mark(r1, "rejected")
			if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
				t.Fatal(err)
			}
			e.stopWith("Waiting.")
			e.endCheck(e.box, "done")
		}, []string{"the check of its marks agreed"}, true, []string{"disagreed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
			r1 := e.review("Merge loses an update")
			c.setup(e, r1)
			e.s.Wait()
			facts, ran := e.s.roundFacts(e.ctx, r1)
			if ran != c.wantRan {
				t.Errorf("ran = %v, want %v (%q)", ran, c.wantRan, facts)
			}
			for _, w := range c.want {
				if !strings.Contains(facts, w) {
					t.Errorf("facts %q lack %q", facts, w)
				}
			}
			for _, w := range c.wantAbsent {
				if strings.Contains(facts, w) {
					t.Errorf("facts %q contain %q", facts, w)
				}
			}
		})
	}
}
