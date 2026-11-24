package core

import (
	"fmt"
	"strings"
	"testing"
)

// A check audit carried to a pull request holds its merge on an open finding
// of any severity while it is the newest finished audit, and its marks the
// check didn't answer are listed in the packet without holding it (WP-1370).
func TestCarriedCheckHoldsOnAnySeverity(t *testing.T) {
	for _, newest := range []bool{true, false} {
		name := "the check is the newest audit"
		if !newest {
			name = "the check is carried beside a newer audit"
		}
		t.Run(name, func(t *testing.T) { carriedCheckHolds(t, newest) })
	}
}

func carriedCheckHolds(t *testing.T, checkNewest bool) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.mark(r1, "rejected")
	if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	e.stopWith("Waiting.")
	e.endCheck(e.box, "done", "[x.1] Merge loses an update")
	e.s.Wait()
	if len(e.g.created) == 0 {
		if err := e.s.shipRound(e.ctx, shipRoundIn{round: r1, why: "the session never took it up"}); err != nil {
			t.Fatal(err)
		}
	}
	var prID, checkID int64
	if err := e.s.db.R.QueryRow(`SELECT id FROM pull_requests ORDER BY id DESC LIMIT 1`).Scan(&prID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.db.R.QueryRow(`SELECT id FROM code_audits WHERE request_key = ?`, fmt.Sprintf(checkAuditKey, r1)).Scan(&checkID); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE code_audits SET pr_id = ?, purpose = '', finished_at = coalesce(finished_at, created_at) + 3600000 WHERE id = ?`, prID, checkID)
	if !checkNewest {
		// The round's own audit finished after the check: the check is
		// carried, not newest.
		e.exec(`UPDATE code_audits SET pr_id = ?, purpose = '', finished_at = (SELECT finished_at FROM code_audits WHERE id = ?) + 3600000 WHERE request_key LIKE 'review-round:%' AND id != ?`, prID, checkID, checkID)
	}
	e.exec(`UPDATE code_audit_findings SET severity = 'low' WHERE audit_id = ?`, checkID)
	reasons, _, err := e.s.auditHolds(e.ctx, prID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(reasons, "\n"), fmt.Sprintf("audit #%d", checkID)) {
		t.Fatalf("an open low finding of the newest check audit doesn't hold the merge: %v", reasons)
	}
}

// Marks the session made on a medium or low finding that nothing checked are
// listed in the pull request's packet, and don't hold its merge or start a
// round.
func TestUncheckedLowRejectionsAreListedNotHeld(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update", "Retry never backs off")
	e.exec(`UPDATE code_audit_findings SET severity = 'low' WHERE audit_id = (SELECT audit_id FROM work_log WHERE id = ?)`, r1)
	e.mark(r1, "rejected", "rejected")
	if _, err := e.ship(e.task.Key, ShipAuto); err != nil {
		t.Fatal(err)
	}
	e.stopWith("Waiting for the check of my marks.")
	e.endCheck(e.box, "cancelled")
	e.s.Wait()
	if err := e.s.shipRound(e.ctx, shipRoundIn{round: r1, why: "the review's report never came"}); err != nil {
		t.Fatal(err)
	}
	var prID int64
	if err := e.s.db.R.QueryRow(`SELECT id FROM pull_requests ORDER BY id DESC LIMIT 1`).Scan(&prID); err != nil {
		t.Fatal(err)
	}
	todo := e.s.todoFindings(e.ctx, prID)
	if len(todo) != 2 {
		t.Fatalf("the packet lists %d findings, want the 2 nobody checked", len(todo))
	}
	for _, f := range todo {
		if f.startsRound() {
			t.Errorf("%s starts a round", f.ID)
		}
	}
	if reasons, _, _ := e.s.auditHolds(e.ctx, prID, 0); len(reasons) != 0 {
		t.Fatalf("unchecked low rejections hold the merge: %v", reasons)
	}
	var b strings.Builder
	renderFindings(&b, todo)
	if !strings.Contains(b.String(), "unchecked") {
		t.Fatalf("the packet doesn't say they are unchecked:\n%s", b.String())
	}
}
