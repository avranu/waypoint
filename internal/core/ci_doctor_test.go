package core

import (
	"os"
	"reflect"
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

// WP-1707: the doctor counts candidates by end_reason and the re-runs, and
// StackFates breaks only the cancelled ones down by reason.
func TestCIDoctorStackEndReasons(t *testing.T) {
	be, pr := armedBabysit(t)
	now := be.clockNow()
	add := func(state, reason string, reran int64, ago time.Duration) {
		t.Helper()
		if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO queue_stacks(forge_id, repo, base_ref, pr_id, base_sha, branch, commit_sha, state, note, run_url, created_at, ended_at, end_reason, reran)
			VALUES (?,?,?,?,'b','wp-queue/main/1','c',?,'','',?,?,?,?)`,
			pr.ForgeID, pr.Repo, pr.BaseRef, pr.ID, state, now.Add(-ago).UnixMilli(), now.UnixMilli(), reason, reran); err != nil {
			t.Fatal(err)
		}
	}
	add("cancelled", endBaseMoved, 0, time.Hour)
	add("cancelled", endBaseMoved, 0, 2*time.Hour)
	add("cancelled", endBaseRed, 5, 3*time.Hour)
	add("failed", endFailed, 7, 4*time.Hour)
	add("failed", endFailed, 0, 5*time.Minute)
	add("cancelled", "", 0, 10*time.Minute) // from before migration 0187
	d, err := be.s.CIDoctor(be.ctx, be.admin)
	if err != nil {
		t.Fatal(err)
	}
	st := d.Lines[0].Stacks
	want := map[string]int{endBaseMoved: 2, endBaseRed: 1, endFailed: 2}
	if !reflect.DeepEqual(st.Reasons, want) || st.Reran != 2 || st.Cancelled != 4 {
		t.Errorf("reasons = %v reran = %d cancelled = %d", st.Reasons, st.Reran, st.Cancelled)
	}
	if got := StackFates(st); got != "2 failed, 4 cancelled (2 base moved, 1 base red)" {
		t.Errorf("fates = %q", got)
	}
}

func TestStackFatesReasons(t *testing.T) {
	for _, c := range []struct {
		st   CIStackStats
		want string
	}{
		{CIStackStats{Conflicts: 1, Failed: 2, Cancelled: 5}, "1 conflict, 2 failed, 5 cancelled"},
		{CIStackStats{Failed: 2, Cancelled: 5, Reasons: map[string]int{endFailed: 2}}, "2 failed, 5 cancelled"},
		{CIStackStats{Failed: 2, Cancelled: 5, Reasons: map[string]int{endFailed: 2, endBaseRed: 2, endBaseMoved: 2, endNoRun: 1},
			cancelledBy: map[string]int{endBaseRed: 2, endBaseMoved: 2, endNoRun: 1}}, "2 failed, 5 cancelled (2 base moved, 2 base red, 1 no run)"},
		{CIStackStats{Failed: 1}, "1 failed"},
		{CIStackStats{}, "none failed"},
	} {
		if got := StackFates(c.st); got != c.want {
			t.Errorf("StackFates(%+v) = %q, want %q", c.st, got, c.want)
		}
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
	st.Reasons, st.cancelledBy = nil, nil // counted in TestCIDoctorStackEndReasons
	if !reflect.DeepEqual(st, CIStackStats{Tried: 5, Running: 1, Conflicts: 2, Failed: 1, Cancelled: 1, LastFail: st.LastFail, LastRun: "https://x.test/run/9"}) || !strings.Contains(st.LastFail, "doesn't merge") {
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

// stacks_paused shows only while the line's fallback is on, with when it
// ends and what it means.
func TestCIDoctorStacksPaused(t *testing.T) {
	be, pr := armedBabysit(t)
	now := be.clockNow()
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO queue_stacks(forge_id, repo, base_ref, pr_id, base_sha, branch, commit_sha, state, end_reason, created_at, ended_at)
		VALUES (?,?,?,?,'b','wp-queue/main/1','c','cancelled',?,?,?)`, pr.ForgeID, pr.Repo, pr.BaseRef, pr.ID, endNoRun, now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	finding := func() (CIFinding, *CILine) {
		t.Helper()
		d, err := be.s.CIDoctor(be.ctx, be.admin)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(d.Findings, func(f CIFinding) bool { return f.Kind == "stacks_paused" })
		if i < 0 {
			return CIFinding{}, &d.Lines[0]
		}
		return d.Findings[i], &d.Lines[0]
	}
	f, l := finding()
	until := now.Add(lineFallbackFor)
	if f.Kind != "stacks_paused" || l.Stacks.PausedUntil == nil || !l.Stacks.PausedUntil.Equal(until.Truncate(time.Millisecond)) {
		t.Fatalf("finding %+v, paused until %v, want %v", f, l.Stacks.PausedUntil, until)
	}
	if f.Detail != stacksPausedNote(until, now) {
		t.Errorf("detail %q", f.Detail)
	}
	be.setClock(now.Add(lineFallbackFor + time.Minute))
	if f, l := finding(); f.Kind != "" || l.Stacks.PausedUntil != nil {
		t.Errorf("after the fallback: %+v, %v", f, l.Stacks.PausedUntil)
	}
}

// A main push run whose only failed job is advisories is a warning that
// doesn't say the tip is held back; any other failed job is main_red as before
// (WP-1660).
func TestDoctorMainAdvisories(t *testing.T) {
	now := time.Now()
	fail := func(run int64, names ...string) []ActionTask {
		var ts []ActionTask
		for _, n := range names {
			ts = append(ts, ActionTask{Name: n, RunNumber: run, Status: "failure"})
		}
		// Another run's failed shard says nothing about this one.
		return append(ts, ActionTask{Name: "lint", RunNumber: run, Status: "success"}, ActionTask{Name: "backend shard 1", RunNumber: run + 1, Status: "failure"})
	}
	if !onlyAdvisoriesFailed(fail(7, "advisories"), 7) {
		t.Error("only advisories failed: want true")
	}
	if onlyAdvisoriesFailed(fail(7, "advisories", "backend shard 2"), 7) {
		t.Error("advisories and a shard failed: want false")
	}
	if onlyAdvisoriesFailed(nil, 7) {
		t.Error("no jobs on record: want false")
	}
	if onlyAdvisoriesFailed([]ActionTask{{Name: "advisories", RunNumber: 7, Status: "cancelled"}}, 7) {
		t.Error("advisories cancelled: it found nothing, want false")
	}

	// doctorMains reads it from the run's jobs, the kept ones too: a failed
	// shard the forge's capped read left out still makes main red.
	main := func(status string, h *ciHistory) CIMain {
		h.runs = map[int64]ActionRun{7: {Index: 7, Event: "push", PrettyRef: "main", Status: status, Created: now}}
		d := &CIDoctor{}
		snap := &ciSnapshot{repos: []*ciRepoData{{ref: ciRepoRef{forge: &Forge{ID: 1}, repo: "o/a"}, hist: h}}}
		doctorMains(d, snap, now, func(int64, string) (string, bool) { return "o/a", true })
		if len(d.Mains) != 1 {
			t.Fatalf("mains = %+v", d.Mains)
		}
		return d.Mains[0]
	}
	if m := main("failure", &ciHistory{tasks: fail(7, "advisories")}); !m.OnlyAdvisories {
		t.Errorf("read from the run's jobs: %+v", m)
	}
	if m := main("failure", &ciHistory{tasks: fail(7, "advisories"), kept: []ciKeptJob{{ActionTask: ActionTask{Name: "backend shard 2", RunNumber: 7, Status: "failure"}}}}); m.OnlyAdvisories {
		t.Errorf("a kept failed shard: %+v", m)
	}
	if m := main("cancelled", &ciHistory{tasks: fail(7, "advisories")}); m.OnlyAdvisories {
		t.Errorf("a cancelled run: %+v", m)
	}

	kinds := func(only bool) []CIFinding {
		d := &CIDoctor{Mains: []CIMain{{Repo: "o/a", Branch: "main", Run: 7, Status: "failure", OnlyAdvisories: only}}}
		doctorFindings(d, &CIBoard{}, now)
		return d.Findings
	}
	got := kinds(true)
	i := slices.IndexFunc(got, func(f CIFinding) bool { return f.Kind == "main_advisories" })
	if i < 0 || slices.ContainsFunc(got, func(f CIFinding) bool { return f.Kind == "main_red" }) {
		t.Fatalf("only advisories: findings = %+v", got)
	}
	f := got[i]
	if f.Level != "warn" || f.Title != "o/a main has known advisories (run #7)" || !strings.Contains(f.Detail, "still released and deployed") || f.Next != "wp ci log --repo o/a --run 7 --job advisories" {
		t.Errorf("finding = %+v", f)
	}
	if !strings.Contains(f.Detail, advisoriesTaskTitle("o/a", "main")) {
		t.Errorf("the finding doesn't name its task: %s", f.Detail)
	}
	got = kinds(false)
	if len(got) != 1 || got[0].Kind != "main_red" || got[0].Level != "problem" {
		t.Errorf("advisories and another job: findings = %+v", got)
	}

	// Not stuck: an advisories-only red main doesn't suggest the urgent lane.
	d := &CIDoctor{Mains: []CIMain{{Repo: "o/a", Branch: "main", Status: "failure", OnlyAdvisories: true}}}
	if doctorUrgentLane(d); len(d.UrgentLane) != 0 {
		t.Errorf("urgent lane for advisories: %+v", d.UrgentLane)
	}
}

// A scheduled run newer than the newest push that failed in its advisories job
// shows main_advisories naming that run, whatever the push run was (WP-1660).
func TestDoctorScheduledAdvisories(t *testing.T) {
	now := time.Now()
	hist := func(runs ...ActionRun) *ciHistory {
		h := &ciHistory{runs: map[int64]ActionRun{}, tasks: []ActionTask{{Name: "advisories", RunNumber: 9, Status: "failure"}, {Name: "advisories", RunNumber: 3, Status: "failure"}}}
		for _, r := range runs {
			r.PrettyRef, r.Created = "main", now
			h.runs[r.Index] = r
		}
		return h
	}
	mains := func(h *ciHistory) CIMain {
		d := &CIDoctor{}
		snap := &ciSnapshot{repos: []*ciRepoData{{ref: ciRepoRef{forge: &Forge{ID: 1}, repo: "o/a"}, hist: h}}}
		doctorMains(d, snap, now, func(int64, string) (string, bool) { return "o/a", true })
		if len(d.Mains) != 1 {
			t.Fatalf("mains = %+v", d.Mains)
		}
		return d.Mains[0]
	}
	m := mains(hist(ActionRun{Index: 7, Event: "push", Status: "success"}, ActionRun{Index: 9, Event: "schedule", Status: "failure", HTMLURL: "https://x/9"}))
	if m.Run != 7 || m.Status != "success" || m.SchedAdvRun != 9 {
		t.Fatalf("scheduled run after a green push: %+v", m)
	}
	if m := mains(hist(ActionRun{Index: 7, Event: "push", Status: "success"}, ActionRun{Index: 9, Event: "schedule", Status: "running"})); m.SchedAdvRun != 0 {
		t.Errorf("a running scheduled run: %+v", m)
	}
	if m := mains(hist(ActionRun{Index: 10, Event: "push", Status: "success"}, ActionRun{Index: 9, Event: "schedule", Status: "failure"})); m.SchedAdvRun != 0 {
		t.Errorf("a scheduled run older than the push: %+v", m)
	}
	if m := mains(hist(ActionRun{Index: 2, Event: "push", Status: "success"}, ActionRun{Index: 4, Event: "workflow_dispatch", Status: "failure"})); m.SchedAdvRun != 0 {
		t.Errorf("a run whose advisories job passed: %+v", m)
	}
	d := &CIDoctor{Mains: []CIMain{{Repo: "o/a", Branch: "main", Run: 7, Status: "success", SchedAdvRun: 9, SchedAdvEvent: "schedule", SchedAdvURL: "https://x/9"}}}
	doctorFindings(d, &CIBoard{}, now)
	if len(d.Findings) != 1 || d.Findings[0].Kind != "main_advisories" || d.Findings[0].Title != "o/a main has known advisories (run #9)" ||
		!strings.Contains(d.Findings[0].Detail, "Run #9 (schedule) found it.") || d.Findings[0].URL != "https://x/9" {
		t.Fatalf("findings = %+v", d.Findings)
	}
	// A red push beside it is still main_red.
	d = &CIDoctor{Mains: []CIMain{{Repo: "o/a", Branch: "main", Run: 7, Status: "failure", SchedAdvRun: 9}}}
	doctorFindings(d, &CIBoard{}, now)
	if len(d.Findings) != 2 {
		t.Fatalf("findings = %+v", d.Findings)
	}
}

// The advisories job the doctor, the base-fix rule and the runner-fault
// re-runs know by name is the one ci.yml has (WP-1660).
func TestAdvisoriesJobIsInWorkflow(t *testing.T) {
	src, err := os.ReadFile("../../.forgejo/workflows/ci.yml")
	if os.IsNotExist(err) {
		t.Skip("the mirror's checkout has no .forgejo/ (WP-1624)")
	}
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "\n  "+advisoriesJob+":\n") {
		t.Errorf("ci.yml has no job %q: advisoriesJob and the workflow disagree", advisoriesJob)
	}
}

// WP-1707: the stacks_failing title counts what its trigger counts (failed and
// conflicts), not the cancelled ones, which StackFates breaks down by reason.
func TestCIDoctorStacksFailingTitleCount(t *testing.T) {
	be, pr := armedBabysit(t)
	now := be.clockNow()
	n := 0
	add := func(state, reason string) {
		t.Helper()
		n++
		if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO queue_stacks(forge_id, repo, base_ref, pr_id, base_sha, branch, commit_sha, state, note, run_url, created_at, ended_at, end_reason)
			VALUES (?,?,?,?,'b','wp-queue/main/1','c',?,'','',?,?,?)`,
			pr.ForgeID, pr.Repo, pr.BaseRef, pr.ID, state, now.Add(-time.Duration(n)*10*time.Minute).UnixMilli(), now.UnixMilli(), reason); err != nil {
			t.Fatal(err)
		}
	}
	add("failed", endFailed)
	add("failed", endFailed)
	add(stackConflict, "")
	for range 3 {
		add("cancelled", endBaseRed)
	}
	for range 2 {
		add("cancelled", endBaseMoved)
	}
	d, err := be.s.CIDoctor(be.ctx, be.admin)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(d.Findings, func(f CIFinding) bool { return f.Kind == "stacks_failing" })
	if i < 0 {
		t.Fatalf("no stacks_failing finding: %+v", d.Findings)
	}
	title := d.Findings[i].Title
	if !strings.Contains(title, ": 3 candidates failed and none passed in the last ") {
		t.Errorf("title = %q", title)
	}
	if !strings.Contains(title, "5 cancelled (3 base red, 2 base moved)") {
		t.Errorf("title lacks the cancelled breakdown: %q", title)
	}
}
