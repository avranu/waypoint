package core

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPRFlowReport(t *testing.T) {
	se, pr, t0 := timelineEnv(t)
	min := func(m int) int64 { return t0 + int64(m)*60000 }
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := se.s.db.W.ExecContext(se.ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE pull_requests SET state = 'merged', merged_at = ? WHERE id = ?`, min(200), pr.ID)
	exec(`UPDATE pull_requests SET opened_at = ? WHERE number = 8`, min(100))
	hold := func(what string, from, until int) {
		exec(`INSERT INTO merge_holds(pr_id, head_sha, what, first_at, cleared_at) VALUES (?, '8888888bbbb', ?, ?, ?)`, pr.ID, what, min(from), min(until))
	}
	hold("checks still running", 0, 60)
	hold("checks still running", 30, 80) // overlaps: one stage, once
	hold("still a draft", 80, 200)
	hold("a footprint check) to finish", 90, 100)
	se.timelineRun(1, "8888888bbbb", "#7", "pull_request", "success", min(62), min(63), min(80))
	se.timelineRun(2, "8888888bbbb", "#7", "pull_request", "success", min(81), min(82), min(100))

	f, err := se.s.PRFlow(se.ctx, se.admin, se.p.Key, PRFlowInput{Repo: "me/app"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Merged != 1 || f.OpenedToMerged == nil || f.OpenedToMerged.P50 != 200*60 || f.OpenedToMerged.P90 != 200*60 {
		t.Fatalf("merged %d, opened to merged %+v", f.Merged, f.OpenedToMerged)
	}
	hours := map[string]float64{}
	for _, s := range f.Stages {
		hours[s.Stage] = s.Hours
	}
	near := func(stage string, h float64) {
		t.Helper()
		if d := hours[stage] - h; d > 0.001 || d < -0.001 {
			t.Errorf("%s: %.3f h, want %.3f (%v)", stage, hours[stage], h, hours)
		}
	}
	near(flowCIRunning, 80.0/60)
	near(flowFootprint, 10.0/60)
	near(flowLine, 110.0/60)
	near(flowUnattribute, 0)
	if len(f.Groups) != 2 || f.Groups[0].Group != FlowConverging || f.Groups[1].Group != FlowIntegrating {
		t.Fatalf("groups = %+v", f.Groups)
	}
	if got := f.Groups[1].Hours; got < 110.0/60-.001 || got > 110.0/60+.001 {
		t.Errorf("integrating %.3f h", got)
	}
	if f.Watch.Heads != 2 || f.Watch.FullRuns != 2 || f.Watch.Rounds != 0 {
		t.Errorf("watch = %+v, want 2 heads (opened, one push), 2 full runs, no rounds", f.Watch)
	}
	if len(f.Daily) == 0 || f.Daily[0].Merged+f.Daily[len(f.Daily)-1].Merged < 1 {
		t.Errorf("daily = %+v", f.Daily)
	}
	if f.StillOpen.Count != 1 || f.StillOpen.Hours <= 0 {
		t.Errorf("still open = %+v: pull request 8 is open and counted apart", f.StillOpen)
	}
}

func TestPRFlowSpanBounds(t *testing.T) {
	se, _, _ := timelineEnv(t)
	for _, d := range []int{-1, prFlowMaxDays + 1} {
		if _, err := se.s.PRFlow(se.ctx, se.admin, se.p.Key, PRFlowInput{Days: d}); err == nil || !strings.Contains(err.Error(), "days") {
			t.Errorf("days %d: err = %v", d, err)
		}
	}
	if _, err := se.s.PRFlow(se.ctx, nil, se.p.Key, PRFlowInput{}); err == nil {
		t.Error("no actor: no error")
	}
	f, err := se.s.PRFlow(se.ctx, se.admin, se.p.Key, PRFlowInput{})
	if err != nil || f.Days != prFlowDefaultDays || f.Merged != 0 || f.OpenedToMerged != nil {
		t.Errorf("default span: %+v, %v", f, err)
	}
}

func TestBabysitKeepsEachWaitAndEndsRounds(t *testing.T) {
	be, job := timeoutRound(t)
	var n int
	var wait string
	if err := be.s.db.R.QueryRow(`SELECT count(*), coalesce(max(wait), '') FROM babysit_waits WHERE state = ?`, BabysitWorking).Scan(&n, &wait); err != nil || n == 0 || wait == "" {
		t.Fatalf("working waits: %d %q %v", n, wait, err)
	}
	be.finishWith(job, false, "Nothing to change.")
	r := be.babysit().Rounds[0]
	if r.EndedAt.IsZero() || r.EndedAt.Before(r.At) {
		t.Errorf("round %+v has no end, or one before its start", r)
	}
}

func TestBabysitSaysWhyAHandedRoundWaits(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	be.start(pr)
	be.ci("7777777aaaa", CheckFailed)
	if b := be.babysit(); !strings.Contains(b.Wait, "handed over") {
		t.Fatalf("wait %q", b.Wait)
	}
	be.step()
	if b := be.babysit(); !strings.Contains(b.Wait, "went to a session") || b.State != BabysitNeedsPerson {
		t.Errorf("after a sweep with nothing new: %s %q", b.State, b.Wait)
	}
}

// A pull request that joined the line, left and joined again has two stays
// recorded by the line's own code, and the classifier gives the time between
// them to converging.
func TestLineSpansRecordJoinLeaveRejoin(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.noWaits()
	const head = "1111111aaaa"
	se.sg.behind = map[string]bool{head: true} // so the line updates it instead of merging
	se.pullHook("opened", testPR)
	se.drain()
	se.sg.setChecks(head, CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	se.poll()
	id := se.pr(5).ID

	se.inLine(id)
	first := se.clockNow()
	se.setClock(first.Add(10 * time.Minute))
	pr := se.pr(5)
	se.s.lineDrop(se.ctx, lineOf(pr), pr, "its checks need another run")
	se.setClock(first.Add(30 * time.Minute))
	se.inLine(id)

	type span struct {
		from, to int64
		why      string
	}
	var got []span
	rows, err := se.s.db.R.QueryContext(se.ctx, `SELECT joined_at, coalesce(left_at, 0), why FROM merge_line_spans WHERE pr_id = ? ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var sp span
		if err := rows.Scan(&sp.from, &sp.to, &sp.why); err != nil {
			t.Fatal(err)
		}
		got = append(got, sp)
	}
	if len(got) != 2 || got[0].to == 0 || got[0].why != "its checks need another run" || got[1].to != 0 {
		t.Fatalf("spans = %+v, want one left (with the drop's reason) and one open", got)
	}
	lines := []flowLineSpan{{got[0].from, got[0].to}, {got[1].from, got[1].to}}
	out := classifyFlow(flowHistory{Opened: got[0].from, End: got[1].from + 60000, Lines: lines})
	var stages []string
	for _, sp := range out {
		stages = append(stages, sp.Stage)
	}
	want := []string{flowLine, flowWork, flowLine}
	if !slices.Equal(stages, want) || flowGroup(out[1].Stage) != FlowConverging {
		t.Fatalf("stages = %v, want %v with the gap converging", stages, want)
	}
}
