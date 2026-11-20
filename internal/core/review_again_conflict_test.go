package core

import (
	"errors"
	"strings"
	"testing"
)

// The four places reviewAgain refuses a ship that lost a race, as the
// round would have scheduled the next one: something changes the round
// while the decision model is asked (the decision is made outside the
// write that records it), and the write finds it (WP-1320).
func TestReviewAgainConflictReturns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(e *loopEnv, round int64)
		want   string // in the error
	}{
		{"another ship recorded a newer round", func(e *loopEnv, round int64) {
			e.exec(`INSERT INTO work_log(issue_id, kind, state, session_id, run_id, user_id, agent, prompt, created_at, updated_at)
				SELECT issue_id, 'review', 'skipped', session_id, run_id, user_id, 'claude', '', 1, 1 FROM work_log WHERE id = ?`, round)
		}, "another wp ship changed"},
		{"the round's report was taken back (not started)", func(e *loopEnv, round int64) {
			e.exec(`UPDATE work_log SET started_at = NULL WHERE id = ?`, round)
		}, "adversarial review changed just now"},
		{"the round is in a state no report can end", func(e *loopEnv, round int64) {
			e.exec(`UPDATE work_log SET state = 'skipped' WHERE id = ?`, round)
		}, "adversarial review changed just now"},
		{"another ship decided the round first", func(e *loopEnv, round int64) {
			var issue, audit int64
			if err := e.s.db.R.QueryRow(`SELECT w.issue_id, coalesce(`+againAuditSQL+`, 0) FROM work_log w WHERE w.id = ?`, round).Scan(&issue, &audit); err != nil {
				e.t.Fatal(err)
			}
			d := againDecision{round: round, issue: issue, audit: audit, arm: AgainEnough, yieldP: -1}
			if err := e.s.write(e.ctx, e.admin, func(tx *tx) error {
				_, err := d.insert(e.ctx, tx, e.s.nowMS())
				return err
			}); err != nil {
				e.t.Fatal(err)
			}
		}, "another wp ship decided"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
			r1 := e.review("Merge loses an update")
			e.mark(r1, "fixed")
			var once bool
			e.j.vary = func(string, int) string {
				if !once {
					once = true
					tc.change(e, r1)
				}
				return ""
			}
			_, err := e.shipChecked(e.box, e.task.Key)
			if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ship = %v, want a conflict saying %q", err, tc.want)
			}
			if !once {
				t.Fatal("the decision model was never asked")
			}
			// The loser scheduled nothing.
			var rounds int
			_ = e.s.db.R.QueryRow(`SELECT count(*) FROM work_log WHERE kind = 'review' AND id > ?`, r1).Scan(&rounds)
			if tc.name == "another ship recorded a newer round" {
				if rounds != 1 {
					t.Errorf("rounds after the first = %d, want only the one the other ship made", rounds)
				}
			} else if rounds != 0 {
				t.Errorf("the losing ship scheduled %d round(s)", rounds)
			}
		})
	}
}
