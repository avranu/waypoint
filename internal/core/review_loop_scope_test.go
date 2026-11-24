package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A loop is the change's: the rounds of the task's branch since the last
// decision that opened a pull request, through runs that only stopped
// short (WP-1320's audit). Counted by run, a round after a pause was round
// 1 again, judged for repeats against nothing and never near the cap.
func TestAgainLoopOf(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.mark(r1, "fixed")
	if res, err := e.shipChecked(e.box, e.task.Key); err != nil {
		t.Fatalf("round 1's ship = %+v, %v", res, err)
	}
	e.setClock(e.clockNow().Add(time.Minute))
	e.j.choice = map[string]string{"": "critical", "fixes": "complete", "r1": "new"}
	r2 := e.review("The cache key leaves out the tenant")
	var run int64
	if err := e.s.db.R.QueryRow(`SELECT run_id FROM work_log WHERE id = ?`, r2).Scan(&run); err != nil || run == 0 {
		t.Fatalf("round 2's run: %d, %v", run, err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.s.db.W.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	loop := func(from int64) string {
		t.Helper()
		l, err := e.s.againLoopOf(e.ctx, r2, from)
		if err != nil || l == nil {
			t.Fatalf("loop = %+v, %v", l, err)
		}
		if want := "(" + strings.Join(append([]string{"0"}, strings.Fields(strings.Trim(fmt.Sprint(l.earlier), "[]"))...), ", ") + ")"; l.in != want {
			t.Errorf("the SQL list %q isn't its rounds %v", l.in, l.earlier)
		}
		return fmt.Sprint(l.earlier)
	}
	with := fmt.Sprint([]int64{r1})
	if got := loop(run); got != with {
		t.Fatalf("in the run that asked for both: %s, want %s", got, with)
	}

	// From a later run of the task, after this one stopped short: the
	// same loop. After one that ended any other way: another change.
	for outcome, want := range map[string]string{"paused": with, "expired": with, "timeout": with, "done": "[]", "released": "[]"} {
		exec(`UPDATE agent_runs SET ended_at = started_at + 1, outcome = ? WHERE id = ?`, outcome, run)
		if got := loop(run + 1); got != want {
			t.Errorf("from the run after one that ended %s: %s, want %s", outcome, got, want)
		}
	}
	exec(`UPDATE agent_runs SET ended_at = NULL, outcome = '' WHERE id = ?`, run)

	// Another branch is another change.
	var bp string
	if err := e.s.db.R.QueryRow(`SELECT before_pr FROM work_log WHERE id = ?`, r1).Scan(&bp); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE work_log SET before_pr = json_set(before_pr, '$.branch', 'wp-1-other') WHERE id = ?`, r1)
	if got := loop(run); got != "[]" {
		t.Errorf("a round of another branch is in the loop: %s", got)
	}
	exec(`UPDATE work_log SET before_pr = ? WHERE id = ?`, bp, r1)

	// A decision that opened the pull request ended the loop before it; a
	// probe's, like another round's, went on.
	for arm, want := range map[string]string{AgainEnough: "[]", AgainRepeat: "[]", AgainCap: "[]", AgainRejected: "[]", AgainProbe: with, AgainReview: with} {
		exec(`UPDATE review_again_decisions SET arm = ? WHERE round_id = ?`, arm, r1)
		if got := loop(run); got != want {
			t.Errorf("after a round decided %q: %s, want %s", arm, got, want)
		}
	}

	// A round that was decided ran, whatever became of it afterwards (a
	// decision is made on findings); one that never ran has no decision
	// and found nothing to repeat. (The arm is set here: the range above
	// leaves whichever came last.)
	exec(`UPDATE review_again_decisions SET arm = ? WHERE round_id = ?`, AgainReview, r1)
	exec(`UPDATE work_log SET state = ? WHERE id = ?`, RoundSkipped, r1)
	if got := loop(run); got != with {
		t.Errorf("a decided round, skipped afterwards: %s, want %s", got, with)
	}
	exec(`DELETE FROM review_again_decisions WHERE round_id = ?`, r1)
	if got := loop(run); got != "[]" {
		t.Errorf("a skipped round with no decision is in the loop: %s", got)
	}
	exec(`UPDATE work_log SET state = ? WHERE id = ?`, RoundDone, r1)

	// A round after the pull request has no loop, nor has a row that
	// isn't a round.
	exec(`UPDATE work_log SET before_pr = '' WHERE id = ?`, r2)
	if l, err := e.s.againLoopOf(e.ctx, r2, run); err != nil || l != nil {
		t.Errorf("a round after its pull request: %+v, %v", l, err)
	}
	if l, err := e.s.againLoopOf(e.ctx, -5, run); err != nil || l != nil {
		t.Errorf("no such round: %+v, %v", l, err)
	}
}

// A round the session ended at a Stop without shipping, in a run that then
// lapsed, is decided at the ship of the run that took the task up again:
// its findings call for another round as they would have at its own ship,
// and that round is the loop's second (WP-1320's audit: both the Stop and
// the new run opened the pull request with nothing decided).
func TestReviewAgainAfterTheRoundEnded(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.mark(r1, "fixed")
	e.stopWith("Fixed it; shipping later.")
	if r := e.round(r1); r.state != RoundDone {
		t.Fatalf("round 1 after its Stop: %+v", r)
	}
	if _, err := e.s.AgentPause(e.ctx, e.ag, e.task.Key, ""); err != nil {
		t.Fatal(err)
	}
	e.lapse(ClaimLease + time.Hour)
	if err := e.s.expireRuns(e.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AgentResume(e.ctx, e.ag, e.task.Key, ""); err != nil {
		t.Fatal(err)
	}
	var runs int
	_ = e.s.db.R.QueryRow(`SELECT count(*) FROM agent_runs WHERE issue_id = ?`, e.task.ID).Scan(&runs)
	if runs < 2 {
		t.Fatalf("runs = %d: the test wants a new run", runs)
	}
	e.gateOpen(e.task.Key)
	res, err := e.shipChecked(e.box, e.task.Key)
	if err != nil || res.Pull != nil {
		t.Fatalf("the resumed run's ship = %+v, %v", res, err)
	}
	if arm, index, _, _, next := e.decision(r1); arm != AgainReview || index != 1 || next == 0 {
		t.Fatalf("round 1: %s index %d next %d", arm, index, next)
	}
	if len(e.g.created) != 0 {
		t.Fatalf("opened %d pull requests", len(e.g.created))
	}
}

// The change's risk is read for the decision whoever reviews it: the tier
// its round was given, else decided from what triage saw of the change,
// else the task's last round's, else unknown (held as elevated), and a
// read that fails is an error, not "unknown" (WP-1320's audit).
func TestRoundRisk(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := e.s.db.W.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	risk := func() string {
		t.Helper()
		tier, err := e.s.roundRisk(e.ctx, r1)
		if err != nil {
			t.Fatal(err)
		}
		return tier
	}
	exec(`DELETE FROM review_stakes WHERE round_id = ?`, r1)
	exec(`DELETE FROM review_triage WHERE round_id = ?`, r1)
	if got := risk(); got != StakesUnknown {
		t.Errorf("nothing known of the change: %q", got)
	}
	// Kept as decided: asked again it is read, not decided again.
	var n int
	if err := e.s.db.R.QueryRow(`SELECT count(*) FROM review_stakes WHERE round_id = ? AND tier = ?`, r1, StakesUnknown).Scan(&n); err != nil || n != 1 {
		t.Errorf("the decided tier is kept %d times, %v", n, err)
	}
	exec(`DELETE FROM review_stakes WHERE round_id = ?`, r1)
	exec(`INSERT INTO review_triage(round_id, issue_id, project_id, repo, head, rule, action, features, created_at)
		VALUES (?, ?, ?, 'acme/app', 'aaa', 'floor:sensitive_path', 'review', '{"files":1,"added":3,"sensitive":["internal/core/hooks.go"]}', 0)`, r1, e.task.ID, e.p.ID)
	if got := risk(); got != StakesElevated {
		t.Errorf("a sensitive path: %q", got)
	}
	exec(`UPDATE review_stakes SET tier = '' WHERE round_id = ?`, r1)
	if got := risk(); got != StakesUnknown {
		t.Errorf("a row with no tier (a Waypoint without the gateway's audit, before WP-1320's audit): %q", got)
	}
	if tier, err := e.s.roundRisk(e.ctx, -5); err == nil {
		t.Errorf("no such round: %q, no error", tier)
	}
}
