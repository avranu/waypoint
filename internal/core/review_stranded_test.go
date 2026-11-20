package core

import (
	"strings"
	"testing"
	"time"
)

// A round that never ran still ends with its pull request, whether or not
// the run ended (WP-1325).

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

// A pull request that can't be opened is tried again after the back-off,
// not on every tick, and doesn't keep a later round from being opened.
func TestStrandedRoundBacksOff(t *testing.T) {
	old := strandedBatch
	strandedBatch = 1
	t.Cleanup(func() { strandedBatch = old })
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

	a.s.sweepRounds(a.ctx) // tries the first, which fails
	if n := a.opened(g); n != 0 {
		t.Fatalf("opened %d on the first sweep", n)
	}
	next, ok := a.s.strandedRetry.Load(first)
	if !ok {
		t.Fatal("no back-off recorded")
	}
	a.s.sweepRounds(a.ctx) // backing off: the later round gets the batch
	if n := a.opened(g); n != 1 {
		t.Fatalf("opened %d with the first backing off", n)
	}
	if again, _ := a.s.strandedRetry.Load(first); again != next {
		t.Errorf("the first was tried again inside its back-off: %v then %v", next, again)
	}
	a.lapse(strandedBackoff + time.Minute)
	a.s.sweepRounds(a.ctx) // after it: tried again, fails again, later back-off
	if again, _ := a.s.strandedRetry.Load(first); again == next {
		t.Errorf("the first wasn't tried again after its back-off: %v", again)
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
	a.s.sweepRounds(a.ctx)
	if n := a.opened(g); n != 1 {
		t.Fatalf("opened %d", n)
	}
}
