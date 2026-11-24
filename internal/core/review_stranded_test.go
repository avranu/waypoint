package core

import (
	"strings"
	"testing"
	"time"
)

// A round that never ran still ends with its pull request, once its run or
// session is over or the session has moved on to another task (WP-1325).

// strandedEnv is a task shipped and held for its review, then left with no
// autopilot step and its run ended, as a session's end or a person's /clear
// leaves it.
func strandedEnv(t *testing.T) (*apEnv, *shipForge, int64) {
	t.Helper()
	a, g, _ := heldEnv(t)
	a.stopWith("Waiting for the review.")
	id := a.heldRound()
	if _, err := a.s.db.W.Exec(`DELETE FROM autopilot_steps`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.W.Exec(`UPDATE agent_runs SET ended_at = ?, outcome = 'released' WHERE ended_at IS NULL`, a.clockNow().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	return a, g, id
}

func (a *apEnv) opened(g *shipForge) int {
	created, _, _, _ := g.snapshot()
	return len(created)
}

func TestStrandedRoundOpensItsPR(t *testing.T) {
	for _, state := range []string{RoundPending, RoundSkipped, RoundFailed, RoundInterrupted} {
		t.Run(state, func(t *testing.T) {
			a, g, id := strandedEnv(t)
			if state != RoundPending {
				if _, err := a.s.db.W.Exec(`UPDATE work_log SET state = ?, reason = 'the runner went away', ended_at = ? WHERE id = ?`, state, a.clockNow().UnixMilli(), id); err != nil {
					t.Fatal(err)
				}
			}
			a.setClock(a.clockNow().Add(heldStopLost + time.Minute))
			a.s.sweepRounds(a.ctx) // a pending round ends first (decideRound: doEnd)
			a.s.sweepRounds(a.ctx)
			created, _, _, _ := g.snapshot()
			if len(created) != 1 || !strings.Contains(created[0].Body, "without an adversarial review") || !strings.Contains(created[0].Body, "What changed.") {
				t.Fatalf("created = %+v", created)
			}
			a.s.sweepRounds(a.ctx)
			if n := a.opened(g); n != 1 {
				t.Fatalf("opened again: %d", n)
			}
			if r := a.round(id); r.state == RoundDone || r.state == RoundRunning {
				t.Errorf("round = %+v", r)
			}
		})
	}
}

func TestStrandedRoundLeftAlone(t *testing.T) {
	for name, reason := range map[string]string{"cancelled by a person": "Cancelled by Robin.", "replaced": roundReplaced} {
		t.Run(name, func(t *testing.T) {
			a, g, id := strandedEnv(t)
			if _, err := a.s.db.W.Exec(`UPDATE work_log SET state = ?, reason = ?, ended_at = ? WHERE id = ?`, RoundFailed, reason, a.clockNow().UnixMilli(), id); err != nil {
				t.Fatal(err)
			}
			a.s.sweepRounds(a.ctx)
			if n := a.opened(g); n != 0 {
				t.Fatalf("opened %d", n)
			}
		})
	}
	t.Run("the session shipped it", func(t *testing.T) {
		a, g, id := strandedEnv(t)
		if _, err := a.s.db.W.Exec(`UPDATE work_log SET state = ?, reason = 'no report came back', ended_at = ? WHERE id = ?`, RoundFailed, a.clockNow().UnixMilli(), id); err != nil {
			t.Fatal(err)
		}
		var issue int64
		_ = a.s.db.R.QueryRow(`SELECT issue_id FROM work_log WHERE id = ?`, id).Scan(&issue)
		a.linkPR(issue, 7, "abc")
		a.s.sweepRounds(a.ctx)
		if n := a.opened(g); n != 0 {
			t.Fatalf("opened %d", n)
		}
	})
	// A resolved task needs no pull request, and a task with a newer review
	// round is that round's: it is pending, or its own end decides.
	for name, change := range map[string]string{
		"the task is resolved": `UPDATE issues SET resolved_at = ?2 WHERE id = (SELECT issue_id FROM work_log WHERE id = ?1)`,
		"a newer round of the task": `INSERT INTO work_log(kind, session_id, run_id, issue_id, state, prompt, created_at, updated_at)
			SELECT 'review', session_id, run_id, issue_id, 'pending', '', ?2, ?2 FROM work_log WHERE id = ?1`,
	} {
		t.Run(name, func(t *testing.T) {
			a, g, id := strandedEnv(t)
			now := a.clockNow().UnixMilli()
			if _, err := a.s.db.W.Exec(`UPDATE work_log SET state = ?, reason = 'no report came back', ended_at = ? WHERE id = ?`, RoundFailed, now, id); err != nil {
				t.Fatal(err)
			}
			if _, err := a.s.db.W.Exec(change, id, now); err != nil {
				t.Fatal(err)
			}
			a.s.sweepRounds(a.ctx)
			if n := a.opened(g); n != 0 {
				t.Fatalf("opened %d", n)
			}
		})
	}
}

// Another session took the task since: its run isn't ended with the old
// branch's pull request opened under it (WP-1325).
func TestStrandedRoundLeavesATaskAnotherSessionTook(t *testing.T) {
	a, g, id := strandedEnv(t)
	if _, err := a.s.db.W.Exec(`UPDATE work_log SET state = ?, reason = 'no report came back', ended_at = ? WHERE id = ?`, RoundFailed, a.clockNow().UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
	other := a.otherSession()
	if _, err := a.s.AgentTake(a.ctx, &other, "APP-1", ""); err != nil {
		t.Fatal(err)
	}
	a.s.sweepRounds(a.ctx)
	if n := a.opened(g); n != 0 {
		t.Fatalf("opened %d under the other session's run", n)
	}
}

// A round with no reason recorded is a round like any other (the column is
// NOT NULL, so the coalesce in the sweep's query is only a guard).
func TestStrandedRoundWithNoReason(t *testing.T) {
	a, g, id := strandedEnv(t)
	if _, err := a.s.db.W.Exec(`UPDATE work_log SET state = ?, reason = '', ended_at = ? WHERE id = ?`, RoundFailed, a.clockNow().UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
	a.s.sweepRounds(a.ctx)
	if n := a.opened(g); n != 1 {
		t.Fatalf("opened %d", n)
	}
}

// A pull request that can't be opened is tried again after the back-off the
// round's ship tries keep (shipTried), not on every tick, and doesn't keep a
// later round from being opened.
func TestStrandedRoundBacksOff(t *testing.T) {
	a, g, first := strandedEnv(t)
	if _, err := a.s.db.W.Exec(`UPDATE work_log SET state = ?, reason = 'no report came back', ended_at = ?, before_pr = ? WHERE id = ?`,
		RoundFailed, a.clockNow().UnixMilli(), `{"project":"APP","repo":"acme/missing","branch":"gone"}`, first); err != nil {
		t.Fatal(err)
	}
	// A later task, stranded the same way.
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, second.Key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ship(second.Key, ShipAuto, func(in *OpenPRInput) { in.Head = "wp-2-second"; in.Title = "Second" }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.W.Exec(`DELETE FROM autopilot_steps`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.W.Exec(`UPDATE agent_runs SET ended_at = ?, outcome = 'released' WHERE ended_at IS NULL`, a.clockNow().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	a.lapse(heldStopLost + time.Minute)

	a.s.sweepRounds(a.ctx) // the later round, pending, ends first
	a.s.sweepRounds(a.ctx) // then it opens; the first fails
	if n := a.opened(g); n != 1 {
		t.Fatalf("opened %d on the first sweep", n)
	}
	tries := func() (int, int64) {
		bp := roundBeforePR(a.ctx, a.s.db.R, first)
		return bp.ShipTries, bp.ShipNext
	}
	n1, next := tries()
	if n1 != 1 || next <= a.clockNow().UnixMilli() {
		t.Fatalf("no back-off recorded: %d tries, next %d", n1, next)
	}
	a.s.sweepRounds(a.ctx) // backing off: not tried again
	if n2, again := tries(); n2 != 1 || again != next {
		t.Errorf("tried again inside its back-off: %d tries, next %d then %d", n2, next, again)
	}
	a.lapse(shipBackoff(1) + time.Minute)
	a.s.sweepRounds(a.ctx) // after it: tried again
	if n3, _ := tries(); n3 != 2 {
		t.Errorf("not tried again after its back-off: %d tries", n3)
	}
}

// The session moved on to another task: once that is older than
// heldStopLost its round is ended and its pull request opened; before that
// the round is left to the session's own Stop.
func TestStrandedRoundOfASessionThatMovedOn(t *testing.T) {
	a, g, _ := heldEnv(t)
	a.stopWith("Waiting for the review.")
	id := a.heldRound()
	if _, err := a.s.db.W.Exec(`DELETE FROM autopilot_steps`); err != nil {
		t.Fatal(err)
	}
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, second.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.s.sweepRounds(a.ctx)
	if r := a.round(id); r.state != RoundPending || a.opened(g) != 0 {
		t.Fatalf("round = %+v, opened %d", r, a.opened(g))
	}
	a.lapse(heldStopLost + time.Minute)
	a.s.sweepRounds(a.ctx) // the pending round ends first
	a.s.sweepRounds(a.ctx)
	if n := a.opened(g); n != 1 {
		t.Fatalf("opened %d", n)
	}
}

// The window runs from the latest of the round's end and its session's: a
// round that ended long ago whose session only just ended is still opened;
// when both are over the window it is left (WP-1325).
func TestStrandedRoundWindow(t *testing.T) {
	for name, tc := range map[string]struct {
		roundAgo, runAgo time.Duration
		want             int
	}{
		"round old, session just ended": {30 * time.Hour, time.Hour, 1},
		"both over the window":          {30 * time.Hour, 26 * time.Hour, 0},
	} {
		t.Run(name, func(t *testing.T) {
			a, g, id := strandedEnv(t)
			now := a.clockNow()
			if _, err := a.s.db.W.Exec(`UPDATE work_log SET state = ?, reason = 'no report came back', started_at = ?, ended_at = ?, updated_at = ? WHERE id = ?`,
				RoundFailed, now.Add(-tc.roundAgo).UnixMilli(), now.Add(-tc.roundAgo).UnixMilli(), now.Add(-tc.roundAgo).UnixMilli(), id); err != nil {
				t.Fatal(err)
			}
			if _, err := a.s.db.W.Exec(`UPDATE agent_runs SET ended_at = ?`, now.Add(-tc.runAgo).UnixMilli()); err != nil {
				t.Fatal(err)
			}
			a.s.sweepRounds(a.ctx)
			if n := a.opened(g); n != tc.want {
				t.Fatalf("opened %d, want %d", n, tc.want)
			}
		})
	}
}

// A round that was restarted for more commits, like one that was replaced,
// has a newer round that carries the review on: its pull request isn't
// opened without one (WP-1325).
func TestStrandedRoundLeftAloneWhenRestarted(t *testing.T) {
	a, g, id := strandedEnv(t)
	if _, err := a.s.db.W.Exec(`UPDATE work_log SET state = ?, reason = ?, ended_at = ? WHERE id = ?`, RoundInterrupted, roundRestarted, a.clockNow().UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
	a.lapse(heldStopLost + time.Minute)
	a.s.sweepRounds(a.ctx)
	if n := a.opened(g); n != 0 {
		t.Fatalf("opened %d", n)
	}
}

// A round that ended failed or interrupted, whose session moved on to another
// task (its run not ended), has its pull request opened: a round in an end
// state has nothing left to wait for, so no age guard applies.
func TestStrandedEndedRoundOfASessionThatMovedOn(t *testing.T) {
	for _, state := range []string{RoundFailed, RoundInterrupted} {
		t.Run(state, func(t *testing.T) {
			a, g, _ := heldEnv(t)
			a.stopWith("Waiting for the review.")
			if _, err := a.s.db.W.Exec(`DELETE FROM autopilot_steps`); err != nil {
				t.Fatal(err)
			}
			if _, err := a.s.db.W.Exec(`UPDATE work_log SET state = ?, reason = 'the runner went away', ended_at = ? WHERE id = ?`, state, a.clockNow().UnixMilli(), a.heldRound()); err != nil {
				t.Fatal(err)
			}
			second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
			if _, err := a.s.AgentTake(a.ctx, a.ag, second.Key, ""); err != nil {
				t.Fatal(err)
			}
			a.s.sweepRounds(a.ctx)
			if n := a.opened(g); n != 1 {
				t.Fatalf("opened %d", n)
			}
		})
	}
}

// Rounds the agent replaced, or that started over for a newer head, don't
// count towards reviewInterruptsMax; others interrupted do.
func TestAskAgainAfterInterruptSkipsReplacedAndRestarted(t *testing.T) {
	a, _, _ := heldEnv(t)
	var session, issue int64
	if err := a.s.db.R.QueryRow(`SELECT session_id, issue_id FROM work_log WHERE kind = 'review' ORDER BY id DESC LIMIT 1`).Scan(&session, &issue); err != nil {
		t.Fatal(err)
	}
	add := func(reason string, n int) {
		t.Helper()
		for range n {
			if _, err := a.s.db.W.Exec(`INSERT INTO work_log(issue_id, kind, state, body, reason, created_at, updated_at, session_id) VALUES (?, 'review', 'interrupted', '', ?, 1, 1, ?)`, issue, reason, session); err != nil {
				t.Fatal(err)
			}
		}
	}
	again := func() bool {
		t.Helper()
		ok, err := askAgainAfterInterrupt(a.ctx, a.s.db.R, session, issue)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	add(roundReplaced, reviewInterruptsMax)
	add(roundRestarted, reviewInterruptsMax)
	if !again() {
		t.Fatal("replaced and restarted rounds counted as interruptions")
	}
	add("a person's prompt", reviewInterruptsMax)
	if again() {
		t.Fatal("interruptions past the max didn't stop asking")
	}
}
