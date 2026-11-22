package core

import (
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"testing"
	"time"
)

// WP-1555: filing a finding instead of fixing it ends a round, and a ship
// held for findings still to decide leaves a row, so both can be read back.

// fileFinding marks the round's only finding medium, confirmed, filed under
// the task itself (findingFiled).
func (e *loopEnv) fileFinding(round int64) {
	e.t.Helper()
	_, au := e.roundAuditRow(round)
	if _, err := e.s.db.W.Exec(`UPDATE code_audit_findings SET severity = 'medium' WHERE audit_id = ?`, au.ID); err != nil {
		e.t.Fatal(err)
	}
	id := fmt.Sprintf("%d.1", au.ID)
	if _, err := e.s.MarkAuditFindings(e.ctx, e.ag, MarkFindings{IDs: []string{id}, State: "confirmed", Note: "Real, outside this change.", Issue: e.task.Key}); err != nil {
		e.t.Fatal(err)
	}
}

// A round whose new finding was filed records how many were filed, on the
// decision and on the finding's score; the review health reads it apart
// from a fix: the round before it is followed but not useful, as a fix
// would have made it.
func TestReviewRoundRecordsFiled(t *testing.T) {
	for _, filed := range []bool{true, false} {
		t.Run(fmt.Sprint("filed ", filed), func(t *testing.T) {
			e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
			r1 := e.review("Merge loses an update")
			e.mark(r1, "fixed")
			if res, err := e.shipChecked(e.box, e.task.Key); err != nil || !strings.Contains(res.Held, "adversarial review round 2") {
				t.Fatalf("round 1's ship = %+v, %v", res, err)
			}
			e.setClock(e.clockNow().Add(time.Minute))
			r2 := e.review("The cache key leaves out the tenant")
			want := 0
			if filed {
				e.fileFinding(r2)
				want = 1
			} else {
				e.mark(r2, "fixed")
			}
			if _, err := e.shipChecked(e.box, e.task.Key); err != nil {
				t.Fatal(err)
			}
			var n, open int
			if err := e.s.db.R.QueryRow(`SELECT filed, left_open FROM review_again_decisions WHERE round_id = ?`, r2).Scan(&n, &open); err != nil || n != want {
				t.Fatalf("the decision's filed = %d (left open %d), want %d: %v", n, open, want, err)
			}
			if err := e.s.db.R.QueryRow(`SELECT count(*), coalesce(sum(filed), 0) FROM review_finding_scores WHERE round_id = ?`, r2).Scan(&open, &n); err != nil || open != 1 || n != want {
				t.Fatalf("scores %d, filed %d, want 1, %d: %v", open, n, want, err)
			}
			byRound, _, err := e.s.againReport(e.ctx, "1 = 1", nil, 0)
			if err != nil || len(byRound) != 2 {
				t.Fatalf("report = %+v, %v", byRound, err)
			}
			first, second := byRound[0], byRound[1]
			if first.Followed != 1 || first.Useful != 1 || first.UsefulAllFiled != want {
				t.Errorf("round 1 followed %d useful %d all-filed %d; want 1, 1, %d", first.Followed, first.Useful, first.UsefulAllFiled, want)
			}
			if second.Standing != 1 || second.Filed != want {
				t.Errorf("round 2 standing %d filed %d, want 1, %d", second.Standing, second.Filed, want)
			}
		})
	}
}

// Findings filed are their own count in a helper's record: a finding from a
// check's leads that a session filed is neither fixed nor rejected, and a
// finding the check of findings refuted that a session filed is not "held"
// as a fix is.
func TestAgainHelperStatsFiled(t *testing.T) {
	fx := newHelperFixture(t)
	p := fx.project(fx.admin, "Alpha", "kanban")
	a := fx.exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at, status) VALUES (?, 0, '', 'repo', 0, 100, 'done')`, p.ID)
	fx.exec(`INSERT INTO audit_helper_runs(audit_id, check_name, state, created_at) VALUES (?, 'goals', 'done', 100)`, a)
	issue := fx.exec(`INSERT INTO issues(project_id, number, title, status_id, created_at, updated_at) VALUES (?, 99, 'T', (SELECT id FROM statuses WHERE project_id = ? LIMIT 1), 1, 1)`, p.ID, p.ID)
	finding := func(n int, state, severity, verdict, lead string, filed bool) {
		var iss any
		if filed {
			iss = issue
		}
		fx.exec(`INSERT INTO code_audit_findings(audit_id, ordinal, title, path, line, severity, problem, state, verdict, lead, issue_id) VALUES (?, ?, 'F', 'a.go', 1, ?, 'x', ?, ?, ?, ?)`, a, n, severity, state, verdict, lead, iss)
	}
	finding(1, "fixed", "high", "", "goals.1", false)
	finding(2, "confirmed", "medium", "", "goals.2", true) // filed
	finding(3, "confirmed", "high", "", "goals.3", true)   // high: not filed, still confirmed
	finding(4, "rejected", "medium", "", "goals.4", false)
	finding(5, "fixed", "high", "refuted", "", false)
	finding(6, "confirmed", "low", "refuted", "", true) // filed
	got, err := fx.s.againHelperStats(fx.ctx, "i.project_id = ?", []any{p.ID}, 50)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]AgainHelperStat{}
	for _, h := range got {
		by[h.Check] = h
	}
	g := by["goals"]
	if g.Added != 4 || g.Fixed != 2 || g.Filed != 1 || g.Rejected != 1 {
		t.Errorf("goals = %+v; want added 4, fixed (or confirmed) 2, filed 1, rejected 1", g)
	}
	v := by[helperVerify]
	if v.Refuted != 2 || v.RefutedHeld != 1 || v.RefutedFiled != 1 {
		t.Errorf("verify = %+v; want refuted 2, held 1, filed 1", v)
	}
}

// The conditions on filing and on ships held.
func TestReviewHealthFiledAndHolds(t *testing.T) {
	cases := []struct {
		name  string
		by    []AgainRoundStat
		holds []AgainHoldStat
		want  string
	}{
		{name: "a majority of standing findings filed", by: []AgainRoundStat{{Index: 1, Standing: 12, Filed: 7, Arms: map[string]int{}}, {Index: 2, Standing: 8, Filed: 4, Arms: map[string]int{}}}, want: "findings are mostly filed"},
		{name: "half filed is fine", by: []AgainRoundStat{{Index: 1, Standing: 20, Filed: 10, Arms: map[string]int{}}}},
		{name: "too few standing findings to say", by: []AgainRoundStat{{Index: 1, Standing: 19, Filed: 19, Arms: map[string]int{}}}},
		{name: "five holds of one task in a day", holds: []AgainHoldStat{{Issue: "WP-1547", Holds: 5}}, want: "ships held again and again"},
		{name: "four holds is a run's usual", holds: []AgainHoldStat{{Issue: "WP-1547", Holds: 4}, {Issue: "WP-2", Holds: 1}}},
	}
	for _, c := range cases {
		got := reviewHealthIssues(c.by, nil, nil, c.holds)
		if c.want == "" {
			if len(got) != 0 {
				t.Errorf("%s: tripped %q", c.name, healthTitles(got))
			}
			continue
		}
		if len(got) != 1 || !hasHealth(got, c.want) {
			t.Errorf("%s: want one %q, got %q", c.name, c.want, healthTitles(got))
			continue
		}
		if !strings.Contains(got[0].body, "30 days") {
			t.Errorf("%s: body doesn't name the window: %s", c.name, got[0].body)
		}
	}
}

// The reader finds the most holds of one task inside any day, in the
// project, within the window.
func TestAgainHolds(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	a := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "A"})
	b := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "B"})
	hour := time.Hour.Milliseconds()
	at := func(issue int64, h int64) {
		pe.mexec(`INSERT INTO review_ship_holds(issue_id, audit_id, round_id, check_name, findings, created_at) VALUES (?, 1, 1, 'round', 1, ?)`, issue, 100000000+h*hour)
	}
	// A: three in an hour of day one, then two more 30 hours on: at most 3 in a day.
	for _, h := range []int64{0, 0, 1, 30, 31} {
		at(a.ID, h)
	}
	// B: one before the window starts.
	at(b.ID, -10)
	got, err := pe.s.againHolds(pe.ctx, "i.project_id = ?", []any{pe.p.ID}, 100000000)
	if err != nil || len(got) != 1 || got[0].Issue != a.Key || got[0].Holds != 3 {
		t.Fatalf("holds = %+v, %v", got, err)
	}
}

func (e *loopEnv) holds() (out []string) {
	e.t.Helper()
	rows, err := e.s.db.R.Query(`SELECT check_name, audit_id, round_id, findings, created_at FROM review_ship_holds WHERE issue_id = ? ORDER BY id`, e.task.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var audit, round, n, at int64
		if err := rows.Scan(&name, &audit, &round, &n, &at); err != nil {
			e.t.Fatal(err)
		}
		if at == 0 {
			e.t.Error("a hold with no time")
		}
		out = append(out, fmt.Sprintf("%s/%d/%d/%d", name, audit, round, n))
	}
	return out
}

// The refusal of reviewAgain: a finding still to do holds the ship, and the
// hold is recorded; one whose record can't be written still returns its text.
func TestShipHoldRecordedByRound(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "cosmetic", "fixes": "complete"})
	r1 := e.review("Merge loses an update") // high: filing it holds
	_, au := e.roundAuditRow(r1)
	id := fmt.Sprintf("%d.1", au.ID)
	if _, err := e.s.MarkAuditFindings(e.ctx, e.ag, MarkFindings{IDs: []string{id}, State: "confirmed", Issue: e.task.Key}); err != nil {
		t.Fatal(err)
	}
	res, err := e.ship(e.task.Key, ShipPR)
	if err != nil || !strings.Contains(res.Held, "still to decide") {
		t.Fatalf("ship = %+v %v", res, err)
	}
	if want := fmt.Sprintf("round/%d/%d/1", au.ID, r1); fmt.Sprint(e.holds()) != fmt.Sprint([]string{want}) {
		t.Fatalf("holds = %v, want %s", e.holds(), want)
	}
	// A record that can't be written leaves the hold as it was.
	if _, err := e.s.db.W.Exec(`DROP TABLE review_ship_holds`); err != nil {
		t.Fatal(err)
	}
	res, err = e.ship(e.task.Key, ShipPR)
	if err != nil || !strings.Contains(res.Held, "still to decide") || res.Pull != nil {
		t.Fatalf("ship with no table = %+v %v", res, err)
	}
}

// The refusal of checkMarks: what the check of the session's marks found
// is still to decide.
func TestShipHoldRecordedByMarks(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.mark(r1, "rejected")
	res, err := e.ship(e.task.Key, ShipPR)
	if err != nil || !strings.Contains(res.Held, "checking your marks") {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	if h := e.holds(); len(h) != 0 {
		t.Fatalf("waiting on the check recorded %v: that isn't findings to decide", h)
	}
	e.stopWith("Waiting for the check of my marks.")
	e.endCheck(e.box, "done", "[x.1] Merge loses an update")
	e.deliverNews()
	res, err = e.ship(e.task.Key, ShipPR)
	if err != nil || !strings.Contains(res.Held, "rejections it thinks were wrong") {
		t.Fatalf("ship before the check's finding is marked = %+v, %v", res, err)
	}
	var check int64
	if err := e.s.db.R.QueryRow(`SELECT id FROM code_audits WHERE request_key = ?`, fmt.Sprintf(checkAuditKey, r1)).Scan(&check); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("marks/%d/%d/1", check, r1); fmt.Sprint(e.holds()) != fmt.Sprint([]string{want}) {
		t.Fatalf("holds = %v, want %s", e.holds(), want)
	}
}

// The refusal of shipAudited: the session read its round's audit itself and
// left findings undecided.
func TestShipHoldRecordedByAudited(t *testing.T) {
	a, box, _ := firstEnv(t)
	if _, err := a.s.SetAutopilot(a.ctx, a.admin, a.p.Key, AutopilotSettingInput{Merge: true,
		Limits: AutopilotLimits{Review: ptrOf(true)}}); err != nil {
		t.Fatal(err)
	}
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	if _, err := a.s.AgentTake(a.ctx, a.ag, first.Key, ""); err != nil {
		t.Fatal(err)
	}
	a.clock = a.clock.Add(time.Minute)
	if res, err := a.ship(first.Key, ShipAuto); err != nil || res.Held == "" {
		t.Fatalf("ship = %+v, %v", res, err)
	}
	roundID := a.newestRoundID()
	a.stopWith("Pushed; the review comes first.")
	a.tick(61 * time.Second)
	a.endAudit(box, "done", "Off by one", "Missing test")
	a.tick(time.Second)
	ra, _ := a.roundAuditRow(roundID)
	if _, err := a.s.MarkAuditFindings(a.ctx, a.ag, MarkFindings{IDs: []string{itoa64(ra.id) + ".1"}, State: "rejected", Note: "not reachable"}); err != nil {
		t.Fatal(err)
	}
	res, err := a.ship(first.Key, ShipPR)
	if err != nil || res.Pull != nil || !strings.Contains(res.Notice, "1 finding still to decide") {
		t.Fatalf("one left = %+v, %v", res, err)
	}
	var name string
	var audit, round, n int64
	if err := a.s.db.R.QueryRow(`SELECT check_name, audit_id, round_id, findings FROM review_ship_holds WHERE issue_id = ?`, first.ID).Scan(&name, &audit, &round, &n); err != nil {
		t.Fatalf("no hold recorded: %v", err)
	}
	if name != "audit" || audit != ra.id || round != roundID || n != 1 {
		t.Errorf("hold = %s audit %d round %d findings %d; want audit, %d, %d, 1", name, audit, round, n, ra.id, roundID)
	}
}

// A round whose only medium finding was filed still counts as a hit in the
// yield record the loop's rule draws from (againYield): filing is measured
// beside the record, not taken out of it (WP-1555; WP-1569 changes the rule).
func TestAgainYieldCountsFiledFinding(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
	r1 := e.review("Merge loses an update")
	e.mark(r1, "fixed")
	if _, err := e.shipChecked(e.box, e.task.Key); err != nil {
		t.Fatal(err)
	}
	e.setClock(e.clockNow().Add(time.Minute))
	r2 := e.review("The cache key leaves out the tenant")
	e.fileFinding(r2)
	if _, err := e.shipChecked(e.box, e.task.Key); err != nil {
		t.Fatal(err)
	}
	var arm string
	if err := e.s.db.R.QueryRow(`SELECT arm FROM review_again_decisions WHERE round_id = ?`, r1).Scan(&arm); err != nil {
		t.Fatal(err)
	}
	p, n, err := e.s.againYield(e.ctx, arm, 1, r1)
	if err != nil || n != 1 {
		t.Fatalf("the record has %d rounds, %v; want 1", n, err)
	}
	h := fnv.New32a()
	if arm == AgainReview {
		_, _ = fmt.Fprintf(h, "yield/%d", r1)
	} else {
		_, _ = fmt.Fprintf(h, "yield/%s/%d", arm, r1)
	}
	u := (float64(h.Sum32()%10000) + 0.5) / 10000
	if want := betaQuantile(u, 2, 1); math.Abs(p-want) > 1e-9 {
		t.Errorf("yield draw %.4f, want %.4f (a hit: the filed finding counts as found); a miss would draw %.4f", p, want, betaQuantile(u, 1, 2))
	}
}
