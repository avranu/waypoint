package core

import (
	"strings"
	"testing"
	"time"
)

// prCount is how many pull requests the forge was asked to open.
func prsOpened(g *shipForge) int { return len(g.created) }

// A round before its pull request, in a session that always reads as
// working (a person's prompt that never ends), has a deadline: Waypoint opens
// the pull request and says what was not reviewed (WP-1370).
func TestRoundBeforePRHasADeadlineWhileSessionWorks(t *testing.T) {
	a, g, _ := heldEnv(t)
	a.stopWith("Waiting for the review.")
	a.prompt(HarnessPromptMarks[0] + " a task finished")
	answered := 0
	for range int(heldScreenWait/(5*time.Minute)) + 3 {
		a.tickAll(5 * time.Minute)
		for ; answered < len(a.jobs); answered++ {
			a.jobDone(answered+1, AutopilotJobResult{Outcome: "held", Reason: WaitScreen, Detail: "the session is working"})
		}
		a.s.Wait()
	}
	a.s.Wait()
	if prsOpened(g) != 1 {
		t.Fatalf("after the deadline opened %d pull requests; step %+v", prsOpened(g), a.step())
	}
	id := a.heldRound()
	if r := a.round(id); r.state != RoundFailed || !strings.Contains(a.roundReason(id), "hours") {
		t.Errorf("round = %s: %q", r.state, a.roundReason(id))
	}
	if n := a.lastNotice(); !strings.Contains(n, "review") {
		t.Errorf("notice doesn't say what was not reviewed: %s", n)
	}
}

// The same with a person's prompt never ending after the Stop's own wait:
// the "prompted" wait has its end too.
func TestRoundBeforePRDeadlineWithFindings(t *testing.T) {
	a, box, g, _ := shippedWithBox(t)
	a.stopWith("Waiting for the review.")
	round := a.heldRound()
	a.tickAll(61 * time.Second)
	a.endAudit(box, "done", "Off by one", "Missing test")
	a.prompt("one more thing")
	a.tickAll(heldScreenWait + 10*time.Minute)
	a.s.Wait()
	a.s.sweepRounds(a.ctx)
	a.s.Wait()
	if prsOpened(g) != 1 {
		t.Fatalf("opened %d pull requests; step %+v round %s", prsOpened(g), a.step(), a.roundReason(round))
	}
	if why := a.roundReason(round); !strings.Contains(why, "2 findings") && !strings.Contains(why, "2 hours") {
		t.Errorf("reason says nothing true: %q", why)
	}
	ra, au := a.roundAuditRow(round)
	if au == nil || au.Purpose != "" || ra.id == 0 {
		t.Fatalf("round audit = %+v %+v", ra, au)
	}
	var pr int64
	_ = a.s.db.R.QueryRow(`SELECT coalesce(pr_id, 0) FROM code_audits WHERE id = ?`, ra.id).Scan(&pr)
	if pr == 0 {
		t.Error("the round's audit wasn't handed to the pull request")
	}
}

// After the pull request, a round with no runner online ends too.
func TestRoundAfterPRNoRunnerEnds(t *testing.T) {
	a, box := auditAPEnv(t)
	round, _, _ := handoffStart(t, a)
	a.endAudit(box, "done")
	if _, err := a.s.db.W.Exec(`DELETE FROM runners WHERE name = 'box'`); err != nil {
		t.Fatal(err)
	}
	a.tickAll(heldNoRunnerWait + 2*time.Minute)
	if r := a.round(round); r.state != RoundFailed || !strings.Contains(a.roundReason(round), "no runner") {
		t.Fatalf("round = %s: %q; step %+v", r.state, a.roundReason(round), a.step())
	}
}

// A runner that reports the screen held, again and again, ends a round after the
// pull request, too.
func TestRoundAfterPRHeldScreenEnds(t *testing.T) {
	a, box := auditAPEnv(t)
	round, _, _ := handoffStart(t, a)
	a.endAudit(box, "done", "Off by one")
	a.tickAll(time.Minute)
	if len(a.jobs) == 0 {
		t.Fatalf("no review job: %+v", a.step())
	}
	a.s.Wait()
	a.setClock(a.clockNow().Add(heldScreenWait + time.Minute))
	if r := a.round(round); r.state == RoundFailed {
		t.Fatalf("the round ended before the runner answered: %q", a.roundReason(round))
	}
	a.jobDone(len(a.jobs), AutopilotJobResult{Outcome: "held", Reason: WaitScreen, Detail: "a dialog is open"})
	a.s.Wait()
	if r := a.round(round); r.state != RoundFailed {
		t.Fatalf("round = %s: %q", r.state, a.roundReason(round))
	}
}
