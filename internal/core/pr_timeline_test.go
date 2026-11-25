package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// timelineEnv opens pull requests 7 (heads 7777777aaaa, then 8888888bbbb) and
// 8 (its own head) so a timeline has another pull request's rows to leave out.
func timelineEnv(t *testing.T) (*shipEnv, *PullRequest, int64) {
	t.Helper()
	se := newShipEnv(t, "forgejo")
	se.pullHook("opened", ForgePR{Number: 7, Title: "Add export", Author: "sam", HeadRef: "add-export", HeadSHA: "7777777aaaa", State: "open"})
	se.pullHook("opened", ForgePR{Number: 8, Title: "Other", Author: "sam", HeadRef: "other", HeadSHA: "9999999cccc", State: "open"})
	se.s.Wait()
	pr := se.pr(7)
	t0 := se.clockNow().Add(-5 * time.Hour).UnixMilli()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := se.s.db.W.ExecContext(se.ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE pull_requests SET opened_at = ? WHERE id = ?`, t0, pr.ID)
	exec(`DELETE FROM pr_events WHERE pr_id = ? AND kind = 'pushed'`, pr.ID)
	exec(`DELETE FROM merge_holds WHERE pr_id = ?`, pr.ID) // the holds opening it made
	exec(`INSERT INTO pr_events(pr_id, kind, actor, sha, created_at) VALUES (?, 'pushed', 'sam', '8888888bbbb', ?)`, pr.ID, t0+60*60000)
	return se, pr, t0
}

func (se *shipEnv) timelineRun(number int, sha, ref, event, status string, created int64, started, stopped any) {
	se.t.Helper()
	if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO ci_runs(forge_id, repo, number, workflow, event, ref, sha, tier, status, created_at, started_at, stopped_at)
		VALUES (?, 'me/app', ?, 'ci.yml', ?, ?, ?, '', ?, ?, ?, ?)`, se.f.ID, number, event, ref, sha, status, created, started, stopped); err != nil {
		se.t.Fatal(err)
	}
}

func TestPRTimelineRuns(t *testing.T) {
	se, _, t0 := timelineEnv(t)
	min := func(m int) int64 { return t0 + int64(m)*60000 }
	se.timelineRun(1, "7777777aaaa", "#7", "pull_request", "failure", min(1), min(4), min(19))
	se.timelineRun(2, "8888888bbbb", "#7", "pull_request", "success", min(62), min(63), min(80))
	se.timelineRun(3, "9999999cccc", "#8", "pull_request", "success", min(2), min(3), min(9))
	se.timelineRun(4, "ffffffffff", "refs/pull/7/head", "pull_request", "cancelled", min(70), nil, nil)
	se.timelineRun(5, "7777777aaaa", "refs/heads/add-export", "push", "success", min(1), min(2), min(3))

	tl, err := se.s.PRTimeline(se.ctx, se.admin, se.p.Key, "", 7)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range tl.Runs {
		got = append(got, fmt.Sprint(r.Number))
	}
	if strings.Join(got, ",") != "1,2,4" {
		t.Fatalf("runs = %v, want 1,2,4 (not another pull request's, not a branch push)", got)
	}
	r1, r4 := tl.Runs[0], tl.Runs[2]
	if r1.WaitS == nil || *r1.WaitS != 180 || r1.RunS == nil || *r1.RunS != 900 || r1.Head != "7777777" {
		t.Errorf("run 1 = %+v", r1)
	}
	if r4.WaitS != nil || r4.RunS != nil || r4.StartedAt != nil || r4.StoppedAt != nil {
		t.Errorf("run 4 never started, but has %+v", r4)
	}
	raw, _ := json.Marshal(r4)
	if strings.Contains(string(raw), "wait_s") || strings.Contains(string(raw), "started_at") {
		t.Errorf("run 4 json = %s", raw)
	}
	if tl.Totals.CIWaitS != 180+60 || tl.Totals.CIRunS != 900+1020 {
		t.Errorf("totals = %+v", tl.Totals)
	}
	if len(tl.Pushes) != 1 || tl.Pushes[0].Head != "8888888" {
		t.Errorf("pushes = %+v", tl.Pushes)
	}
}

// Only a pull request event on a head is the pull request's own: a scheduled
// or dispatched run of the default branch can sit on a commit that is one of
// its heads, and so can another repo's run of the same number.
func TestPRTimelineRunsOfOtherEventsAndRepos(t *testing.T) {
	se, _, t0 := timelineEnv(t)
	min := func(m int) int64 { return t0 + int64(m)*60000 }
	se.timelineRun(1, "8888888bbbb", "#7", "pull_request", "success", min(62), min(63), min(80))
	se.timelineRun(2, "8888888bbbb", "refs/heads/main", "schedule", "success", min(70), min(71), min(75))
	se.timelineRun(3, "8888888bbbb", "refs/heads/main", "workflow_dispatch", "success", min(72), min(73), min(76))
	se.timelineRun(4, "8888888bbbb", "refs/heads/add-export", "pull_request_target", "success", min(74), min(75), min(78))
	if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO ci_runs(forge_id, repo, number, workflow, event, ref, sha, tier, status, created_at, started_at, stopped_at)
		VALUES (?, 'other/app', 5, 'ci.yml', 'pull_request', '#7', '8888888bbbb', '', 'success', ?, ?, ?)`, se.f.ID, min(62), min(63), min(80)); err != nil {
		t.Fatal(err)
	}
	tl, err := se.s.PRTimeline(se.ctx, se.admin, se.p.Key, "", 7)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range tl.Runs {
		got = append(got, fmt.Sprint(r.Number))
	}
	if strings.Join(got, ",") != "1,4" {
		t.Errorf("runs = %v, want 1,4 (no schedule or dispatch run, no other repo's)", got)
	}
}

// A merge before the open time, from clocks that differ, is not a negative time.
func TestPRTimelineOpenToMergedNotNegative(t *testing.T) {
	se, pr, t0 := timelineEnv(t)
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET state = 'merged', merged_at = ? WHERE id = ?`, t0-60000, pr.ID); err != nil {
		t.Fatal(err)
	}
	tl, err := se.s.PRTimeline(se.ctx, se.admin, se.p.Key, "", 7)
	if err != nil {
		t.Fatal(err)
	}
	if tl.Totals.OpenToMergedS == nil || *tl.Totals.OpenToMergedS != 0 {
		t.Errorf("open_to_merged_s = %v, want 0", tl.Totals.OpenToMergedS)
	}
}

// The merge queue's run of a stack the pull request rode is one of its runs,
// though it is a push to a wp-queue/ branch on a commit that is no head of it.
func TestPRTimelineStackRuns(t *testing.T) {
	se, pr, t0 := timelineEnv(t)
	min := func(m int) int64 { return t0 + int64(m)*60000 }
	stack := func(prID int64, branch, sha string) {
		t.Helper()
		if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO queue_stacks(forge_id, repo, base_ref, pr_id, base_sha, branch, commit_sha, created_at) VALUES (?, 'me/app', 'main', ?, 'base', ?, ?, ?)`,
			se.f.ID, prID, branch, sha, min(90)); err != nil {
			t.Fatal(err)
		}
	}
	stack(pr.ID, "wp-queue/main/7-abc", "aaaaaaa1111")
	stack(se.pr(8).ID, "wp-queue/main/8-abd", "bbbbbbb2222")
	se.timelineRun(1, "8888888bbbb", "#7", "pull_request", "success", min(62), min(63), min(80))
	se.timelineRun(2, "aaaaaaa1111", "wp-queue/main/7-abc", "push", "success", min(91), min(93), min(113))
	se.timelineRun(3, "bbbbbbb2222", "wp-queue/main/8-abd", "push", "success", min(91), min(92), min(99))

	tl, err := se.s.PRTimeline(se.ctx, se.admin, se.p.Key, "", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Runs) != 2 || tl.Runs[0].Number != 1 || tl.Runs[0].Stack || tl.Runs[1].Number != 2 || !tl.Runs[1].Stack {
		t.Fatalf("runs = %+v, want #1 and the stack run #2", tl.Runs)
	}
	if tl.Totals.CIWaitS != 60+120 || tl.Totals.CIRunS != 1020+1200 {
		t.Errorf("totals = %+v", tl.Totals)
	}

	// A batch's row is its last pull request's; the others are in its heads.
	if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO queue_stacks(forge_id, repo, base_ref, pr_id, base_sha, heads, branch, commit_sha, created_at) VALUES (?, 'me/app', 'main', ?, 'base', ?, 'wp-queue/main/8-abe', 'ccccccc3333', ?)`,
		se.f.ID, se.pr(8).ID, fmt.Sprintf(`[{"pr_id":%d,"number":7,"sha":"8888888bbbb"},{"pr_id":%d,"number":8,"sha":"9999999cccc"}]`, pr.ID, se.pr(8).ID), min(100)); err != nil {
		t.Fatal(err)
	}
	se.timelineRun(4, "ccccccc3333", "wp-queue/main/8-abe", "push", "success", min(101), min(102), min(110))
	if tl, err = se.s.PRTimeline(se.ctx, se.admin, se.p.Key, "", 7); err != nil {
		t.Fatal(err)
	}
	if len(tl.Runs) != 3 || tl.Runs[2].Number != 4 || !tl.Runs[2].Stack {
		t.Errorf("runs = %+v, want the batch's run #4 too", tl.Runs)
	}
}

// Runs and audit rows are read by the pull request's open time, so a run on
// one of its heads long after it merged (a scheduled one, say) is not its.
func TestPRTimelineLeavesOutRunsAfterItEnded(t *testing.T) {
	se, pr, t0 := timelineEnv(t)
	min := func(m int) int64 { return t0 + int64(m)*60000 }
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET state = 'merged', merged_at = ? WHERE id = ?`, min(100), pr.ID); err != nil {
		t.Fatal(err)
	}
	se.timelineRun(1, "8888888bbbb", "#7", "pull_request", "success", min(62), min(63), min(80))
	se.timelineRun(2, "8888888bbbb", "refs/heads/main", "schedule", "success", min(100+61), min(100+62), min(100+70))
	tl, err := se.s.PRTimeline(se.ctx, se.admin, se.p.Key, "", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Runs) != 1 || tl.Runs[0].Number != 1 || tl.State != "merged" {
		t.Errorf("state %q, runs = %+v, want only #1", tl.State, tl.Runs)
	}
}

func TestPRTimelineHolds(t *testing.T) {
	se, pr, t0 := timelineEnv(t)
	min := func(m int) int64 { return t0 + int64(m)*60000 }
	head := "7777777aaaa"
	add := func(what string, from, until any) {
		t.Helper()
		if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO merge_holds(pr_id, head_sha, what, first_at, cleared_at) VALUES (?, ?, ?, ?, ?)`, pr.ID, head, what, from, until); err != nil {
			t.Fatal(err)
		}
	}
	// Its CI went green at 15m: a hold is every moment it was armed and not
	// ready, so what came before is the wait for CI, not a hold after green.
	se.timelineRun(1, head, "#7", "pull_request", "success", min(1), min(2), min(15))
	for i := 0; i < 9; i++ {
		add(fmt.Sprintf("short %d", i), min(100+i), min(100+i)+30000) // under a minute each
	}
	add("a", min(10), min(30))
	add("b", min(20), min(40)) // overlaps a: 15..40 after green is 25m
	now := se.clockNow().UnixMilli()
	add("open", now-10*60000, nil) // counts to now: 10m
	head = "8888888bbbb"
	add("never green", min(50), min(60)) // its head had no passing run

	tl, err := se.s.PRTimeline(se.ctx, se.admin, se.p.Key, "", 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Holds) != 13 {
		t.Fatalf("%d holds, want all 13", len(tl.Holds))
	}
	if a := tl.Holds[0]; a.AfterGreenS != 15*60 {
		t.Errorf("a is %ds after green, want 15m", a.AfterGreenS)
	}
	for _, h := range tl.Holds {
		if h.What == "never green" && h.AfterGreenS != 0 {
			t.Errorf("a hold on a head that never passed counts %ds", h.AfterGreenS)
		}
	}
	if tl.Holds[0].What != "a" || tl.Holds[len(tl.Holds)-1].What != "open" || tl.Holds[len(tl.Holds)-1].Until != nil {
		t.Errorf("order or open end wrong: %+v", tl.Holds)
	}
	if want := int64(25*60 + 9*30 + 10*60); tl.Totals.HeldS != want {
		t.Errorf("held_s = %d, want %d", tl.Totals.HeldS, want)
	}
}

// A hold is cleared only while the pull request is armed and open, so one
// left open must not keep counting after the merge, the close or the disarm.
func TestPRTimelineOpenHoldEndsWithThePullRequest(t *testing.T) {
	for _, c := range []struct{ name, sql, state string }{
		{"merged", `UPDATE pull_requests SET state = 'merged', merged_at = %d WHERE id = ?`, "merged"},
		{"closed", `UPDATE pull_requests SET state = 'closed', closed_at = %d, armed_at = NULL WHERE id = ?`, "closed"},
		// Disarmed at 75m and armed again at 76m, it closed at 100m: the earlier disarm ends nothing.
		{"rearmed", `INSERT INTO audit_log(action, category, target_type, target_id, created_at) VALUES ('pr.disarm', 'control', 'pr', ?1, %[1]d - 25*60000), ('pr.arm', 'approval', 'pr', ?1, %[1]d - 24*60000)`, "closed"},
		{"disarmed", `INSERT INTO audit_log(action, category, target_type, target_id, created_at) VALUES ('pr.disarm', 'control', 'pr', ?, %d)`, "open"},
	} {
		t.Run(c.name, func(t *testing.T) {
			se, pr, t0 := timelineEnv(t)
			min := func(m int) int64 { return t0 + int64(m)*60000 }
			se.timelineRun(1, "8888888bbbb", "#7", "pull_request", "success", min(62), min(63), min(80))
			// Opening it armed it by the repo's setting, at the test's now; each case lists its own rows.
			if _, err := se.s.db.W.ExecContext(se.ctx, `DELETE FROM audit_log WHERE target_type = 'pr' AND target_id = ?`, pr.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO merge_holds(pr_id, head_sha, what, first_at) VALUES (?, '8888888bbbb', 'behind main', ?)`, pr.ID, min(70)); err != nil {
				t.Fatal(err)
			}
			if _, err := se.s.db.W.ExecContext(se.ctx, fmt.Sprintf(c.sql, min(100)), pr.ID); err != nil {
				t.Fatal(err)
			}
			after := map[string]string{"disarmed": `UPDATE pull_requests SET armed_at = NULL WHERE id = ?`,
				"rearmed": fmt.Sprintf(`UPDATE pull_requests SET state = 'closed', closed_at = %d, armed_at = NULL WHERE id = ?`, min(100))}[c.name]
			if after != "" {
				if _, err := se.s.db.W.ExecContext(se.ctx, after, pr.ID); err != nil {
					t.Fatal(err)
				}
			}
			tl, err := se.s.PRTimeline(se.ctx, se.admin, se.p.Key, "", 7)
			if err != nil {
				t.Fatal(err)
			}
			if tl.State != c.state || tl.Totals.HeldS != 20*60 { // green at 80m, over at 100m
				t.Errorf("state %q, held_s = %d, want %s and 20m", tl.State, tl.Totals.HeldS, c.state)
			}
			if h := tl.Holds[0]; h.Until == nil || !h.Until.Equal(msTime(min(100))) {
				t.Errorf("the hold ends %v, want when the pull request did", h.Until)
			}
			if (tl.ClosedAt != nil) != (c.state == "closed") {
				t.Errorf("closed_at = %v", tl.ClosedAt)
			}
		})
	}
}

func TestPRTimelineArmAndMerge(t *testing.T) {
	se, pr, t0 := timelineEnv(t)
	min := func(m int) int64 { return t0 + int64(m)*60000 }
	audit := func(action string, at int64, target int64) {
		t.Helper()
		if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO audit_log(user_id, action, category, project_id, target_type, target_id, created_at) VALUES (?, ?, 'approval', ?, 'pr', ?, ?)`,
			se.admin.User.ID, action, se.p.ID, target, at); err != nil {
			t.Fatal(err)
		}
	}
	// Opening it armed it by the repo's setting; this test lists its own rows.
	if _, err := se.s.db.W.ExecContext(se.ctx, `DELETE FROM audit_log WHERE target_type = 'pr' AND target_id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	audit("pr.merge", min(270), pr.ID)
	audit("pr.arm", min(90), pr.ID)
	audit("pr.disarm", min(120), pr.ID)
	audit("pr.arm", min(130), pr.ID)
	audit("pr.arm", min(95), se.pr(8).ID)
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE pull_requests SET state = 'merged', merged_at = ? WHERE id = ?`, min(277), pr.ID); err != nil {
		t.Fatal(err)
	}
	tl, err := se.s.PRTimeline(se.ctx, se.admin, se.p.Key, "", 7)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range tl.Armed {
		got = append(got, a.What+"/"+a.By)
	}
	if want := "arm/" + se.admin.User.Username + ",disarm/" + se.admin.User.Username + ",arm/" + se.admin.User.Username + ",merge/" + se.admin.User.Username; strings.Join(got, ",") != want {
		t.Errorf("armed = %v, want %s", got, want)
	}
	if tl.MergedAt == nil || tl.Totals.OpenToMergedS == nil || *tl.Totals.OpenToMergedS != 277*60 {
		t.Errorf("merged = %v, totals = %+v", tl.MergedAt, tl.Totals)
	}
}

func TestPRTimelineRefusesWhatThePullRequestRefuses(t *testing.T) {
	se, _, _ := timelineEnv(t)
	stranger := se.user("stranger", false)
	_, err := se.s.PRTimeline(se.ctx, stranger, se.p.Key, "", 7)
	if err == nil {
		t.Fatal("a stranger read the timeline")
	}
	_, want := se.s.FindPullRequest(se.ctx, stranger, se.p.Key, "", 7)
	if !errors.Is(err, want) && err.Error() != want.Error() {
		t.Errorf("err = %v, FindPullRequest says %v", err, want)
	}
	if _, err := se.s.PRTimeline(se.ctx, nil, se.p.Key, "", 7); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("no actor: %v", err)
	}
}
