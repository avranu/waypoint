package core

import (
	"strings"
	"testing"
	"time"
)

// roundThenShip runs a task through a review round and its pull request.
// early is a comment written before the round's audit is requested, late one
// written after; tamper changes the round's records before the open.
func roundThenShip(t *testing.T, early, late string, tamper func(a *apEnv, roundID int64)) (res *OpenPRResult, ask string) {
	t.Helper()
	a, first, id, ask := roundDone(t, early, late)
	if tamper != nil {
		tamper(a, id)
	}
	res, err := a.ship(first, ShipPR)
	if err != nil || res.Pull == nil {
		t.Fatalf("open = %+v, %v", res, err)
	}
	return res, ask
}

// reviewedAfter is what the open would say about a round whose records
// tamper changed, without the review gate's own say on whether it opens.
func reviewedAfter(t *testing.T, tamper func(a *apEnv, roundID int64)) bool {
	t.Helper()
	a, first, id, _ := roundDone(t, "", "")
	tamper(a, id)
	var branch string
	if err := a.s.db.R.QueryRowContext(a.ctx, `SELECT json_extract(before_pr, '$.branch') FROM work_log WHERE id = ?`, id).Scan(&branch); err != nil {
		t.Fatal(err)
	}
	issue := a.issueID(first)
	run, err := a.s.openRunIn(a.ctx, a.s.db.R, a.ag.Session, issue)
	if err != nil || run == 0 {
		t.Fatalf("open run = %d, %v", run, err)
	}
	return a.s.reviewedRound(a.ctx, issue, run, branch)
}

// roundDone runs a task through a review round that ended with its finding
// marked fixed, the session about to open the pull request.
func roundDone(t *testing.T, early, late string) (a *apEnv, key string, id int64, ask string) {
	t.Helper()
	var box *Runner
	a, box, _ = firstEnv(t)
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Merge: true,
		Limits: AutopilotLimits{Review: ptrOf(true)}}); err != nil {
		t.Fatal(err)
	}
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.clock = a.clock.Add(time.Minute)
	if r, err := a.ship(first.Key, ShipAuto); err != nil || r.Held == "" {
		t.Fatalf("ship = %+v, %v", r, err)
	}
	if early != "" {
		if _, err := a.s.AddComment(a.ctx, a.admin, first.Key, early); err != nil {
			t.Fatal(err)
		}
	}
	a.stopWith("Pushed; the review comes first.")
	a.tick(61 * time.Second)
	a.endAudit(box, "done", "Off by one")
	a.tick(time.Second)
	id = a.newestRoundID()
	ra, au := a.roundAuditRow(id)
	if au != nil && au.Helpers != nil {
		for _, c := range au.Helpers.Panel {
			if c.Name == "complete" {
				ask = c.Ask
			}
		}
	}
	if late != "" {
		if _, err := a.s.AddComment(a.ctx, a.admin, first.Key, late); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.s.MarkAuditFindings(a.ctx, a.ag, MarkFindings{IDs: []string{itoa64(ra.id) + ".1"}, State: "fixed", Note: "added"}); err != nil {
		t.Fatal(err)
	}
	return a, first.Key, id, ask
}

// Reviewed means this ship's own round ran its audit with the task's notes
// in the completeness check's ask; anything less leaves the open's own block.
func TestReviewedNeedsThisRunsRoundWithItsAuditAndTheNotes(t *testing.T) {
	const note = "Step 2 only if the first pass is still slow."
	exec := func(a *apEnv, q string, args ...any) {
		if _, err := a.s.db.W.ExecContext(a.ctx, q, args...); err != nil {
			a.t.Fatal(err)
		}
	}
	t.Run("the audit ran with the notes in its ask", func(t *testing.T) {
		res, ask := roundThenShip(t, note, "", nil)
		if !res.Reviewed || !strings.Contains(ask, note) {
			t.Errorf("reviewed %v, complete ask:\n%s", res.Reviewed, ask)
		}
	})
	t.Run("a note written after the audit was requested", func(t *testing.T) {
		res, ask := roundThenShip(t, "", note, nil)
		if res.Reviewed || strings.Contains(ask, note) {
			t.Errorf("reviewed %v, ask:\n%s", res.Reviewed, ask)
		}
	})
	t.Run("a round of an earlier run", func(t *testing.T) {
		if reviewedAfter(t, func(a *apEnv, id int64) {
			exec(a, `INSERT INTO agent_runs(issue_id, agent, user_id, started_at, ended_at, session_id)
				SELECT issue_id, agent, user_id, started_at, started_at, session_id FROM agent_runs WHERE id = (SELECT run_id FROM work_log WHERE id = ?)`, id)
			exec(a, `UPDATE work_log SET run_id = (SELECT max(id) FROM agent_runs) WHERE id = ?`, id)
		}) {
			t.Error("another run's round marked this open reviewed")
		}
	})
	t.Run("a round that never requested its audit", func(t *testing.T) {
		if reviewedAfter(t, func(a *apEnv, id int64) {
			exec(a, `UPDATE work_log SET audit_id = NULL WHERE id = ?`, id)
		}) {
			t.Error("a round with no audit marked the open reviewed")
		}
	})
	t.Run("a round whose audit failed", func(t *testing.T) {
		if reviewedAfter(t, func(a *apEnv, id int64) {
			exec(a, `UPDATE code_audits SET status = 'failed' WHERE id = (SELECT audit_id FROM work_log WHERE id = ?)`, id)
		}) {
			t.Error("a failed audit marked the open reviewed")
		}
	})
	t.Run("an audit requested before the notes reached its panel", func(t *testing.T) {
		res, _ := roundThenShip(t, note, "", func(a *apEnv, id int64) {
			exec(a, `UPDATE code_audits SET helpers = '' WHERE id = (SELECT audit_id FROM work_log WHERE id = ?)`, id)
		})
		if res.Reviewed {
			t.Error("an audit with no put-off leads marked the open reviewed")
		}
	})
}

// The pull request's own put-off block is for a direct open: after a review
// round the completeness check already had the notes as leads (WP-1370).
func TestShipReviewedMarksTheOpenAfterARound(t *testing.T) {
	t.Run("direct open", func(t *testing.T) {
		a, _, g := firstEnv(t)
		if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Limits: AutopilotLimits{Review: ptrOf(false)}}); err != nil {
			t.Fatal(err)
		}
		first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
		_, _ = a.s.AgentTake(a.ctx, a.ag, first.Key, "")
		res, err := a.ship(first.Key, ShipAuto)
		if err != nil || res.Pull == nil || len(g.created) != 1 || res.Reviewed {
			t.Fatalf("direct open = %+v, %v", res, err)
		}
	})
	t.Run("skipped review", func(t *testing.T) {
		a, _, g := firstEnv(t)
		first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
		_, _ = a.s.AgentTake(a.ctx, a.ag, first.Key, "")
		res, err := a.ship(first.Key, ShipSkip, func(in *OpenPRInput) { in.Reason = "a typo" })
		if err != nil || res.Pull == nil || len(g.created) != 1 || res.Reviewed {
			t.Fatalf("skipped = %+v, %v", res, err)
		}
	})
	t.Run("after a round", func(t *testing.T) {
		a, box, g := firstEnv(t)
		if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Merge: true,
			Limits: AutopilotLimits{Review: ptrOf(true)}}); err != nil {
			t.Fatal(err)
		}
		first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
		if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
			t.Fatal(err)
		}
		a.clock = a.clock.Add(time.Minute)
		if res, err := a.ship(first.Key, ShipAuto); err != nil || res.Held == "" || res.Reviewed {
			t.Fatalf("ship = %+v, %v", res, err)
		}
		a.stopWith("Pushed; the review comes first.")
		a.tick(61 * time.Second)
		a.endAudit(box, "done", "Off by one")
		a.tick(time.Second)
		ra, _ := a.roundAuditRow(a.newestRoundID())
		if _, err := a.s.MarkAuditFindings(a.ctx, a.ag, MarkFindings{IDs: []string{itoa64(ra.id) + ".1"}, State: "fixed", Note: "added"}); err != nil {
			t.Fatal(err)
		}
		res, err := a.ship(first.Key, ShipPR)
		if err != nil || res.Pull == nil || len(g.created) != 1 || !res.Reviewed {
			t.Fatalf("after the round = %+v, %v", res, err)
		}
	})
}
