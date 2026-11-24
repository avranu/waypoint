package core

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func healthTitles(is []reviewHealthIssue) []string {
	var out []string
	for _, i := range is {
		out = append(out, i.title)
	}
	return out
}

func hasHealth(is []reviewHealthIssue, title string) bool {
	for _, i := range is {
		if strings.Contains(i.title, title) {
			return true
		}
	}
	return false
}

// Each reopen condition trips on a record that meets it and not on one just
// short of it, in size or in rate.
func TestReviewHealthIssues(t *testing.T) {
	cases := []struct {
		name    string
		by      []AgainRoundStat
		agree   *AgainAgreement
		helpers []AgainHelperStat
		want    string
	}{
		{name: "nothing recorded"},
		{name: "rounds rarely useful", by: []AgainRoundStat{{Index: 1, Followed: 20, Useful: 4, Arms: map[string]int{}}}, want: "rounds after round 1 rarely find anything"},
		{name: "a quarter useful is fine", by: []AgainRoundStat{{Index: 1, Followed: 20, Useful: 5, Arms: map[string]int{}}}},
		{name: "too few rounds to say", by: []AgainRoundStat{{Index: 1, Followed: 19, Useful: 0, Arms: map[string]int{}}}},
		{name: "index 3 reads as 3 or later", by: []AgainRoundStat{{Index: 3, Followed: 30, Useful: 1, Arms: map[string]int{}}}, want: "round 3 or later"},
		{name: "one cap trips", by: []AgainRoundStat{{Index: 3, Arms: map[string]int{AgainCap: 1}}}, want: "reached the round cap"},
		{name: "probes as useful as again", by: []AgainRoundStat{{Index: 2, Followed: 20, Useful: 10, ProbeFollowed: 10, ProbeUseful: 4, Arms: map[string]int{}}}, want: "probes at round 2"},
		{name: "probes well under again", by: []AgainRoundStat{{Index: 2, Followed: 20, Useful: 10, ProbeFollowed: 10, ProbeUseful: 3, Arms: map[string]int{}}}},
		{name: "too few probes", by: []AgainRoundStat{{Index: 2, Followed: 20, Useful: 10, ProbeFollowed: 9, ProbeUseful: 9, Arms: map[string]int{}}}},
		{name: "too few again rounds to compare", by: []AgainRoundStat{{Index: 2, Followed: 19, Useful: 10, ProbeFollowed: 10, ProbeUseful: 9, Arms: map[string]int{}}}},
		{name: "neither probes nor again rounds find anything", by: []AgainRoundStat{{Index: 2, Followed: 20, Useful: 0, ProbeFollowed: 10, ProbeUseful: 0, Arms: map[string]int{}}}, want: "rounds after round 2 rarely find anything"},
		{name: "the check of marks mostly passed over, summed over indexes", by: []AgainRoundStat{{Index: 1, Checks: 12, Passed: 6, Arms: map[string]int{}}, {Index: 2, Checks: 8, Passed: 5, Unchecked: 3, Arms: map[string]int{}}}, want: "check of sessions' marks mostly doesn't answer"},
		{name: "the check of marks half passed over is fine", by: []AgainRoundStat{{Index: 1, Checks: 20, Passed: 10, Arms: map[string]int{}}}},
		{name: "too few checks of marks", by: []AgainRoundStat{{Index: 1, Checks: 19, Passed: 19, Arms: map[string]int{}}}},
		{name: "the decision model mostly missing", by: []AgainRoundStat{{Index: 1, Decided: 12, Scoring: 12, Labels: 6, Arms: map[string]int{AgainNoModel: 6}}, {Index: 2, Decided: 8, Scoring: 8, Labels: 5, Arms: map[string]int{AgainUnscored: 5}}}, want: "decision model mostly can't score"},
		{name: "the decision model missing where signals decided", by: []AgainRoundStat{{Index: 1, Decided: 20, Scoring: 20, Labels: 11, Signalled: 20, Arms: map[string]int{"": 20}}}, want: "decision model mostly can't score"},
		{name: "rounds the model wasn't asked about don't dilute it", by: []AgainRoundStat{{Index: 1, Decided: 60, Scoring: 20, Labels: 11, Arms: map[string]int{AgainRejected: 40, AgainNoModel: 11}}}, want: "decision model mostly can't score"},
		{name: "the decision model missing half the time is fine", by: []AgainRoundStat{{Index: 1, Decided: 20, Scoring: 20, Labels: 10, Arms: map[string]int{AgainNoModel: 10}}}},
		{name: "too few decided to say", by: []AgainRoundStat{{Index: 1, Decided: 19, Scoring: 19, Labels: 19, Arms: map[string]int{AgainNoModel: 19}}}},
		{name: "labels and scores apart", agree: &AgainAgreement{Findings: 50, MeanGap: 3.1}, want: "disagree"},
		{name: "labels and scores close", agree: &AgainAgreement{Findings: 50, MeanGap: 3}},
		{name: "too few scored", agree: &AgainAgreement{Findings: 49, MeanGap: 6}},
		{name: "panel check mostly skipped", helpers: []AgainHelperStat{{Check: "goals", Runs: 20, Done: 9, Skipped: 11}}, want: "goals mostly doesn't run"},
		{name: "half skipped is fine", helpers: []AgainHelperStat{{Check: "goals", Runs: 20, Done: 10, Skipped: 6, Failed: 4}}},
		{name: "verify mostly fails", helpers: []AgainHelperStat{{Check: helperVerify, Runs: 20, Done: 5, Failed: 15}}, want: "verify mostly doesn't run"},
		{name: "problems rarely confirmed", helpers: []AgainHelperStat{{Check: "claims", Runs: 20, Done: 20, Answered: 20, Raised: 100, Confirmed: 4}}, want: "claims's problems are rarely confirmed"},
		{name: "problems confirmed enough", helpers: []AgainHelperStat{{Check: "claims", Runs: 20, Done: 20, Answered: 20, Raised: 100, Confirmed: 5}}},
		{name: "too few answered runs to judge leads", helpers: []AgainHelperStat{{Check: "claims", Runs: 30, Done: 30, Answered: 19, Raised: 100}}},
		{name: "findings mostly rejected by sessions", helpers: []AgainHelperStat{{Check: "tests", Runs: 5, Done: 5, Raised: 20, Confirmed: 12, Fixed: 2, Rejected: 10}}, want: "from Haiku check tests are mostly rejected by sessions"},
		{name: "findings mostly held by sessions", helpers: []AgainHelperStat{{Check: "tests", Runs: 5, Done: 5, Raised: 20, Confirmed: 12, Fixed: 3, Rejected: 9}}},
		{name: "refuted findings fixed", helpers: []AgainHelperStat{{Check: helperVerify, Runs: 5, Done: 5, Refuted: 10, RefutedHeld: 6}}, want: "refutes findings sessions fix"},
		{name: "refuted findings mostly rejected", helpers: []AgainHelperStat{{Check: helperVerify, Runs: 5, Done: 5, Refuted: 10, RefutedHeld: 5}}},
		{name: "confirmed findings rejected", helpers: []AgainHelperStat{{Check: helperVerify, Runs: 5, Done: 5, Confirmed: 10, ConfirmedRejected: 6}}, want: "confirms findings sessions reject"},
		{name: "asks mostly unsettled", by: []AgainRoundStat{{Index: 1, Sampled: 12, Unsettled: 7, Arms: map[string]int{}}, {Index: 2, Sampled: 8, Unsettled: 4, Arms: map[string]int{}}}, want: "asks mostly don't settle the decision"},
		{name: "unsettled half the time is fine", by: []AgainRoundStat{{Index: 1, Sampled: 20, Unsettled: 10, Arms: map[string]int{}}}},
		{name: "too few sampled", by: []AgainRoundStat{{Index: 1, Sampled: 19, Unsettled: 19, Arms: map[string]int{}}}},
		{name: "refutations don't repeat", helpers: []AgainHelperStat{{Check: helperVerify, Runs: 5, Done: 5, Reasked: 10, Overturned: 6}}, want: "refutations don't repeat"},
		{name: "refutations half repeat is fine", helpers: []AgainHelperStat{{Check: helperVerify, Runs: 5, Done: 5, Reasked: 10, Overturned: 5}}},
		{name: "too few re-asked", helpers: []AgainHelperStat{{Check: helperVerify, Runs: 5, Done: 5, Reasked: 9, Overturned: 9}}},
		{name: "asking again never overturns", helpers: []AgainHelperStat{{Check: helperVerify, Runs: 20, Done: 20, Reasked: 50}}, want: "never overturns"},
		{name: "asking again overturned once", helpers: []AgainHelperStat{{Check: helperVerify, Runs: 20, Done: 20, Reasked: 50, Overturned: 1}}},
		{name: "too few re-asked to say it never overturns", helpers: []AgainHelperStat{{Check: helperVerify, Runs: 20, Done: 20, Reasked: 49}}},
		{name: "verify has no leads condition", helpers: []AgainHelperStat{{Check: helperVerify, Runs: 20, Done: 20, Raised: 0}}},
	}
	for _, c := range cases {
		got := reviewHealthIssues(c.by, c.agree, c.helpers, nil)
		if c.want == "" {
			if len(got) != 0 {
				t.Errorf("%s: tripped %q", c.name, healthTitles(got))
			}
			continue
		}
		if len(got) != 1 || !hasHealth(got, c.want) {
			t.Errorf("%s: want one %q, got %q", c.name, c.want, healthTitles(got))
		}
		if len(got) == 1 && !strings.Contains(got[0].body, "30 days") {
			t.Errorf("%s: body doesn't name the window: %s", c.name, got[0].body)
		}
	}
}

// The loop's record changing in a project (EvReviewLoopRecorded) files a
// task for a tripped condition there, once while that task is open, and
// again after it is closed and the condition still holds. Nothing polls:
// only the event checks.
func TestReviewHealthOnEvent(t *testing.T) {
	pe := newPREnv(t, "forgejo")
	now := pe.s.nowMS()
	recorded := func(pid int64) error {
		t.Helper()
		err := pe.s.write(pe.ctx, SystemActor(), func(tx *tx) error {
			tx.emit(&Event{Kind: EvReviewLoopRecorded, ProjectID: pid})
			return nil
		})
		pe.s.bg.Wait()
		return err
	}
	audit := pe.mexec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at) VALUES (?, ?, 'me/app', 'repo', ?, ?)`, pe.p.ID, pe.f.ID, pe.admin.User.ID, now)
	for i := range 20 {
		state := "skipped"
		if i < 5 {
			state = "done"
		}
		pe.mexec(`INSERT INTO audit_helper_runs(audit_id, check_name, state, created_at) VALUES (?, 'goals', ?, ?)`, audit, state, now)
	}
	open := func() []int64 {
		t.Helper()
		rows, err := pe.s.db.R.QueryContext(pe.ctx, `SELECT i.id FROM issues i JOIN statuses st ON st.id = i.status_id
			WHERE i.project_id = ? AND i.title = 'Review loop: Haiku check goals mostly doesn''t run' AND st.category NOT IN ('done','canceled')`, pe.p.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		return ids
	}
	if ids := open(); len(ids) != 0 {
		t.Fatalf("a task before any event: %v", ids)
	}
	for range 2 {
		if err := recorded(pe.p.ID); err != nil {
			t.Fatal(err)
		}
	}
	ids := open()
	if len(ids) != 1 {
		t.Fatalf("open health tasks after two events: %v", ids)
	}
	var desc string
	if err := pe.s.db.R.QueryRowContext(pe.ctx, `SELECT description FROM issues WHERE id = ?`, ids[0]).Scan(&desc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(desc, "Of 20 runs of the goals check") || !strings.Contains(desc, "15 were skipped") {
		t.Fatalf("task text: %s", desc)
	}
	pe.mexec(`UPDATE issues SET status_id = (SELECT id FROM statuses WHERE project_id = ? AND category = 'done' LIMIT 1) WHERE id = ?`, pe.p.ID, ids[0])
	if err := recorded(pe.p.ID); err != nil {
		t.Fatal(err)
	}
	if again := open(); len(again) != 1 || again[0] == ids[0] {
		t.Fatalf("after closing the first, open: %v (first %d)", again, ids[0])
	}
	pe.mexec(`DELETE FROM audit_helper_runs`)
	old := pe.s.now().AddDate(0, 0, -reviewHealthDays-1).UnixMilli()
	for range 30 {
		pe.mexec(`INSERT INTO audit_helper_runs(audit_id, check_name, state, created_at) VALUES (?, 'tests', 'failed', ?)`, audit, old)
	}
	before := len(open())
	if err := recorded(pe.p.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pe.s.db.R.QueryRowContext(pe.ctx, `SELECT count(*) FROM issues WHERE title LIKE 'Review loop: Haiku check tests%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(open()) != before {
		t.Fatalf("runs outside the window filed %d", n)
	}
	// The event checks only its own project; a record that can't be read files nothing, and the next event reads it again.
	other := pe.project(pe.admin, "Other", "")
	otherAudit := pe.mexec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at) VALUES (?, ?, 'me/other', 'repo', ?, ?)`, other.ID, pe.f.ID, pe.admin.User.ID, now)
	for range 20 {
		pe.mexec(`INSERT INTO audit_helper_runs(audit_id, check_name, state, created_at) VALUES (?, 'claims', 'skipped', ?)`, otherAudit, now)
	}
	claims := func() int {
		t.Helper()
		if err := pe.s.db.R.QueryRowContext(pe.ctx, `SELECT count(*) FROM issues WHERE project_id = ? AND title = 'Review loop: Haiku check claims mostly doesn''t run'`, other.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := recorded(pe.p.ID); err != nil || claims() != 0 {
		t.Errorf("another project's event filed in Other: %d, %v", n, err)
	}
	pe.s.healthReadFault = func(int64) error { return errors.New("database is locked") }
	if err := recorded(other.ID); err != nil || claims() != 0 {
		t.Errorf("an unreadable record filed: %d, %v", n, err)
	}
	pe.s.healthReadFault = nil
	if err := recorded(other.ID); err != nil || claims() != 1 {
		t.Errorf("Other's own event: %d, %v", n, err)
	}
	h := reviewHealthIssue{title: "Review loop: a test condition", body: "Figures."}
	pe.mexec(`UPDATE users SET disabled = 1 WHERE id = ?`, pe.s.leftoverOwner(pe.ctx, pe.s.db.R, pe.p.ID))
	if err := pe.s.fileReviewHealth(pe.ctx, pe.p.ID, h); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("a disabled owner: %v", err)
	}
	pe.mexec(`DELETE FROM project_members WHERE project_id = ?`, pe.p.ID)
	pe.mexec(`UPDATE projects SET created_by = NULL WHERE id = ?`, pe.p.ID)
	if err := pe.s.fileReviewHealth(pe.ctx, pe.p.ID, h); err == nil || !strings.Contains(err.Error(), "no owner") {
		t.Errorf("no owner: %v", err)
	}
}

// The helpers' record, by check and project and since a time: runs by
// state, leads after pruning, the findings that came from a check's leads
// and what the session did with them, and the check of findings' verdicts
// against the session's marks.
func TestAgainHelperStats(t *testing.T) {
	fx := newHelperFixture(t)
	p := fx.project(fx.admin, "Alpha", "kanban")
	other := fx.project(fx.admin, "Beta", "kanban")
	audit := func(project int64, at int64) int64 {
		return fx.exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, owner_id, created_at, status) VALUES (?, 0, '', 'repo', 0, ?, 'done')`, project, at)
	}
	run := func(a int64, check, state, leads string, dropped int, cost float64, at int64) {
		fx.exec(`INSERT INTO audit_helper_runs(audit_id, check_name, state, leads, dropped, cost_usd, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, a, check, state, leads, dropped, cost, at)
	}
	finding := func(a int64, n int, state, verdict, lead string) {
		fx.exec(`INSERT INTO code_audit_findings(audit_id, ordinal, title, path, line, severity, problem, state, verdict, lead) VALUES (?, ?, 'F', 'a.go', 1, 'high', 'x', ?, ?, ?)`, a, n, state, verdict, lead)
	}
	a := audit(p.ID, 100)
	run(a, "goals", "done", `[{"id":"goals.1","confirmed":true},{"id":"goals.2","confirmed":true}]`, 4, 0.01, 100)
	fx.exec(`UPDATE audit_helper_runs SET cut = '[{"title":"A","why":"repeats"},{"id":"goals.3","title":"B","why":"refuted by a second read of the code: no"},{"id":"goals.4","title":"C","why":"not checked: the check ran out of time"},{"id":"goals.5","title":"D","why":"confirmed, the same defect as the reviewer''s finding R1"}]' WHERE check_name = 'goals'`)
	run(a, "goals", "skipped", `[]`, 0, 0, 100)
	run(a, "goals", "done", `[{"id":"goals.1"},{"id":"goals.2"},{"id":"goals.3"}]`, 0, 0.02, 100)
	fx.exec(`UPDATE audit_helper_runs SET answered = 0 WHERE id = (SELECT max(id) FROM audit_helper_runs)`)
	run(a, "tests", "failed", `[]`, 0, 0.002, 100)
	run(a, helperVerify, "done", `[]`, 0, 0.03, 100)
	run(a, helperCombine, "done", `[]`, 0, 0.004, 100)
	finding(a, 1, "fixed", "confirmed", "")
	finding(a, 2, "rejected", "confirmed", "")
	finding(a, 3, "fixed", "refuted", "")
	finding(a, 4, "rejected", "refuted", "")
	finding(a, 5, "open", "unsupported", "")
	finding(a, 6, "fixed", "", "")
	finding(a, 7, "rejected", "refuted", "")
	finding(a, 8, "open", "unsupported", "")
	fx.exec(`UPDATE code_audit_findings SET verdict_note = 'Asked twice, refuted both times. no' WHERE audit_id = ? AND ordinal = 7`, a)
	fx.exec(`UPDATE code_audit_findings SET verdict_note = 'Asked twice, the check answered refuted, then confirmed. First: no Then: yes' WHERE audit_id = ? AND ordinal = 8`, a)
	fx.exec(`INSERT INTO code_audit_findings(audit_id, ordinal, title, path, line, severity, problem, state, verdict, lead) VALUES (?, 9, 'P', 'a.go', 1, 'high', 'x', 'fixed', 'confirmed', 'goals.3,tests.2')`, a)
	finding(a, 10, "fixed", "confirmed", "goals.1")
	finding(a, 11, "rejected", "confirmed", "goals.2")
	finding(a, 12, "rejected", "confirmed", "tests.1")
	fx.exec(`UPDATE code_audit_findings SET verdict_note = 'Asked twice; the second ask failed, and one answer doesn''t refute. First: no' WHERE audit_id = ? AND ordinal = 3`, a)
	b := audit(other.ID, 100)
	run(b, "goals", "done", `[{"id":"goals.1"}]`, 0, 1, 100)
	finding(b, 1, "fixed", "refuted", "goals.1")
	old := audit(p.ID, 10)
	run(old, "goals", "done", `[{"id":"goals.1"}]`, 0, 1, 10)
	finding(old, 1, "fixed", "refuted", "goals.1")

	got, err := fx.s.againHelperStats(fx.ctx, "i.project_id = ?", []any{p.ID}, 50)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]AgainHelperStat{}
	var order []string
	for _, h := range got {
		by[h.Check] = h
		order = append(order, h.Check)
	}
	if strings.Join(order, ",") != "goals,tests,verify,combine" {
		t.Fatalf("checks in order = %v", order)
	}
	g := by["goals"]
	if g.Runs != 3 || g.Done != 2 || g.Answered != 1 || g.Skipped != 1 || g.Raised != 5 || g.Confirmed != 3 || g.Added != 3 || g.Fixed != 2 || g.Rejected != 1 || math.Abs(g.CostUSD-0.03) > 1e-9 {
		t.Errorf("goals = %+v", g)
	}
	if ts := by["tests"]; ts.Runs != 1 || ts.Failed != 1 || ts.Added != 2 || ts.Fixed != 1 || ts.Rejected != 1 {
		t.Errorf("tests = %+v", ts)
	}
	v := by[helperVerify]
	if v.Runs != 1 || v.Confirmed != 2 || v.ConfirmedRejected != 1 || v.Refuted != 3 || v.RefutedHeld != 1 || v.Unsupported != 2 || v.Reasked != 2 || v.Overturned != 1 {
		t.Errorf("verify = %+v", v)
	}
	none, err := fx.s.againHelperStats(fx.ctx, "i.project_id = ?", []any{p.ID}, 200)
	if err != nil || len(none) != 0 {
		t.Errorf("after the window: %+v, %v", none, err)
	}
}
