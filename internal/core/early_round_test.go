package core

import (
	"strings"
	"testing"
	"time"
)

// earlyEnv is a babysit on a head whose CI still runs, with one review
// comment on it when comment is set.
func earlyEnv(t *testing.T, comment bool, pol *MergePolicyInput, checks ...CommitCheck) *bsEnv {
	t.Helper()
	be := newBabysitEnv(t)
	be.runnerOnline()
	if comment {
		be.sg.threads = []ReviewThread{{ID: "T1", Path: "app/export.go", Line: 4, Comments: []ThreadComment{
			{ID: "501", Author: "alice", Body: "This loop never ends when rows is empty.", SHA: bsPR.HeadSHA, At: be.clockNow().Add(-time.Minute)}}}}
	}
	pr := be.open()
	if pol != nil {
		be.policy(*pol)
	}
	if len(checks) > 0 {
		be.sg.setChecks(bsPR.HeadSHA, checks...)
		be.poll()
	}
	be.start(pr)
	be.step()
	return be
}

// Review work on a head whose CI runs gets its round now, told which checks
// were still running and which had failed (WP-ADR163).
func TestEarlyRoundNamesRunningChecks(t *testing.T) {
	be := earlyEnv(t, true, nil,
		CommitCheck{Name: "ci / test", State: CheckPending},
		CommitCheck{Name: "ci / lint", State: CheckFailed},
		CommitCheck{Name: "ci / build", State: CheckSuccess})
	b := be.babysit()
	jobs := be.jobs()
	if len(jobs) != 1 || b.State != BabysitWorking {
		t.Fatalf("no round while CI runs: %d jobs, %s %q", len(jobs), b.State, b.Wait)
	}
	r := b.Rounds[0]
	if !r.Early || len(r.Running) != 1 || r.Running[0] != "ci / test" || len(r.Checks) != 1 || r.Checks[0] != "ci / lint" {
		t.Errorf("round = early %v running %v failed %v", r.Early, r.Running, r.Checks)
	}
	p := be.jobInput(jobs[0]).Prompt
	for _, want := range []string{"CI has not finished on this head: ci / test still running", "Run the tests of what you change"} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, p)
		}
	}
	if len(be.sg.merged()) != 0 {
		t.Error("merged before its checks passed")
	}
	if strings.Contains(b.Wait, "checks running") {
		t.Errorf("the state still says it waits for CI: %q", b.Wait)
	}
}

// A check that hasn't reported at all is told so.
func TestEarlyRoundBeforeAnyCheckReported(t *testing.T) {
	be := earlyEnv(t, true, nil)
	jobs := be.jobs()
	if len(jobs) != 1 {
		t.Fatalf("no round before CI reported: %d jobs", len(jobs))
	}
	if p := be.jobInput(jobs[0]).Prompt; !strings.Contains(p, "no check had reported") {
		t.Errorf("the prompt: %s", p)
	}
}

// A reviewer still expected holds the round, and the state says what for
// rather than only that CI runs.
func TestEarlyRoundWaitsForMissingReviewer(t *testing.T) {
	be := earlyEnv(t, true, &MergePolicyInput{ExternalReviewers: &[]string{"llm-review"}, ReviewWaitMinutes: ptr(30), QuietMinutes: ptr(0)},
		CommitCheck{Name: "ci / test", State: CheckPending})
	b := be.babysit()
	if len(be.jobs()) != 0 || !strings.Contains(b.Wait, "checks running") || !strings.Contains(b.Wait, "waiting for llm-review") {
		t.Fatalf("round before the reviewer: %d jobs, %s %q", len(be.jobs()), b.State, b.Wait)
	}
}

// Failed checks alone, or nothing to do, wait for CI as before.
func TestEarlyRoundNeedsReviewWork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		checks []CommitCheck
	}{
		{"failed check only", []CommitCheck{{Name: "ci / test", State: CheckPending}, {Name: "ci / lint", State: CheckFailed}}},
		{"nothing to do", []CommitCheck{{Name: "ci / test", State: CheckPending}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := earlyEnv(t, false, nil, tc.checks...)
			b := be.babysit()
			if len(be.jobs()) != 0 || !strings.Contains(b.Wait, "checks running") {
				t.Fatalf("round without review work: %d jobs, %s %q", len(be.jobs()), b.State, b.Wait)
			}
		})
	}
}

// The quiet period before a round is two minutes once every expected
// reviewer has reported, and the policy's own until then; the merge's is
// always the policy's.
func TestQuietPeriodShortensOnReports(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	be.policy(MergePolicyInput{ExternalReviewers: &[]string{"llm-review"}, ReviewWaitMinutes: ptr(0), QuietMinutes: ptr(10)})
	pol, _, err := be.s.repoMergePolicy(be.ctx, pr.ForgeID, pr.Repo)
	if err != nil {
		t.Fatal(err)
	}
	waits := func(forRound bool) bool {
		t.Helper()
		r, _, err := be.s.settleWaitsFor(be.ctx, be.pr(7), pol, nil, forRound)
		if err != nil {
			t.Fatal(err)
		}
		return len(r) > 0
	}
	post := func(actor string) {
		t.Helper()
		postReview(be, pr, actor)
	}
	post("alice")
	be.setClock(be.clockNow().Add(3 * time.Minute))
	if !waits(true) {
		t.Error("the quiet period ended at 3 minutes with the expected reviewer missing")
	}
	post("llm-review")
	be.setClock(be.clockNow().Add(3 * time.Minute))
	if waits(true) {
		t.Error("a round still waits 3 minutes after the last post with every expected reviewer reported")
	}
	if !waits(false) {
		t.Error("the merge's quiet period was shortened")
	}
}

func postReview(be *bsEnv, pr *PullRequest, actor string) {
	be.t.Helper()
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO pr_events(pr_id, kind, actor, bot, sha, created_at) VALUES (?, 'reviewed', ?, 1, ?, ?)`,
		pr.ID, actor, bsPR.HeadSHA, be.s.nowMS()); err != nil {
		be.t.Fatal(err)
	}
}
