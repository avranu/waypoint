package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// timeoutLog is a backend job stopped by go test's -timeout.
const timeoutLog = "=== RUN   TestSyncRefreshesMergeable\n" +
	"panic: test timed out after 30m0s\n\trunning tests:\n\t\tTestSyncRefreshesMergeable (4s)\n\ngoroutine 1 [running]:\n" +
	"FAIL\tgit.example.com/me/app/internal/core\t1800.208s\nFAIL\n"

// otherTimeout records a backend job of sha (on ref) that go test's
// -timeout stopped ago before now.
func (be *bsEnv) otherTimeout(task int64, sha, ref string, ago time.Duration) {
	be.t.Helper()
	end := be.clockNow().Add(-ago).UnixMilli()
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO ci_jobs(forge_id, repo, task_id, run_number, workflow, name, event, ref, sha, tier, status, started_at, ended_at)
		VALUES (?, 'me/app', ?, ?, 'ci.yml', 'backend', 'push', ?, ?, '', 'failure', ?, ?)`, be.f.ID, task, task, ref, sha, end-31*60_000, end); err != nil {
		be.t.Fatal(err)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO ci_job_packages(forge_id, repo, task_id, package, outcome, ms, limit_ms, running, at)
		VALUES (?, 'me/app', ?, 'internal/core', 'timeout', 1800200, 1800000, 'TestSyncRefreshesMergeable', ?)`, be.f.ID, task, end); err != nil {
		be.t.Fatal(err)
	}
}

// timeoutAtFront queues #7 and #8 and times out #7's full run in the
// backend job, as it timed out lately on the commits others names.
func timeoutAtFront(t *testing.T, others func(be *bsEnv)) (be *bsEnv, head7, head8 string) {
	t.Helper()
	be = newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.sg.noRetry = true
	be.runnerOnline()
	head7, head8 = "7777777aaaa", "8888888cccc"
	be.queuedDraft(7, "add-export", head7)
	be.queuedDraft(8, "add-import", head8)
	be.draftChecks(head7, CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
	be.draftChecks(head8, CommitCheck{State: CheckSuccess, ID: 7, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 8, Description: "Has been skipped"})
	if pr := be.pr(7); pr.ReadyAt == nil || pr.QueuedAt == nil {
		t.Fatalf("#7 isn't the front: %+v", pr)
	}
	others(be)
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE babysits SET session_id = ? WHERE pr_id = ? AND ended_at IS NULL`, be.agent.Session, be.pr(7).ID); err != nil {
		t.Fatal(err)
	}
	be.sg.sm.Lock()
	be.sg.logs["ci / backend (pull_request)"] = timeoutLog
	be.sg.sm.Unlock()
	be.draftChecks(head7, CommitCheck{State: CheckFailed, ID: 9, Description: "Failing after 31m"}, CommitCheck{State: CheckSuccess, ID: 10, Description: "Successful in 18m"})
	return be, head7, head8
}

func TestSharedTimeoutGivesUpTheFront(t *testing.T) {
	be, head7, head8 := timeoutAtFront(t, func(be *bsEnv) {
		be.otherTimeout(100, "d145047aaaa", "main", 2*time.Hour)
		be.otherTimeout(101, "5555555bbbb", "#5", time.Hour)
		be.otherTimeout(102, "7777777zzzz", "#7", time.Hour)
		be.otherTimeout(103, "6666666dddd", "wp-queue/main/7-8", time.Hour)
	})
	b := be.babysitOf(7)
	if b.State != BabysitWatching || b.Round != 0 || len(be.jobs()) != 0 ||
		!strings.Contains(b.Wait, "go test timed out in ci / backend (pull_request), as it did lately on #5 5555555 and main d145047: the suite is slow, not this change") ||
		!strings.Contains(b.Wait, "out of the merge line until main moves on") {
		t.Fatalf("#7 after a shared timeout: %+v jobs %v", b, be.jobs())
	}
	if be.flakeRerunSince(head7, 0) {
		t.Error("re-ran a shared timeout as a flake")
	}
	if pr := be.pr(7); pr.QueuedAt != nil {
		t.Fatalf("#7 still holds the line: %+v", pr)
	}
	if pr := be.pr(8); pr.ReadyAt == nil || pr.QueuedAt == nil {
		t.Fatalf("#8 didn't get the front: %+v %q", pr, be.babysitOf(8).Wait)
	}
	key, prio, text := be.limitTask()
	if key == "" || prio != "3" || !strings.Contains(text, "`me/app#7` held by it: `ci / backend (pull_request)` timed out on 7777777") ||
		!strings.Contains(text, "Problem: `internal/core in backend") {
		t.Fatalf("task %s %s:\n%s", key, prio, text)
	}
	if !strings.Contains(b.Wait, "("+key+" tracks the slow suite)") {
		t.Errorf("wait doesn't name %s: %q", key, b.Wait)
	}
	told := be.told()
	if m := lastOf(told); !strings.Contains(m, "Waypoint starts no round for it") || !strings.Contains(m, "don't push to re-run it") || !strings.Contains(m, key) ||
		!strings.Contains(m, "timed out in `ci / backend (pull_request)`, as it did lately on `#5 5555555 and main d145047`") {
		t.Errorf("told %q", m)
	}
	be.step()
	if _, _, again := be.limitTask(); again != text || len(be.told()) != len(told) || be.pr(7).QueuedAt != nil {
		t.Fatalf("held twice:\n%s", again)
	}

	be.sg.baseTip = "tip-2"
	be.sg.behind = map[string]bool{head7: true}
	be.step()
	if pr := be.pr(7); pr.QueuedAt != nil {
		t.Fatalf("#7 back in the line ahead of #8's full run: %+v", pr)
	}
	if w := be.babysitOf(7).Wait; !strings.Contains(w, "main moved on, so it is tried again on it at its turn, behind #8") {
		t.Fatalf("#7 waits %q", w)
	}
	if w := be.babysitOf(8).Wait; strings.Contains(w, "behind #7") {
		t.Fatalf("#8 waits %q", w)
	}
	if u := be.sg.updatedPRs(); len(u) != 0 {
		t.Fatalf("updated %v behind #8", u)
	}

	be.draftChecks(head8, CommitCheck{State: CheckSuccess, ID: 11, Description: "Successful in 16m"}, CommitCheck{State: CheckSuccess, ID: 12, Description: "Successful in 18m"})
	if pr := be.pr(8); pr.State != "merged" {
		t.Fatalf("#8: %+v (%q)", pr, be.babysitOf(8).Wait)
	}
	be.step()
	if u := be.sg.updatedPRs(); len(u) != 1 || u[0] != 7 {
		t.Fatalf("updates %v", u)
	}
	if w := be.babysitOf(7).Wait; !strings.Contains(w, "tried again on the new main: merged main into it") || strings.Contains(w, "front of the merge queue") {
		t.Fatalf("#7 waits %q", w)
	}
	if st := be.s.sharedTimeoutState(be.ctx, be.babysitOf(7)); st.Tries != 1 || st.Retry || st.Tip != "tip-2" {
		t.Fatalf("state %+v", st)
	}
}

// Behind its base when first held, it stays held: the base hasn't moved
// since the hold.
func TestSharedTimeoutBehindAtFirstHold(t *testing.T) {
	be, _, _ := timeoutAtFront(t, func(be *bsEnv) {
		be.otherTimeout(100, "d145047aaaa", "main", 2*time.Hour)
		be.otherTimeout(101, "5555555bbbb", "#5", time.Hour)
		be.sg.baseTip = "tip-1"
		be.sg.behind = map[string]bool{"7777777aaaa": true}
	})
	b := be.babysitOf(7)
	if !strings.Contains(b.Wait, "out of the merge line until main moves on") || strings.Contains(b.Wait, "moved on") {
		t.Fatalf("#7 behind at its first hold: %q", b.Wait)
	}
	if st := be.s.sharedTimeoutState(be.ctx, b); st.Tip != "tip-1" || st.Retry || st.Tries != 0 {
		t.Fatalf("state %+v", st)
	}
	be.step()
	if pr := be.pr(7); pr.QueuedAt != nil || len(be.sg.updatedPRs()) != 0 {
		t.Fatalf("#7 rejoined on a base that hasn't moved: %+v %q", pr, be.babysitOf(7).Wait)
	}
}

// With fewer than sharedTimeoutOthers other commits timed out alike, a
// timeout goes the usual way.
func TestSharedTimeoutNeedsOthers(t *testing.T) {
	be, _, _ := timeoutAtFront(t, func(be *bsEnv) {
		be.otherTimeout(100, "d145047aaaa", "main", 2*time.Hour)
		be.otherTimeout(101, "5555555bbbb", "#5", 7*time.Hour)
		be.otherTimeout(102, "7777777zzzz", "#7", time.Hour)
	})
	b := be.babysitOf(7)
	if strings.Contains(b.Wait, "the suite is slow") {
		t.Fatalf("held on one other: %+v", b)
	}
	if key, _, _ := be.limitTask(); key != "" {
		t.Fatalf("filed %s", key)
	}
}

// After sharedTimeoutRetries tries on a moved base, its timeouts go the
// usual way: they may be its own after all.
func TestSharedTimeoutRetriesEnd(t *testing.T) {
	be, _, _ := timeoutAtFront(t, func(be *bsEnv) {
		be.otherTimeout(100, "d145047aaaa", "main", 2*time.Hour)
		be.otherTimeout(101, "5555555bbbb", "#5", time.Hour)
	})
	b := be.babysitOf(7)
	if !strings.Contains(b.Wait, "the suite is slow") {
		t.Fatalf("not held: %+v", b)
	}
	be.s.saveSharedTimeout(be.ctx, b, sharedTimeoutState{SHA: be.pr(7).HeadSHA, Tip: "tip-1", Tries: sharedTimeoutRetries})
	be.step()
	if b := be.babysitOf(7); strings.Contains(b.Wait, "the suite is slow") {
		t.Fatalf("still held after %d tries: %+v", sharedTimeoutRetries, b)
	}
}

// A failure that isn't only timeouts is the change's.
func TestSharedTimeoutOnlyTimeouts(t *testing.T) {
	be, _, _ := timeoutAtFront(t, func(be *bsEnv) {
		be.otherTimeout(100, "d145047aaaa", "main", 2*time.Hour)
		be.otherTimeout(101, "5555555bbbb", "#5", time.Hour)
		be.sg.sm.Lock()
		be.sg.logs["ci / e2e (pull_request)"] = "--- FAIL: TestExport (0.01s)\n    export_test.go:12: got 1, want 2\nFAIL\tapp\n"
		be.sg.sm.Unlock()
	})
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE ci_checks SET state = 'failed', log = ? WHERE name = 'ci / e2e (pull_request)'`,
		"--- FAIL: TestExport (0.01s)\nFAIL\tapp\n"); err != nil {
		t.Fatal(err)
	}
	if ev := be.s.sharedTimeoutOf(be.ctx, be.pr(7)); ev != nil {
		t.Fatalf("a code failure with the timeout taken as shared: %+v", ev)
	}
}

// A test that failed in the job that timed out is the change's too.
func TestSharedTimeoutMixedLog(t *testing.T) {
	be, _, _ := timeoutAtFront(t, func(be *bsEnv) {
		be.otherTimeout(100, "d145047aaaa", "main", 2*time.Hour)
		be.otherTimeout(101, "5555555bbbb", "#5", time.Hour)
	})
	mixed := "--- FAIL: TestRealBug (0.01s)\nFAIL\tgit.example.com/me/app/internal/api\t12.0s\n" + timeoutLog
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE ci_checks SET log = ? WHERE name = 'ci / backend (pull_request)'`, mixed); err != nil {
		t.Fatal(err)
	}
	if ev := be.s.sharedTimeoutOf(be.ctx, be.pr(7)); ev != nil {
		t.Fatalf("a failed test beside the timeout taken as shared: %+v", ev)
	}
}

func TestOnlyGoTimeouts(t *testing.T) {
	for log, want := range map[string]bool{
		timeoutLog: true,
		"ok  \tgit.example.com/me/app/internal/api\t12.0s\n" + timeoutLog:                          true,
		"--- FAIL: TestRealBug (0.01s)\n" + timeoutLog:                                             false,
		"FAIL\tgit.example.com/me/app/internal/api\t12.0s\n" + timeoutLog:                          false,
		"FAIL\tgit.example.com/me/app/internal/api [build failed]\n" + timeoutLog:                  false,
		"FAIL\tgit.example.com/me/app/internal/core\t2.0s\nFAIL\n":                                 false,
		"panic: test timed out after 30m0s\nFAIL\tgit.example.com/me/app/internal/core\t20.000s\n": false,
	} {
		if got := onlyGoTimeouts(cleanLog(log)); got != want {
			t.Errorf("onlyGoTimeouts(%q) = %v, want %v", log, got, want)
		}
	}
}

func TestCheckJobName(t *testing.T) {
	if a, b := evidenceKind(checkJobName("ci / backend shard 1 (pull_request)")), evidenceKind("backend shard 3"); a != b {
		t.Errorf("shard kinds differ: %q, %q", a, b)
	}
	for in, want := range map[string]string{
		"ci / backend shard 1 (pull_request)": "backend shard 1",
		"ci / e2e (push)":                     "e2e",
		"backend":                             "backend",
		"test (1, linux)":                     "test",
	} {
		if got := checkJobName(in); got != want {
			t.Errorf("checkJobName(%q) = %q, want %q", in, got, want)
		}
	}
}

// A check read as failed-then-stopped counts its job as stopping itself in
// the doctor: the check's name is the forge job's ("e2e"), not the status
// context's ("ci / e2e").
func TestSelfStoppedFromChecks(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	res, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO ci_heads(forge_id, repo, sha, state, seen_at) VALUES (?, 'me/app', 'abc1234', 'failed', ?)`, be.f.ID, be.clockNow().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	head, _ := res.LastInsertId()
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO ci_checks(head_id, name, state, description, updated_at) VALUES (?, 'ci / e2e (pull_request)', 'failed', ?, ?)`,
		head, failedThenStopped, be.clockNow().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if self := be.s.selfStoppedJobs(be.ctx, be.clockNow().Add(-time.Hour)); !self[selfStoppedKey(be.f.ID, "me/app", "abc1234", "e2e")] {
		t.Fatalf("not self-stopped: %v", self)
	}
}

// The merge line can't be read when the base moves on: the held pull request
// isn't taken for the front, so its branch isn't brought up to date ahead of
// #8 (audit 1520.3).
func TestSharedTimeoutRetryWaitsOnAnUnreadableLine(t *testing.T) {
	be, head7, _ := timeoutAtFront(t, func(be *bsEnv) {
		be.otherTimeout(100, "d145047aaaa", "main", 2*time.Hour)
		be.otherTimeout(101, "5555555bbbb", "#5", time.Hour)
	})
	if pr := be.pr(7); pr.QueuedAt != nil {
		t.Fatalf("#7 still holds the line: %+v", pr)
	}
	be.sg.baseTip = "tip-2"
	be.sg.behind = map[string]bool{head7: true}
	restore := be.breakTable("branch_activity")
	be.step()
	restore()
	if u := be.sg.updatedPRs(); len(u) != 0 {
		t.Fatalf("updated %v without its place in the line", u)
	}
	if w := be.babysitOf(7).Wait; !strings.Contains(w, "couldn't be read") {
		t.Fatalf("#7 waits %q", w)
	}
	if st := be.s.sharedTimeoutState(be.ctx, be.babysitOf(7)); st.Tries != 0 {
		t.Fatalf("a try counted: %+v", st)
	}
}

// A held pull request takes its place in the line by when it was armed, as if
// it were in it: a member armed after it is behind it, one armed before it is
// ahead (WP-1700).
func TestSharedTimeoutRetryTakesItsPlaceByArming(t *testing.T) {
	for _, tc := range []struct {
		name    string
		armed8  int64
		updated bool
		waits   string
	}{
		{"member armed after", 2000, true, ""},
		{"member armed before", 500, false, "behind #8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be, head7, _ := timeoutAtFront(t, func(be *bsEnv) {
				be.otherTimeout(100, "d145047aaaa", "main", 2*time.Hour)
				be.otherTimeout(101, "5555555bbbb", "#5", time.Hour)
			})
			if pr := be.pr(7); pr.QueuedAt != nil {
				t.Fatalf("#7 still holds the line: %+v", pr)
			}
			for _, q := range []struct {
				sql string
				id  int64
			}{
				{`UPDATE pull_requests SET armed_at = 1000 WHERE id = ?`, be.pr(7).ID},
				{`UPDATE pull_requests SET ready_at = NULL, wip_auto = 0, armed_at = ` + fmt.Sprint(tc.armed8) + ` WHERE id = ?`, be.pr(8).ID},
			} {
				if _, err := be.s.db.W.ExecContext(be.ctx, q.sql, q.id); err != nil {
					t.Fatal(err)
				}
			}
			be.sg.baseTip = "tip-2"
			be.sg.behind = map[string]bool{head7: true}
			be.step()
			u := be.sg.updatedPRs()
			if tc.updated {
				if len(u) != 1 || u[0] != 7 {
					t.Fatalf("updates %v, wait %q", u, be.babysitOf(7).Wait)
				}
				return
			}
			if len(u) != 0 || !strings.Contains(be.babysitOf(7).Wait, tc.waits) {
				t.Fatalf("updates %v, wait %q", u, be.babysitOf(7).Wait)
			}
		})
	}
}
