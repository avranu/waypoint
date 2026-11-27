package core

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const ciBackendPR = "ci / backend (pull_request)"

// held waits until a wp pr wait is held on the pull request.
func (se *shipEnv) held(prID int64) {
	se.t.Helper()
	for range 500 {
		se.s.prWait.mu.Lock()
		n := len(se.s.prWait.m[prID])
		se.s.prWait.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	se.t.Fatal("no wait held")
}

// checks sets the head's checks on the forge and sends its status webhook.
func (se *shipEnv) checks(sha string, checks ...CommitCheck) {
	se.sg.setChecks(sha, checks...)
	se.s.onForgeCI(se.ctx, se.f, "me/app", sha)
	se.s.Wait()
}

func TestWaitPullRequest(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.policy(MergePolicyInput{AutoMerge: ptr(false)})
	head := testPR.HeadSHA
	se.sg.setChecks(head, CommitCheck{Name: ciBackendPR, State: CheckPending})
	se.pullHook("opened", testPR)
	pr := se.pr(5)

	start := time.Now()
	w, err := se.s.WaitPullRequest(se.ctx, se.admin, pr.ID, PRWaitInput{Timeout: 150 * time.Millisecond})
	if err != nil || w.Verdict != PRWaitWaiting || w.Why != "1 check still running" || w.CI != CIPending || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("timed out: %+v %v", w, err)
	}

	type result struct {
		w   *PRWait
		err error
	}
	done := make(chan result, 1)
	go func() {
		w, err := se.s.WaitPullRequest(se.ctx, se.admin, pr.ID, PRWaitInput{Timeout: time.Minute})
		done <- result{w, err}
	}()
	se.held(pr.ID)
	start = time.Now()
	se.checks(head, CommitCheck{Name: ciBackendPR, State: CheckFailed, URL: "/me/app/actions/runs/3/jobs/0"}, CommitCheck{Name: "ci / web (pull_request)", State: CheckSuccess})
	select {
	case r := <-done:
		if r.err != nil || r.w.Verdict != PRWaitFailed || r.w.Why != "checks failed: "+ciBackendPR || r.w.HeadSHA != head || len(r.w.Checks) != 2 {
			t.Fatalf("failed: %+v %v", r.w, r.err)
		}
		if time.Since(start) > 5*time.Second {
			t.Errorf("woke after %s", time.Since(start))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the failed check didn't end the wait")
	}

	if w, _ := se.s.WaitPullRequest(se.ctx, se.admin, pr.ID, PRWaitInput{Head: "2222222bbbb"}); w.Verdict != PRWaitWaiting || w.Why != "Waypoint hasn't seen 2222222 on it yet" {
		t.Errorf("another head: %+v", w)
	}
	se.checks(head, CommitCheck{Name: ciBackendPR, State: CheckSuccess}, CommitCheck{Name: "ci / web (pull_request)", State: CheckSuccess})
	if w, _ := se.s.WaitPullRequest(se.ctx, se.admin, pr.ID, PRWaitInput{Head: head[:7]}); w.Verdict != PRWaitReady || !w.Readiness.Ready || w.CI != CIPassed {
		t.Fatalf("ready: %+v", w)
	}
	conflicted := testPR
	conflicted.Mergeable, conflicted.BaseRef = ptr(false), "main"
	if _, err := se.s.recordPR(se.ctx, se.f, "me/app", &conflicted, false); err != nil {
		t.Fatal(err)
	}
	if w, _ := se.s.WaitPullRequest(se.ctx, se.admin, pr.ID, PRWaitInput{}); w.Verdict != PRWaitWaiting || !w.Readiness.Checking {
		t.Errorf("checking: %+v", w)
	}
	se.setClock(se.clockNow().Add(2 * time.Minute))
	if _, err := se.s.recordPR(se.ctx, se.f, "me/app", &conflicted, false); err != nil {
		t.Fatal(err)
	}
	if w, _ := se.s.WaitPullRequest(se.ctx, se.admin, pr.ID, PRWaitInput{}); w.Verdict != PRWaitConflict || w.Why != "has conflicts with main" {
		t.Errorf("conflict: %+v", w)
	}
	clean := testPR
	clean.Mergeable, clean.BaseRef = ptr(true), "main"
	if _, err := se.s.recordPR(se.ctx, se.f, "me/app", &clean, false); err != nil {
		t.Fatal(err)
	}

	_, err = se.s.MergePR(se.ctx, se.admin, pr.ID, MergeInput{Head: "2222222bbbb"})
	if err == nil || !strings.Contains(err.Error(), "its head moved to 1111111 since 2222222") || len(se.sg.merged()) != 0 {
		t.Fatalf("merged a head it didn't wait on: %v", err)
	}
	res, err := se.s.MergePR(se.ctx, se.admin, pr.ID, MergeInput{Head: head})
	if err != nil || !res.Merged || res.SHA != "4e7ced0ffee" || se.sg.merged()[0].HeadSHA != head {
		t.Fatalf("merge: %+v %v", res, err)
	}
	if w, _ := se.s.WaitPullRequest(se.ctx, se.admin, pr.ID, PRWaitInput{}); w.Verdict != PRWaitMerged || w.MergedSHA != "4e7ced0ffee" {
		t.Errorf("merged: %+v", w)
	}

	draft := ForgePR{Number: 6, Title: "WIP: docs", Author: "sam", HeadRef: "docs", HeadSHA: "2222222bbbb", State: "draft"}
	se.sg.setChecks(draft.HeadSHA, CommitCheck{Name: ciBackendPR, State: CheckSuccess})
	se.pullHook("opened", draft)
	if w, _ := se.s.WaitPullRequest(se.ctx, se.admin, se.pr(6).ID, PRWaitInput{}); w.Verdict != PRWaitBlocked || w.Why != "still a draft" {
		t.Errorf("draft: %+v", w)
	}
	draft.State = "closed"
	se.pullHook("closed", draft)
	if w, _ := se.s.WaitPullRequest(se.ctx, se.admin, se.pr(6).ID, PRWaitInput{}); w.Verdict != PRWaitClosed {
		t.Errorf("closed: %+v", w)
	}
	if _, err := se.s.WaitPullRequest(se.ctx, se.user("zed", false), pr.ID, PRWaitInput{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a stranger waited: %v", err)
	}
}

// A failed check settles the wait while others still run (WP-155).
func TestWaitFailedWhileRunning(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.policy(MergePolicyInput{AutoMerge: ptr(false)})
	head := testPR.HeadSHA
	se.sg.setChecks(head, CommitCheck{Name: "ci / lint (pull_request)", State: CheckFailed}, CommitCheck{Name: ciBackendPR, State: CheckPending}, CommitCheck{Name: "ci / web (pull_request)", State: CheckPending})
	se.pullHook("opened", testPR)
	pr := se.pr(5)
	w, err := se.s.WaitPullRequest(se.ctx, se.admin, pr.ID, PRWaitInput{Timeout: time.Second})
	if err != nil || w.Verdict != PRWaitFailed || w.Why != "checks failed: ci / lint (pull_request) (2 still running)" {
		t.Fatalf("failed while running: %+v %v", w, err)
	}
}

// A push the webhook hasn't announced yet: the old head's cancelled checks
// are not the new head's failure (WP-153).
func TestWaitSupersededHead(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.policy(MergePolicyInput{AutoMerge: ptr(false)})
	head := testPR.HeadSHA
	se.sg.setChecks(head, CommitCheck{Name: ciBackendPR, State: CheckFailed})
	se.pullHook("opened", testPR)
	pr := se.pr(5)
	const newHead = "3333333cccc"
	se.fg.mu.Lock()
	se.fg.prs[mk(se.f, "me/app", 5)].HeadSHA = newHead
	se.fg.mu.Unlock()
	se.sg.setChecks(newHead, CommitCheck{Name: ciBackendPR, State: CheckPending})

	w, err := se.s.prWaitState(se.ctx, pr.ID, "")
	if err != nil || w.Verdict != PRWaitWaiting || !strings.Contains(w.Why, "3333333") {
		t.Fatalf("superseded: %+v %v", w, err)
	}
	se.s.Wait()
	if got := se.pr(5).HeadSHA; got != newHead {
		t.Fatalf("head not followed: %s", got)
	}
	if w, _ := se.s.prWaitState(se.ctx, pr.ID, ""); w.Verdict != PRWaitWaiting || w.Why != "1 check still running" {
		t.Errorf("new head: %+v", w)
	}
	se.checks(newHead, CommitCheck{Name: ciBackendPR, State: CheckFailed})
	if w, _ := se.s.prWaitState(se.ctx, pr.ID, ""); w.Verdict != PRWaitFailed {
		t.Errorf("failed head: %+v", w)
	}
}

func TestPRCheckLogs(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	head := testPR.HeadSHA
	goLog := "2026-10-17T15:21:11.0000000Z ok  \texample.com/app/db\t0.2s\n" +
		"2026-10-17T15:21:12.0000000Z --- FAIL: TestLogin (0.01s)\n" +
		"2026-10-17T15:21:12.0000000Z     login_test.go:41: status = 200, want 401\n" +
		"2026-10-17T15:21:12.0000000Z FAIL\texample.com/app/auth\t0.4s\n"
	se.sg.logs[ciBackendPR] = goLog
	se.sg.setChecks(head, CommitCheck{Name: ciBackendPR, State: CheckFailed, URL: "/me/app/actions/runs/3/jobs/0"},
		CommitCheck{Name: "ci / web (pull_request)", State: CheckSuccess})
	se.pullHook("opened", testPR)
	pr := se.pr(5)

	logs, err := se.s.PRCheckLogs(se.ctx, se.admin, pr.ID, "", false)
	if err != nil || logs.HeadSHA != head || len(logs.Checks) != 1 {
		t.Fatalf("logs: %+v %v", logs, err)
	}
	c := logs.Checks[0]
	if c.Name != ciBackendPR || c.From != "forge" || !c.Found || c.Lines != 5 || c.Error != "" ||
		c.Text != "--- FAIL: TestLogin (0.01s)\n    login_test.go:41: status = 200, want 401\nFAIL\texample.com/app/auth\t0.4s\n" {
		t.Errorf("excerpt: %+v", c)
	}
	logs, _ = se.s.PRCheckLogs(se.ctx, se.admin, pr.ID, "backend", true)
	if c := logs.Checks[0]; !strings.HasPrefix(c.Log, "the start of the log\nok  \texample.com/app/db") || c.Text != "" || strings.Contains(c.Log, "2026-10-17T") {
		t.Errorf("full: %+v", c)
	}
	logs, _ = se.s.PRCheckLogs(se.ctx, se.admin, pr.ID, "WEB", false)
	if len(logs.Checks) != 1 || logs.Checks[0].State != CheckSuccess || logs.Checks[0].Error != "the forge keeps no job log for this check" {
		t.Errorf("web: %+v", logs.Checks)
	}
	if _, err := se.s.PRCheckLogs(se.ctx, se.admin, pr.ID, "e2e", false); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "its checks: "+ciBackendPR) {
		t.Errorf("unknown check: %v", err)
	}
	delete(se.sg.logs, ciBackendPR)
	logs, _ = se.s.PRCheckLogs(se.ctx, se.admin, pr.ID, "", false)
	if c := logs.Checks[0]; c.From != "kept" || !strings.Contains(c.Text, "--- FAIL: TestLogin") || !strings.Contains(c.Error, "Waypoint kept") {
		t.Errorf("kept: %+v", c)
	}
	if _, err := se.s.PRCheckLogs(se.ctx, se.user("zed", false), pr.ID, "", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("a stranger read the logs: %v", err)
	}
}

// A new run of the same head cancels the old one, whose cancelled statuses
// stand until the new run's jobs start: they don't end the wait while
// anything still runs, and do once nothing does (WP-352).
func TestWaitCancelledRun(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.policy(MergePolicyInput{AutoMerge: ptr(false)})
	head := testPR.HeadSHA
	cancelled := func(name string, run int) CommitCheck {
		return CommitCheck{Name: name, State: CheckFailed, Description: "Has been cancelled", URL: fmt.Sprintf("/me/app/actions/runs/%d/jobs/0", run)}
	}
	se.sg.setChecks(head, cancelled("ci / lint (pull_request)", 762), cancelled("ci / e2e (pull_request)", 762),
		CommitCheck{Name: ciBackendPR, State: CheckPending, Description: "Has started running", URL: "/me/app/actions/runs/764/jobs/1"})
	se.pullHook("opened", testPR)
	pr := se.pr(5)
	w, err := se.s.prWaitState(se.ctx, pr.ID, "")
	if err != nil || w.Verdict != PRWaitWaiting || w.Why != "1 check still running" {
		t.Fatalf("cancelled while the new run goes: %+v %v", w, err)
	}
	se.checks(head, cancelled("ci / e2e (pull_request)", 762), CommitCheck{Name: "ci / lint (pull_request)", State: CheckFailed, Description: "Failing after 1m"},
		CommitCheck{Name: ciBackendPR, State: CheckPending})
	if w, _ := se.s.prWaitState(se.ctx, pr.ID, ""); w.Verdict != PRWaitFailed || !strings.HasPrefix(w.Why, "checks failed: ") {
		t.Errorf("a real failure: %+v", w)
	}
	se.checks(head, cancelled("ci / e2e (pull_request)", 762), CommitCheck{Name: ciBackendPR, State: CheckSuccess})
	if w, _ := se.s.prWaitState(se.ctx, pr.ID, ""); w.Verdict != PRWaitFailed {
		t.Errorf("cancelled, nothing running: %+v", w)
	}
}

// A queued review holds the pull request only within the review wait: one
// parked on a model's quota or behind a backlog doesn't hold the merge queue
// for hours. A running one always holds.
func TestReviewGoingQueuedWithinReviewWait(t *testing.T) {
	se := newShipEnv(t, "github")
	se.policy(MergePolicyInput{ReviewWaitMinutes: ptr(30), QuietMinutes: ptr(0)})
	se.pullHook("opened", testPR)
	se.drain()
	pr := se.pr(5)
	if _, err := se.s.db.W.ExecContext(se.ctx, `DELETE FROM bot_runs`); err != nil {
		t.Fatal(err)
	}
	if going, _ := se.s.reviewGoing(se.ctx, pr); going {
		t.Fatal("going with no review")
	}
	if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO bot_runs(project_id, pr_id, kind, head_sha, status, created_at)
		VALUES ((SELECT min(id) FROM projects), ?, 'review', ?, 'queued', ?)`, pr.ID, pr.HeadSHA, se.s.nowMS()); err != nil {
		t.Fatal(err)
	}
	if going, why := se.s.reviewGoing(se.ctx, pr); !going || why != "a review is queued" {
		t.Fatalf("queued within the wait: %v %q", going, why)
	}
	se.setClock(se.clockNow().Add(31 * time.Minute))
	if going, why := se.s.reviewGoing(se.ctx, se.pr(5)); going {
		t.Fatalf("queued past the wait still holds: %q", why)
	}
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE bot_runs SET status = 'running'`); err != nil {
		t.Fatal(err)
	}
	if going, why := se.s.reviewGoing(se.ctx, se.pr(5)); !going || why != "a review is running" {
		t.Fatalf("running: %v %q", going, why)
	}
	se.policy(MergePolicyInput{ReviewWaitMinutes: ptr(0)})
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE bot_runs SET status = 'queued'`); err != nil {
		t.Fatal(err)
	}
	if going, _ := se.s.reviewGoing(se.ctx, se.pr(5)); !going {
		t.Fatal("queued with no review wait doesn't hold")
	}
}
