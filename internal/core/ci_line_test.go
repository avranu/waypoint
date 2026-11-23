package core

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestCILineReason(t *testing.T) {
	for _, c := range []struct{ state, note, want string }{
		{stackMerged, "merged as 3e70000", "merged on it"},
		{stackMerged, "merged outside its stack", "merged outside its stack"},
		{stackCancelled, "#7's head moved", "head moved"},
		{stackCancelled, "#9 merged at another head", "head moved"},
		{stackCancelled, "#7's base changed", "base moved"},
		{stackCancelled, "main moved past the stack's heads", "base moved"},
		{stackCancelled, "#8's checks failed", "checks failed"},
		{stackFailed, "required check ci / lint (push) didn't pass on it", "checks failed"},
		{stackConflict, "#7 (abcdef1) doesn't merge cleanly onto the base", "conflict"},
		{stackCancelled, "#7 left the merge queue", "queue changed"},
		{stackCancelled, "the queue ahead of it changed", "queue changed"},
		{stackCancelled, "the stack it builds on (#7's) ended: #7's head moved", "head moved"},
		{stackCancelled, "the batch it rides (#8's place in it) ended: #9 (abcdef1) doesn't merge cleanly onto the base", "conflict"},
		{stackFailed, "no job log of its run could be read after 120 minutes", "no CI status"},
		{stackFailed, "its run was still going after 120 minutes", "overdue/timed out"},
		{stackFailed, "no CI run started on its branch in 10 minutes; no stacks for a day", "no CI status"},
		{stackRefused, "the forge refused the merge: 405; no stacks for a day", "refused by the forge"},
		{stackCancelled, "something new", "other"},
	} {
		if got := ciLineReason(c.state, c.note); got != c.want {
			t.Errorf("%s %q = %q, want %q", c.state, c.note, got, c.want)
		}
	}
}

// The line lists its armed pull requests in order with their live stack,
// and counts the window's rows by day, state and reason, with the merges and
// those made on a stack.
func TestCILine(t *testing.T) {
	se := newStackEnv(t)
	now := se.clockNow()
	day := func(back int, h int) int64 {
		d := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, time.UTC).AddDate(0, 0, -back)
		return d.UnixMilli()
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := se.s.db.W.ExecContext(se.ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE pull_requests SET queued_at = ? WHERE id = ?`, now.Add(-time.Hour).UnixMilli(), se.pr(7).ID)
	exec(`UPDATE pull_requests SET queued_at = ? WHERE id = ?`, now.Add(-30*time.Minute).UnixMilli(), se.pr(8).ID)
	// two merged pull requests, yesterday; the first on a stack
	se.queuedDraft(10, "m-one", "aaaaaaa1")
	se.queuedDraft(11, "m-two", "bbbbbbb2")
	for n, h := range map[int]int{10: 9, 11: 10} {
		exec(`UPDATE pull_requests SET state = 'merged', merged_at = ? WHERE id = ?`, day(1, h), se.pr(n).ID)
	}
	heads := func(ns ...int) string {
		var hs []stackHead
		for _, n := range ns {
			hs = append(hs, stackHead{PRID: se.pr(n).ID, Number: n, SHA: "abc1234"})
		}
		b, _ := json.Marshal(hs)
		return string(b)
	}
	add := func(n int, pos int, hs, branch, state, note string, created int64, ended bool) {
		t.Helper()
		var end any
		if ended {
			end = created + 60000
		}
		exec(`INSERT INTO queue_stacks(forge_id, repo, base_ref, pr_id, base_sha, heads, pos, branch, commit_sha, state, note, created_at, ended_at)
			VALUES (?, ?, 'main', ?, 'tip', ?, ?, ?, ?, ?, ?, ?, ?)`, se.pr(7).ForgeID, se.pr(7).Repo, se.pr(n).ID, hs, pos, branch, branch, state, note, created, end)
	}
	add(10, -1, heads(10), "wp-queue/main/10", stackMerged, "merged as 3e70000", day(1, 8), true)
	add(11, -1, heads(10, 11), "wp-queue/main/11", stackCancelled, "#10's head moved", day(1, 8), true)
	add(11, -1, heads(11), "wp-queue/main/11b", stackFailed, "#11's checks failed", day(1, 11), true)
	add(11, -1, heads(11), "wp-queue/main/11c", stackFailed, "#12's checks failed", day(1, 12), true)
	// a batch of #7 and #8: two rows, one branch, live today
	add(7, 0, heads(7, 8), "wp-queue/main/batch", stackRunning, "", day(0, 0), false)
	add(8, 1, heads(7, 8), "wp-queue/main/batch", stackRunning, "", day(0, 0), false)
	add(9, -1, heads(7, 9), "wp-queue/main/9", stackConflict, "#9 (abcdef1) doesn't merge cleanly onto the base", day(0, 0), true)
	add(9, -1, heads(9), "wp-queue/main/old", stackCancelled, "before the window", day(9, 8), true)
	// full-tier runs of the repo: three yesterday, one a stack's own
	for i, ref := range []string{"a", "b", "c", "wp-queue/main/9"} {
		exec(`INSERT INTO ci_runs(forge_id, repo, number, workflow, event, ref, sha, title, tier, status, created_at)
			VALUES (?, ?, ?, 'ci.yml', 'push', ?, 's', 't', '', 'success', ?)`, se.pr(7).ForgeID, se.pr(7).Repo, 900+i, ref, day(1, 9))
	}

	rep, err := se.s.CILine(se.ctx, se.admin, CILineInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Lines) != 1 || rep.Days != 7 {
		t.Fatalf("lines: %+v", rep.Lines)
	}
	l := rep.Lines[0]
	var order []int
	for _, p := range l.Armed {
		order = append(order, p.Number)
	}
	if !slices.Equal(order, []int{7, 8, 9}) {
		t.Errorf("armed order %v", order)
	}
	if p := l.Armed[0]; !p.Queued || p.Babysit == "" || p.State != "draft" || p.Stack == nil || !p.Stack.Batch || p.Stack.Heads != "main + #7 + #8" || p.Stack.State != "running" {
		t.Errorf("#7: %+v stack %+v", p, p.Stack)
	}
	if l.Armed[2].Queued || l.Armed[2].Stack != nil {
		t.Errorf("#9 has a live stack: %+v", l.Armed[2].Stack)
	}
	if len(l.Days) != 7 {
		t.Fatalf("days: %d", len(l.Days))
	}
	yd, today := l.Days[5], l.Days[6]
	if yd.Rows != 4 || yd.Runs != 4 || yd.RunsUsed != 1 || yd.States[stackMerged] != 1 || yd.States[stackFailed] != 2 ||
		yd.Reasons["checks failed"] != 2 || yd.Reasons["head moved"] != 1 || yd.Reasons["merged on it"] != 1 {
		t.Errorf("yesterday: %+v", yd)
	}
	if yd.Merges != 2 || yd.MergedOnStack != 1 || yd.FullRuns == nil || *yd.FullRuns != 3 || yd.FullPerMerge == nil || *yd.FullPerMerge != 1.5 {
		t.Errorf("yesterday merges: %+v full %v", yd, yd.FullRuns)
	}
	if today.Rows != 3 || today.Runs != 2 || today.Batches != 1 || today.States[stackRunning] != 2 || today.Reasons["conflict"] != 1 {
		t.Errorf("today: %+v", today)
	}
	if t0 := l.Totals; t0.Rows != 7 || t0.Runs != 6 || t0.RunsUsed != 1 || t0.UsedShare == nil || *t0.UsedShare != 1.0/6 || t0.Merges != 2 {
		t.Errorf("totals: %+v", t0)
	}
	// the two failures differ only by a number: one group, two raw notes
	var failed *CILineReason
	for i := range l.Reasons {
		if l.Reasons[i].Reason == "checks failed" {
			failed = &l.Reasons[i]
		}
	}
	if failed == nil || failed.Count != 2 || len(failed.Examples) != 2 {
		t.Errorf("reasons: %+v", l.Reasons)
	}

	// one repo, a longer window, and who may look
	if r, err := se.s.CILine(se.ctx, se.admin, CILineInput{Repo: "nobody/else"}); err != nil || len(r.Lines) != 0 {
		t.Errorf("other repo: %+v %v", r, err)
	}
	if r, err := se.s.CILine(se.ctx, se.admin, CILineInput{Days: 14}); err != nil || r.Lines[0].Totals.Rows != 8 {
		t.Errorf("14 days: %+v %v", r, err)
	}
	if _, err := se.s.CILine(se.ctx, se.admin, CILineInput{Days: 91}); err == nil {
		t.Error("91 days accepted")
	}
	if _, err := se.s.CILine(se.ctx, nil, CILineInput{}); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("no actor: %v", err)
	}
	if r, err := se.s.CILine(se.ctx, se.user("sam", false), CILineInput{}); err != nil || len(r.Lines) != 0 {
		t.Errorf("outsider saw lines: %+v %v", r, err)
	}
}
