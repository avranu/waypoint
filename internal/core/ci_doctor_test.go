package core

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMergeSteps(t *testing.T) {
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	m := func(n int) time.Time { return at.Add(time.Duration(n) * time.Minute) }
	run := ciFullRun{ok: true, trigger: m(10), start: m(15), end: m(40)}
	st := mergeSteps(m(0), m(5), run, m(50))
	want := CIMergeSteps{N: 1, Update: 300, BeforeFull: 300, RunnerWait: 300, FullRun: 1500, AfterGreen: 600, Total: 3000}
	if st != want {
		t.Errorf("steps = %+v, want %+v", st, want)
	}
	// A head tested before its turn: its run costs the turn nothing.
	early := ciFullRun{ok: true, trigger: m(-60), start: m(-55), end: m(-30)}
	if st := mergeSteps(m(0), m(-70), early, m(20)); st.AfterGreen != 1200 || st.Update+st.BeforeFull+st.RunnerWait+st.FullRun != 0 {
		t.Errorf("an early run: %+v", st)
	}
	// No run known: the total only.
	if st := mergeSteps(m(0), m(5), ciFullRun{}, m(30)); st.Total != 1800 || st.FullRun != 0 {
		t.Errorf("no run: %+v", st)
	}
	med := medianSteps([]CIMergeTiming{
		{Known: true, CIMergeSteps: CIMergeSteps{FullRun: 1000, Total: 2000}},
		{Known: true, CIMergeSteps: CIMergeSteps{FullRun: 3000, RunnerWait: 100, Total: 4000}},
		{Known: false, CIMergeSteps: CIMergeSteps{Total: 99999}},
	})
	if med.N != 2 || med.FullRun != 2000 || med.Total != 3000 {
		t.Errorf("medians = %+v", med)
	}
	if step, v := biggestStep(med); step != StepFullRun || v != 2000 {
		t.Errorf("biggest = %s %d", step, v)
	}
}

func TestFullRunOf(t *testing.T) {
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	m := func(n int) time.Time { return at.Add(time.Duration(n) * time.Minute) }
	h := &ciHistory{runs: map[int64]ActionRun{11: {Index: 11, Created: m(-22), Draft: true}, 12: {Index: 12, Created: m(-2)}}, tasks: []ActionTask{
		// The draft tier and a push run of the same head don't count.
		{Name: "lint", RunNumber: 11, WorkflowID: "ci.yml", Event: "pull_request", SHA: "abc", Title: "Fix", Status: "success", Started: m(-20), Updated: m(-15)},
		{Name: "lint", RunNumber: 13, WorkflowID: "ci.yml", Event: "push", SHA: "abc", Title: "Fix", Status: "success", Started: m(30), Updated: m(40)},
		{Name: "lint", RunNumber: 12, WorkflowID: "ci.yml", Event: "pull_request", SHA: "abc", Title: "Fix", Status: "success", Started: m(0), Updated: m(5)},
		{Name: "e2e", RunNumber: 12, WorkflowID: "ci.yml", Event: "pull_request", SHA: "abc", Title: "Fix", Status: "failure", Started: m(1), Updated: m(10)},
		{Name: "e2e", RunNumber: 12, WorkflowID: "ci.yml", Event: "pull_request", SHA: "abc", Title: "Fix", Status: "success", Started: m(12), Updated: m(25)},
		{Name: "lint", RunNumber: 9, WorkflowID: "ci.yml", Event: "pull_request", SHA: "other", Title: "Fix", Status: "success", Started: m(-90), Updated: m(-80)},
	}}
	r := fullRunOf(h, "ABC")
	if !r.ok || !r.trigger.Equal(m(-2)) || !r.start.Equal(m(0)) || !r.end.Equal(m(25)) || r.reruns != 1 {
		t.Errorf("full run = %+v", r)
	}
	if r := fullRunOf(h, "nope"); r.ok {
		t.Errorf("an unknown head: %+v", r)
	}
}

func TestDoctorRules(t *testing.T) {
	for _, c := range []struct {
		runs, fail, cancel int
		want               bool
	}{{3, 3, 0, false}, {8, 2, 0, true}, {9, 2, 0, false}, {12, 0, 6, true}, {12, 1, 4, false}, {12, 8, 0, true}} {
		if got := unreliableJob(c.runs, c.fail, c.cancel); got != c.want {
			t.Errorf("unreliable(%d runs, %d failed, %d cancelled) = %v", c.runs, c.fail, c.cancel, got)
		}
	}
	for _, c := range []struct {
		p    CIDoctorPool
		want bool
	}{
		{CIDoctorPool{Capacity: 10, Busy3h: 0.3, WaitP50_3h: 60}, false},
		{CIDoctorPool{Capacity: 10, WaitP50_3h: 900}, true},
		{CIDoctorPool{Capacity: 10, Busy3h: 0.9, AvgWaiting3h: 3}, true},
		{CIDoctorPool{Capacity: 10, Busy3h: 0.9, AvgWaiting3h: 0.2}, false},
		{CIDoctorPool{Capacity: 4, Waiting: 5, OldestWait: 1200}, true},
	} {
		if got := poolSaturated(c.p); got != c.want {
			t.Errorf("saturated(%+v) = %v", c.p, got)
		}
	}
}

// The front's wait is when its state or wait last changed, not its last sync.
func TestBabysitWaitSince(t *testing.T) {
	be, pr := armedBabysit(t)
	b := be.babysit()
	start := be.clockNow()
	be.setClock(be.clockNow().Add(time.Hour))
	be.s.setState(be.ctx, b, pr, BabysitWatching, "waiting for the forge's new results")
	be.setClock(be.clockNow().Add(time.Hour))
	if err := be.s.setBabysit(be.ctx, b, map[string]any{"round": 1}); err != nil {
		t.Fatal(err)
	}
	b = be.babysit()
	if !b.WaitSince.Equal(start.Add(time.Hour)) || !b.UpdatedAt.Equal(start.Add(2*time.Hour)) {
		t.Errorf("wait since %s, updated %s (started %s)", b.WaitSince, b.UpdatedAt, start)
	}
}

func TestCIDoctorStalledLine(t *testing.T) {
	be, pr := armedBabysit(t)
	now := be.clockNow()
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := be.s.db.W.ExecContext(be.ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Two merges this morning, an hour and half an hour each.
	for _, n := range []int{8, 9, 10} {
		p := bsPR
		p.Number, p.HeadRef, p.HeadSHA = n, "b"+string(rune('0'+n)), strings.Repeat(string(rune('a'+n)), 11)
		if n == 10 {
			p.Title = "Re-run flaky e2e shards once"
		}
		be.pullHook("opened", p)
		be.s.Wait()
	}
	exec(`UPDATE pull_requests SET state = 'merged', opened_at = ?, merged_at = ? WHERE number = 8`, ms(-6*time.Hour), ms(-5*time.Hour))
	exec(`UPDATE pull_requests SET state = 'merged', opened_at = ?, merged_at = ? WHERE number = 9`, ms(-6*time.Hour), ms(-270*time.Minute))
	// #7 has been at the front four hours, waiting three on one thing.
	exec(`UPDATE pull_requests SET queued_at = ?, armed_at = ? WHERE id = ?`, ms(-4*time.Hour), ms(-4*time.Hour), pr.ID)
	exec(`UPDATE babysits SET state = 'watching', wait = 'waiting for the forge''s new results', created_at = ?, wait_at = ? WHERE pr_id = ?`, ms(-4*time.Hour), ms(-3*time.Hour), pr.ID)
	// #10, armed, waits for a person.
	p10 := be.pr(10)
	exec(`UPDATE pull_requests SET armed_at = ?, armed_by = ?, armed_style = 'merge' WHERE id = ?`, ms(-2*time.Hour), be.admin.User.ID, p10.ID)
	be.start(p10)
	exec(`UPDATE babysits SET state = 'needs_person', wait = '1 review comment waits for a person' WHERE pr_id = ? AND ended_at IS NULL`, p10.ID)

	d, err := be.s.CIDoctor(be.ctx, be.admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Lines) != 1 {
		t.Fatalf("lines: %+v", d.Lines)
	}
	l := d.Lines[0]
	if l.Armed != 2 || l.Moving != 1 || len(l.NeedsPerson) != 1 || l.Merges7d != 2 || l.Merges24h != 2 || l.PerMerge7d != 2700 {
		t.Errorf("line: %+v", l)
	}
	if l.Front == nil || l.Front.Number != 7 || l.Front.WaitFor != 3*3600 || l.AtFront != 4*3600 || !l.Stalled {
		t.Errorf("front: %+v at front %d stalled %v", l.Front, l.AtFront, l.Stalled)
	}
	if d.Verdict != "problem" || d.Bottleneck.Kind != "stalled" || !strings.Contains(d.Bottleneck.Summary, "#7") {
		t.Errorf("verdict %s, bottleneck %+v", d.Verdict, d.Bottleneck)
	}
	var kinds []string
	for _, f := range d.Findings {
		kinds = append(kinds, f.Kind)
	}
	if len(kinds) < 2 || kinds[0] != "line_stalled" || !slices.Contains(kinds, "needs_person") {
		t.Errorf("findings: %q", kinds)
	}
	if len(d.CIWork) != 1 || d.CIWork[0].Number != 10 || d.CIWork[0].Position != 0 {
		t.Errorf("CI work: %+v", d.CIWork)
	}
	// Stuck: the doctor says how #10, about CI, could cut the line (WP-544).
	if u := d.UrgentLane; len(u) != 1 || u[0].Command != "wp pr urgent 10 --repo "+p10.Repo || !strings.Contains(u[0].Text, "delays non-urgent CI") {
		t.Errorf("urgent lane: %+v", u)
	}
	// Once it is urgent, nothing more is suggested.
	exec(`UPDATE pull_requests SET labels = '["ci-urgent"]' WHERE id = ?`, p10.ID)
	if d, err := be.s.CIDoctor(be.ctx, be.admin); err != nil || len(d.CIWork) != 1 || !d.CIWork[0].Urgent || len(d.UrgentLane) != 0 {
		t.Errorf("urgent #10: err %v, CI work %+v, urgent lane %+v", err, d.CIWork, d.UrgentLane)
	}
	exec(`UPDATE pull_requests SET labels = '[]' WHERE id = ?`, p10.ID)

	// A task in a project the viewer can't open says nothing about #10.
	other := be.project(be.admin, "Secret", "")
	hidden := be.issue(be.admin, IssueInput{Project: other.Key, Title: "Speed up the merge queue"})
	exec(`INSERT INTO external_links(issue_id, provider, kind, url, repo, number, state, created_at) VALUES (?, 'github', 'pull', ?, ?, 11, 'open', 0)`, hidden.ID, "https://x.test/pull/11", p10.Repo)
	p11 := bsPR
	p11.Number, p11.HeadRef, p11.HeadSHA, p11.Title = 11, "b11", strings.Repeat("e", 11), "Tidy the docs"
	be.pullHook("opened", p11)
	be.s.Wait()
	exec(`UPDATE pull_requests SET armed_at = ?, armed_by = ? WHERE number = 11`, ms(-time.Hour), be.admin.User.ID)
	sam := be.user("sam", false)
	if err := be.s.SetMember(be.ctx, be.admin, be.p.Key, "sam", "viewer"); err != nil {
		t.Fatal(err)
	}
	work := func(a *Actor) []int {
		d, err := be.s.CIDoctor(be.ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		var ns []int
		for _, w := range d.CIWork {
			ns = append(ns, w.Number)
		}
		return ns
	}
	if got := work(be.admin); !slices.Contains(got, 11) {
		t.Errorf("admin's CI work: %v", got)
	}
	if got := work(sam); slices.Contains(got, 11) || !slices.Contains(got, 10) {
		t.Errorf("sam's CI work: %v", got)
	}
	exec(`UPDATE pull_requests SET armed_at = NULL WHERE number = 11`)

	// Armed ten minutes ago, after a babysit four hours old: ten minutes at
	// the front, not stalled.
	exec(`UPDATE pull_requests SET armed_at = ? WHERE id = ?`, ms(-10*time.Minute), pr.ID)
	if d, err = be.s.CIDoctor(be.ctx, be.admin); err != nil {
		t.Fatal(err)
	}
	if l := d.Lines[0]; l.AtFront != 600 || l.Stalled {
		t.Errorf("just armed: at front %d stalled %v", l.AtFront, l.Stalled)
	}
	if len(d.UrgentLane) != 0 {
		t.Errorf("not stuck, yet the urgent lane is suggested: %+v", d.UrgentLane)
	}

	// The front merges: the line is empty, nothing stalls.
	exec(`UPDATE pull_requests SET state = 'merged', merged_at = ?, armed_at = NULL, queued_at = NULL WHERE id = ?`, ms(0), pr.ID)
	exec(`UPDATE babysits SET state = 'merged', ended_at = ? WHERE pr_id = ?`, ms(0), pr.ID)
	d, err = be.s.CIDoctor(be.ctx, be.admin)
	if err != nil {
		t.Fatal(err)
	}
	if d.Bottleneck.Kind != "none" || d.Verdict != "warn" || d.Lines[0].Merges24h != 3 {
		t.Errorf("after the merge: %s %+v %+v", d.Verdict, d.Bottleneck, d.Lines)
	}
}

// A line whose stacks all end on conflicts or failures says so.
func TestCIDoctorStacksFailing(t *testing.T) {
	be, pr := armedBabysit(t)
	now := be.clockNow()
	add := func(state string, ago time.Duration, ended bool) {
		t.Helper()
		var end any
		if ended {
			end = now.UnixMilli()
		}
		if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO queue_stacks(forge_id, repo, base_ref, pr_id, base_sha, branch, commit_sha, state, note, run_url, created_at, ended_at)
			VALUES (?,?,?,?,'b','wp-queue/main/1','c',?,?,'https://x.test/run/9',?,?)`,
			pr.ForgeID, pr.Repo, pr.BaseRef, pr.ID, state, "#7 (abcdef1) doesn't merge cleanly onto the base + #6", now.Add(-ago).UnixMilli(), end); err != nil {
			t.Fatal(err)
		}
	}
	add(stackConflict, time.Hour, true)
	add(stackConflict, 2*time.Hour, true)
	add("failed", 3*time.Hour, true)
	add("cancelled", 4*time.Hour, true)
	add("running", 10*time.Minute, false)
	add("passed", 8*time.Hour, true) // before the window
	d, err := be.s.CIDoctor(be.ctx, be.admin)
	if err != nil {
		t.Fatal(err)
	}
	st := d.Lines[0].Stacks
	if st != (CIStackStats{Tried: 5, Running: 1, Conflicts: 2, Failed: 1, Cancelled: 1, LastFail: st.LastFail, LastRun: "https://x.test/run/9"}) || !strings.Contains(st.LastFail, "doesn't merge") {
		t.Errorf("stacks = %+v", st)
	}
	i := slices.IndexFunc(d.Findings, func(f CIFinding) bool { return f.Kind == "stacks_failing" })
	if i < 0 || !strings.Contains(d.Findings[i].Title, "2 conflicts, 1 failed, 1 cancelled") {
		t.Errorf("findings = %+v", d.Findings)
	}
}

// The urgent lane is suggested only where CI is stuck, once a place, and
// not while a pull request about CI there is urgent already (WP-544).
func TestDoctorUrgentLane(t *testing.T) {
	d := &CIDoctor{
		Lines: []CILine{{Repo: "o/a", Stalled: true}, {Repo: "o/b"}},
		Mains: []CIMain{{Repo: "o/a", Branch: "main", Status: "failure"}, {Repo: "o/c", Branch: "main", Status: "failure"}, {Repo: "o/d", Branch: "main", Status: "success"}},
		CIWork: []CIWorkPR{
			{Repo: "o/a", Number: 1, Position: 1}, // the front: urgent wouldn't move it
			{Repo: "o/a", Number: 2, Position: 4},
			{Repo: "o/b", Number: 3, Urgent: true},
		},
	}
	doctorUrgentLane(d)
	var got []string
	for _, u := range d.UrgentLane {
		got = append(got, u.Repo+": "+u.Command)
	}
	want := []string{`o/a: wp pr urgent 2 --repo o/a`, `o/c: wp ship --urgent -m "SUMMARY"`}
	if !slices.Equal(got, want) {
		t.Errorf("urgent lane = %q, want %q", got, want)
	}
	if u := d.UrgentLane[0]; u.Why != "its merge line is stalled" || !strings.Contains(u.Text, "puts #2 on the urgent lane: it delays non-urgent CI so the fix can cut the line") {
		t.Errorf("o/a: %+v", u)
	}

	// Runners: any repo's pull request about CI; none when one is urgent.
	d = &CIDoctor{Pool: CIDoctorPool{Saturated: true}, CIWork: []CIWorkPR{{Repo: "o/b", Number: 3}}}
	if doctorUrgentLane(d); len(d.UrgentLane) != 1 || d.UrgentLane[0].Repo != "" || d.UrgentLane[0].Command != "wp pr urgent 3 --repo o/b" {
		t.Errorf("runners: %+v", d.UrgentLane)
	}
	d = &CIDoctor{Pool: CIDoctorPool{Saturated: true}, CIWork: []CIWorkPR{{Repo: "o/b", Number: 3, Urgent: true}}}
	if doctorUrgentLane(d); len(d.UrgentLane) != 0 {
		t.Errorf("runners with an urgent fix: %+v", d.UrgentLane)
	}
	// Nothing stuck: nothing said.
	d = &CIDoctor{Lines: []CILine{{Repo: "o/a", Moving: 5}}, CIWork: []CIWorkPR{{Repo: "o/a", Number: 2, Position: 3}}}
	if doctorUrgentLane(d); len(d.UrgentLane) != 0 {
		t.Errorf("not stuck: %+v", d.UrgentLane)
	}
}
