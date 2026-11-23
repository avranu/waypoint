package core

import (
	"fmt"
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

// markNth marks the round's i-th finding (from 0).
func (e *loopEnv) markNth(round int64, i int, state string) {
	e.t.Helper()
	_, au := e.roundAuditRow(round)
	full, err := e.s.GetAudit(e.ctx, e.admin, au.ID)
	if err != nil || len(full.Findings) <= i {
		e.t.Fatalf("audit = %+v %v", full, err)
	}
	if _, err := e.s.MarkAuditFindings(e.ctx, e.ag, MarkFindings{IDs: []string{full.Findings[i].ID}, State: state, Note: "Checked."}); err != nil {
		e.t.Fatal(err)
	}
}

// A round whose findings were filed records how many, on the decision and
// on the findings' scores. The record the loop's rule reads still counts a
// filed finding as found: the round before it is useful either way, and
// counted apart (UsefulAllFiled) when every useful finding of the round
// after it was filed. Standing leaves rejected findings out.
func TestReviewRoundRecordsFiled(t *testing.T) {
	one := []string{"The cache key leaves out the tenant"}
	two := []string{"The cache key leaves out the tenant", "The retry drops the last write"}
	cases := []struct {
		name                      string
		titles                    []string
		mark                      func(e *loopEnv, r2 int64)
		allFiled, standing, filed int
	}{
		{"filed", one, func(e *loopEnv, r2 int64) { e.fileFinding(r2) }, 1, 1, 1},
		{"fixed", one, func(e *loopEnv, r2 int64) { e.mark(r2, "fixed") }, 0, 1, 0},
		{"filed beside a fix", two, func(e *loopEnv, r2 int64) { e.fileFinding(r2); e.markNth(r2, 1, "fixed") }, 0, 2, 1},
		{"filed beside a rejection", two, func(e *loopEnv, r2 int64) { e.fileFinding(r2); e.markNth(r2, 1, "rejected") }, 1, 1, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
			r1 := e.review("Merge loses an update")
			e.mark(r1, "fixed")
			// Waypoint scheduled round 2 itself when the check agreed (shipReviewed), so this ship only finds it pending.
			if res, err := e.shipChecked(e.box, e.task.Key); err != nil || !strings.Contains(res.Held, "Nothing more to run") {
				t.Fatalf("round 1's ship = %+v, %v", res, err)
			}
			e.setClock(e.clockNow().Add(time.Minute))
			r2 := e.review(c.titles...)
			c.mark(e, r2)
			if _, err := e.shipChecked(e.box, e.task.Key); err != nil {
				t.Fatal(err)
			}
			var n, open int
			if err := e.s.db.R.QueryRow(`SELECT filed, left_open FROM review_again_decisions WHERE round_id = ?`, r2).Scan(&n, &open); err != nil || n != c.filed {
				t.Fatalf("the decision's filed = %d (left open %d), want %d: %v", n, open, c.filed, err)
			}
			if err := e.s.db.R.QueryRow(`SELECT count(*), coalesce(sum(filed), 0) FROM review_finding_scores WHERE round_id = ?`, r2).Scan(&open, &n); err != nil || open != len(c.titles) || n != c.filed {
				t.Fatalf("scores %d, filed %d, want %d, %d: %v", open, n, len(c.titles), c.filed, err)
			}
			byRound, _, err := e.s.againReport(e.ctx, "1 = 1", nil, 0)
			if err != nil || len(byRound) != 2 {
				t.Fatalf("report = %+v, %v", byRound, err)
			}
			first, second := byRound[0], byRound[1]
			if first.Followed != 1 || first.Useful != 1 || first.UsefulAllFiled != c.allFiled {
				t.Errorf("round 1 followed %d useful %d all-filed %d; want 1, 1, %d", first.Followed, first.Useful, first.UsefulAllFiled, c.allFiled)
			}
			if second.Standing != c.standing || second.Filed != c.filed {
				t.Errorf("round 2 standing %d filed %d, want %d, %d", second.Standing, second.Filed, c.standing, c.filed)
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

// The conditions on filing and on ships held, and the helpers' conditions
// that read filed findings: a filed finding held as real, so it is neither
// a rejection of a check's finding nor a refutation proved wrong only when
// it was fixed.
func TestReviewHealthFiledAndHolds(t *testing.T) {
	cases := []struct {
		name    string
		by      []AgainRoundStat
		helpers []AgainHelperStat
		holds   []AgainHoldStat
		want    string
	}{
		{name: "a majority of standing findings filed", by: []AgainRoundStat{{Index: 1, Standing: 12, Filed: 7, Arms: map[string]int{}}, {Index: 2, Standing: 8, Filed: 4, Arms: map[string]int{}}}, want: "findings are mostly filed"},
		{name: "half filed is fine", by: []AgainRoundStat{{Index: 1, Standing: 20, Filed: 10, Arms: map[string]int{}}}},
		{name: "too few standing findings to say", by: []AgainRoundStat{{Index: 1, Standing: 19, Filed: 19, Arms: map[string]int{}}}},
		{name: "three repeat holds of one task in a day", holds: []AgainHoldStat{{Issue: "WP-1547", Holds: 4, Repeats: 3}}, want: "Review loop: WP-1547's ship held again and again"},
		{name: "holds that each came after marks", holds: []AgainHoldStat{{Issue: "WP-1547", Holds: 12, Repeats: 2}, {Issue: "WP-2", Holds: 1}}},
		{name: "a check's findings mostly filed aren't rejected", helpers: []AgainHelperStat{{Check: "goals", Fixed: 3, Filed: 30, Rejected: 10}}},
		{name: "a check's findings mostly rejected, a few filed", helpers: []AgainHelperStat{{Check: "goals", Fixed: 1, Filed: 2, Rejected: 10}}, want: "Haiku check goals are mostly rejected"},
		{name: "refuted findings held, half of them filed", helpers: []AgainHelperStat{{Check: helperVerify, Runs: 5, Done: 5, Refuted: 10, RefutedHeld: 3, RefutedFiled: 3}}, want: "refutes findings sessions fix"},
		{name: "refuted findings held, half or fewer", helpers: []AgainHelperStat{{Check: helperVerify, Runs: 5, Done: 5, Refuted: 10, RefutedHeld: 3, RefutedFiled: 2}}},
	}
	for _, c := range cases {
		got := reviewHealthIssues(c.by, nil, c.helpers, c.holds)
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
	// Two tasks looping are two tasks to file: the open one of one doesn't
	// stand for the other.
	got := reviewHealthIssues(nil, nil, nil, []AgainHoldStat{{Issue: "WP-1", Holds: 3, Repeats: 3}, {Issue: "WP-2", Holds: 5, Repeats: 4}})
	if len(got) != 2 || got[0].title == got[1].title {
		t.Errorf("two looping tasks = %q", healthTitles(got))
	}
}

// The reader counts a task's holds since a time, and the most repeat holds
// inside one day. A repeat is a hold for the same audit's findings by the
// same refusal as the task's hold before it, with no fewer left to decide:
// a hold with fewer left came after marks, and one by another refusal or
// for another audit is new. The hold before may be older than the window;
// a hold 24 hours after another is in the next day; another project's
// holds aren't read.
func TestAgainHolds(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	a := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "A"})
	b := pe.issue(pe.admin, IssueInput{Project: pe.p.Key, Title: "B"})
	other := pe.project(pe.admin, "Other", "")
	c := pe.issue(pe.admin, IssueInput{Project: other.Key, Title: "C"})
	const base = 100000000
	hour := time.Hour.Milliseconds()
	hold := func(issue, audit int64, check string, findings int, h int64) {
		pe.mexec(`INSERT INTO review_ship_holds(issue_id, audit_id, round_id, check_name, findings, created_at) VALUES (?, ?, 1, ?, ?, ?)`, issue, audit, check, findings, base+h*hour)
	}
	// A: audit 1 held with 4, 3, 2, 1 left: marks each time, no repeat.
	for i, n := range []int{4, 3, 2, 1} {
		hold(a.ID, 1, "round", n, int64(i))
	}
	// Audit 2: its first hold, then repeats at hours 5, 6 and 29 (a day
	// after 5), and the check's first hold of it.
	hold(a.ID, 2, "round", 2, 4)
	hold(a.ID, 2, "round", 2, 5)
	hold(a.ID, 2, "round", 2, 6)
	hold(a.ID, 2, "marks", 2, 7)
	hold(a.ID, 2, "round", 2, 29)
	// B: a hold before the window, then its repeat inside it.
	hold(b.ID, 3, "round", 1, -10)
	hold(b.ID, 3, "round", 1, 1)
	// C, another project's: three repeats.
	for h := range int64(4) {
		hold(c.ID, 4, "round", 1, h)
	}
	got, err := pe.s.againHolds(pe.ctx, "i.project_id = ?", []any{pe.p.ID}, base)
	want := []AgainHoldStat{{Issue: a.Key, Holds: 9, Repeats: 2, Audits: []int64{2}}, {Issue: b.Key, Holds: 1, Repeats: 1, Audits: []int64{3}}}
	if err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("holds = %+v, %v; want %+v", got, err, want)
	}
}

// A ship held again and again with nothing marked between files the health
// task, named for the task; one held each time after marks files nothing,
// however many times.
func TestShipHoldsLoopFilesHealth(t *testing.T) {
	e := newLoopEnv(t, map[string]string{"": "cosmetic", "fixes": "complete"})
	r1 := e.review("Merge loses an update", "Missing test", "Off by one", "The cache key leaves out the tenant", "The retry drops the last write", "A log line leaks the token")
	title := "Review loop: " + e.task.Key + "'s ship held again and again with nothing marked"
	filed := func() int {
		t.Helper()
		var n int
		if err := e.s.db.R.QueryRow(`SELECT count(*) FROM issues WHERE title = ?`, title).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	ship := func() {
		t.Helper()
		res, err := e.ship(e.task.Key, ShipPR)
		if err != nil || !strings.Contains(res.Held, "still to decide") {
			t.Fatalf("ship = %+v, %v", res, err)
		}
		e.s.Wait()
	}
	ship()
	for i := range 5 {
		e.markNth(r1, i, "fixed")
		ship()
	}
	if n := filed(); n != 0 {
		t.Fatalf("six holds, each after marks, filed %d", n)
	}
	for range 3 {
		ship()
	}
	if n := filed(); n != 1 {
		t.Fatalf("three holds with nothing marked filed %d, want 1 (holds %v)", n, e.holds())
	}
	// The task names the audit the ship was held for, which wp pr rounds
	// shows too.
	var audit int64
	var body string
	if err := e.s.db.R.QueryRow(`SELECT audit_id FROM review_ship_holds WHERE issue_id = ? ORDER BY id DESC LIMIT 1`, e.task.ID).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if err := e.s.db.R.QueryRow(`SELECT description FROM issues WHERE title = ?`, title).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("audit #%d", audit); !strings.Contains(body, want) {
		t.Errorf("the health task doesn't name %s:\n%s", want, body)
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
// yield record the loop's rule draws from (againYield): it draws what a
// round whose finding was fixed draws, not what one whose only finding was
// low does. Filing is measured beside the record, not taken out of it
// (WP-1555; WP-1569 changes the rule).
func TestAgainYieldCountsFiledFinding(t *testing.T) {
	draw := func(name string, mark func(e *loopEnv, r2 int64)) float64 {
		t.Helper()
		e := newLoopEnv(t, map[string]string{"": "critical", "fixes": "complete"})
		r1 := e.review("Merge loses an update")
		e.mark(r1, "fixed")
		if _, err := e.shipChecked(e.box, e.task.Key); err != nil {
			t.Fatal(err)
		}
		e.setClock(e.clockNow().Add(time.Minute))
		r2 := e.review("The cache key leaves out the tenant")
		mark(e, r2)
		if _, err := e.shipChecked(e.box, e.task.Key); err != nil {
			t.Fatal(err)
		}
		var arm string
		if err := e.s.db.R.QueryRow(`SELECT arm FROM review_again_decisions WHERE round_id = ?`, r1).Scan(&arm); err != nil {
			t.Fatal(err)
		}
		// A round of only low findings is still running after the ship,
		// and the draw reads done rounds only, so the test ends it.
		if _, err := e.s.db.W.Exec(`UPDATE work_log SET state = 'done' WHERE id = ?`, r2); err != nil {
			t.Fatal(err)
		}
		p, n, err := e.s.againYield(e.ctx, arm, 1, r1)
		if err != nil || n != 1 {
			t.Fatalf("%s: the record has %d rounds, %v; want 1", name, n, err)
		}
		return p
	}
	filed := draw("filed", func(e *loopEnv, r2 int64) { e.fileFinding(r2) })
	fixed := draw("fixed", func(e *loopEnv, r2 int64) { e.markNth(r2, 0, "fixed") })
	missed := draw("low", func(e *loopEnv, r2 int64) {
		_, au := e.roundAuditRow(r2)
		if _, err := e.s.db.W.Exec(`UPDATE code_audit_findings SET severity = 'low' WHERE audit_id = ?`, au.ID); err != nil {
			t.Fatal(err)
		}
		e.markNth(r2, 0, "fixed")
	})
	if fixed == missed {
		t.Fatalf("a hit and a miss draw the same %.4f: the runs don't tell them apart", fixed)
	}
	if filed != fixed {
		t.Errorf("filed draws %.4f, a fix %.4f, a low finding's fix %.4f: the filed finding should count as found", filed, fixed, missed)
	}
}
