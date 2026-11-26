package core

import (
	"slices"
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
		CommitCheck{Name: "ci / e2e", State: CheckFailed, Description: "Has been cancelled"},
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

// No round goes out before a first check has reported on the head; it goes
// once one has.
func TestEarlyRoundWaitsForFirstCheck(t *testing.T) {
	be := earlyEnv(t, true, nil)
	b := be.babysit()
	if len(be.jobs()) != 0 || !strings.Contains(b.Wait, "a first check to report") {
		t.Fatalf("round before any check reported: %d jobs, %s %q", len(be.jobs()), b.State, b.Wait)
	}
	be.setCI(CommitCheck{Name: "ci / test", State: CheckPending})
	be.step()
	if len(be.jobs()) != 1 {
		b = be.babysit()
		t.Fatalf("no round once a check reported: %s %q", b.State, b.Wait)
	}
}

// A prompt with no check shown running still says a listed failure may not be
// the only one, and doesn't claim that none is listed.
func TestEarlyRoundNoteWithNothingRunning(t *testing.T) {
	var p strings.Builder
	writeEarlyCI(&p, &BabysitRound{Early: true, Checks: []string{"ci / lint"}})
	if got := p.String(); !strings.Contains(got, "CI has not finished on this head. Any failed check listed below") || strings.Contains(got, "still running") {
		t.Errorf("note = %q", got)
	}
}

// Audit findings beside a failed check, with no comment work, wait for the
// checks to finish: the runner-fault and flake gates look at the failure
// before any round does.
func TestEarlyRoundHoldsFindingsOnFailedCheck(t *testing.T) {
	be := newBabysitEnv(t)
	box := be.auditBox()
	be.auditOn()
	be.runnerOnline()
	pr := be.openMine()
	be.sg.setChecks(bsPR.HeadSHA, CommitCheck{Name: "ci / test", State: CheckPending}, CommitCheck{Name: "ci / lint", State: CheckFailed})
	be.poll()
	be.start(pr)
	be.endAudit(box, "done", [2]string{"medium", "No limit on rows"})
	be.runnerOnline()
	be.step()
	b := be.babysit()
	if len(be.jobs()) != 0 || !strings.Contains(b.Wait, "flaky test") {
		t.Fatalf("round before the failure was looked at: %d jobs, %s %q", len(be.jobs()), b.State, b.Wait)
	}
}

// An early round no runner can take yet is held and says so, and goes once
// one can.
func TestEarlyRoundWaitsForRunner(t *testing.T) {
	be := earlyEnv(t, true, nil)
	be.setClock(be.clockNow().Add(time.Hour))
	be.sg.setChecks(bsPR.HeadSHA, CommitCheck{Name: "ci / test", State: CheckPending})
	be.poll()
	be.step()
	b := be.babysit()
	if len(b.Rounds) != 0 || !strings.Contains(b.Wait, "checks running") || !strings.Contains(b.Wait, "waiting for a runner") {
		t.Fatalf("with no runner: %+v %s %q", b.Rounds, b.State, b.Wait)
	}
	be.runnerOnline()
	be.step()
	if len(be.jobs()) != 1 {
		b = be.babysit()
		t.Fatalf("no round once a runner could take it: %s %q", b.State, b.Wait)
	}
}

// Comments no round would be given (here: copied from a bot, on files the
// pull request doesn't change) stay new while the checks run.
func TestEarlyRoundWithNoWorkLeavesCommentsNew(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT OR IGNORE INTO forge_people(forge_id, login, user_id) VALUES (?, 'mirror', NULL)`, be.f.ID); err != nil {
		t.Fatal(err)
	}
	be.sg.setNotes(PRNote{ID: "702", Kind: "comment", Author: "mirror", Body: crReview, At: be.clockNow().Add(-time.Minute)})
	pr := be.open()
	be.changes(bsPR.HeadSHA, "app/other.go")
	be.sg.setChecks(bsPR.HeadSHA, CommitCheck{Name: "ci / test", State: CheckPending})
	be.poll()
	be.start(pr)
	be.step()
	cs := be.comments()
	if len(be.jobs()) != 0 || len(cs) == 0 {
		t.Fatalf("%d jobs, %d comments", len(be.jobs()), len(cs))
	}
	for _, c := range cs {
		if c.Verdict != VerdictNew {
			t.Errorf("comment %s is %s with no round sent", c.Ref, c.Verdict)
		}
	}
}

// An early round is told of a merge conflict the forge reports.
func TestEarlyRoundCarriesConflict(t *testing.T) {
	be := earlyEnv(t, true, nil)
	be.exec(`UPDATE pull_requests SET mergeable = 0`)
	be.setClock(be.clockNow().Add(mergeCheckingLimit))
	be.setCI(CommitCheck{Name: "ci / test", State: CheckPending})
	be.step()
	b := be.babysit()
	if len(b.Rounds) != 1 || !b.Rounds[0].Early || !b.Rounds[0].Conflict {
		t.Fatalf("rounds = %+v: %s %q", b.Rounds, b.State, b.Wait)
	}
}

// A reviewer still expected holds the round, and the state says what for
// rather than only that CI runs.
func TestEarlyRoundWaitsForMissingReviewer(t *testing.T) {
	be := earlyEnv(t, true, &MergePolicyInput{ExternalReviewers: &[]string{"llm-review"}, ReviewWaitMinutes: ptr(30), QuietMinutes: ptr(0)},
		CommitCheck{Name: "ci / test", State: CheckPending})
	b := be.babysit()
	if len(be.jobs()) != 0 || !strings.Contains(b.Wait, "checks running") || !strings.Contains(b.Wait, "; a round for its review work is held: waiting for llm-review") {
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
	post("llm-review")
	if !waits(true) {
		t.Error("a later post didn't start the quiet period before a round again")
	}
	be.setClock(be.clockNow().Add(3 * time.Minute))
	be.exec(`UPDATE automations SET enabled = 0`)
	pol.ExternalReviewers = nil
	if !waits(true) {
		t.Error("the quiet period was shortened with no reviewer expected")
	}
}

func postReview(be *bsEnv, pr *PullRequest, actor string) {
	be.t.Helper()
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO pr_events(pr_id, kind, actor, bot, sha, created_at) VALUES (?, 'reviewed', ?, 1, ?, ?)`,
		pr.ID, actor, bsPR.HeadSHA, be.s.nowMS()); err != nil {
		be.t.Fatal(err)
	}
}

func (be *bsEnv) setCI(cs ...CommitCheck) {
	be.t.Helper()
	be.sg.setChecks(bsPR.HeadSHA, cs...)
	from := be.clockNow().UnixMilli()
	be.runnerOnline()
	be.poll()
	be.s.Wait()
	for _, c := range cs {
		if c.State == CheckFailed && be.flakeRerunSince(bsPR.HeadSHA, from) {
			be.rerunFails()
			break
		}
	}
}

// An early round that pushed nothing, then CI failing a check it wasn't
// given, is an ordinary round carrying that check: new work, not a stuck
// head, so the same model (WP-ADR163).
func TestEarlyRoundThenNewFailureIsOrdinaryRound(t *testing.T) {
	be := earlyEnv(t, true, nil, CommitCheck{Name: "ci / test", State: CheckPending}, CommitCheck{Name: "ci / lint", State: CheckPending})
	jobs := be.jobs()
	if len(jobs) != 1 {
		t.Fatalf("no early round: %d jobs", len(jobs))
	}
	be.finish(jobs[0], false)
	be.setCI(CommitCheck{Name: "ci / test", State: CheckPending}, CommitCheck{Name: "ci / lint", State: CheckFailed})
	be.setCI(CommitCheck{Name: "ci / test", State: CheckSuccess}, CommitCheck{Name: "ci / lint", State: CheckFailed})
	be.step()
	b := be.babysit()
	if len(b.Rounds) != 2 {
		t.Fatalf("rounds = %d: %s %q", len(b.Rounds), b.State, b.Wait)
	}
	r := b.Rounds[1]
	if r.Early || strings.Contains(be.jobInput(be.jobs()[1]).Prompt, "CI has not finished") {
		t.Error("the round after CI ended is told it hadn't")
	}
	if !slices.Contains(r.Checks, "ci / lint") || r.Model == escalateModel || r.Assessor {
		t.Errorf("round 2 = checks %v model %q assessor %v, want the ordinary model carrying ci / lint", r.Checks, r.Model, r.Assessor)
	}
}

// The twin: the early round was given the failed check, pushed nothing, and
// CI ends with only that check failed. That is a head a round already had
// the failure of, so it is stuck and the ladder climbs: the stronger model.
func TestEarlyRoundThenSameFailureClimbsTheLadder(t *testing.T) {
	be := earlyEnv(t, true, nil, CommitCheck{Name: "ci / test", State: CheckFailed}, CommitCheck{Name: "ci / lint", State: CheckPending})
	jobs := be.jobs()
	if len(jobs) != 1 {
		t.Fatalf("no early round: %d jobs", len(jobs))
	}
	if r := be.babysit().Rounds[0]; len(r.Checks) != 1 || r.Checks[0] != "ci / test" || !r.Early {
		t.Fatalf("round 1 = %+v", r)
	}
	be.finish(jobs[0], false)
	be.setCI(CommitCheck{Name: "ci / test", State: CheckFailed}, CommitCheck{Name: "ci / lint", State: CheckSuccess})
	be.step()
	b := be.babysit()
	if len(b.Rounds) != 2 || b.Rounds[1].Model != escalateModel {
		t.Fatalf("rounds = %+v: %s %q", b.Rounds, b.State, b.Wait)
	}
}

// Audit findings alone are early-round work.
func TestEarlyRoundForAuditFindings(t *testing.T) {
	be := newBabysitEnv(t)
	box := be.auditBox()
	be.auditOn()
	be.runnerOnline()
	pr := be.openMine()
	be.sg.setChecks(bsPR.HeadSHA, CommitCheck{Name: "ci / test", State: CheckPending})
	be.poll()
	be.start(pr)
	be.endAudit(box, "done", [2]string{"medium", "No limit on rows"})
	be.runnerOnline()
	be.step()
	b := be.babysit()
	jobs := be.jobs()
	if len(jobs) != 1 || len(b.Rounds) != 1 || len(b.Rounds[0].Findings) != 1 || !b.Rounds[0].Early {
		t.Fatalf("no early round for a finding: %d jobs, %+v, %s %q", len(jobs), b.Rounds, b.State, b.Wait)
	}
	if p := be.jobInput(jobs[0]).Prompt; !strings.Contains(p, "No limit on rows") || !strings.Contains(p, "CI has not finished") {
		t.Errorf("the prompt:\n%s", p)
	}
}

// A bot's claim that the build breaks waits for the build while no check has
// failed, and gets its round once one has.
func TestEarlyRoundHoldsCompileClaim(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	claim := "**P0** critical · Duplicate definition of `Export` causes a compile error, breaking the build."
	be.sg.threads = []ReviewThread{{ID: "T1", Path: "app/export.go", Line: 4, Comments: []ThreadComment{
		{ID: "501", Author: "llm-review", Body: claim, SHA: bsPR.HeadSHA, At: be.clockNow().Add(-time.Minute)}}}}
	pr := be.open()
	be.sg.setChecks(bsPR.HeadSHA, CommitCheck{Name: "ci / test", State: CheckPending}, CommitCheck{Name: "ci / lint", State: CheckPending})
	be.poll()
	be.start(pr)
	be.step()
	b := be.babysit()
	if len(be.jobs()) != 0 || !strings.Contains(b.Wait, "checks running") || !strings.Contains(b.Wait, "compile") {
		t.Fatalf("round on an unchecked compile claim: %d jobs, %s %q", len(be.jobs()), b.State, b.Wait)
	}
	be.setCI(CommitCheck{Name: "ci / test", State: CheckFailed}, CommitCheck{Name: "ci / lint", State: CheckPending})
	be.step()
	if len(be.jobs()) != 1 {
		b = be.babysit()
		t.Fatalf("no round once a check failed: %s %q", b.State, b.Wait)
	}
}

// A review still running holds the early round, and the state says so;
// the round goes once it ends.
func TestEarlyRoundWaitsForRunningReview(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	be.sg.threads = []ReviewThread{{ID: "T1", Path: "app/export.go", Line: 4, Comments: []ThreadComment{
		{ID: "501", Author: "alice", Body: "This loop never ends when rows is empty.", SHA: bsPR.HeadSHA, At: be.clockNow().Add(-time.Minute)}}}}
	pr := be.open()
	res, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO bot_runs(project_id, pr_id, kind, head_sha, status, created_at) VALUES (?, ?, 'review', ?, 'running', ?)`,
		be.p.ID, pr.ID, bsPR.HeadSHA, be.s.nowMS())
	if err != nil {
		t.Fatal(err)
	}
	be.sg.setChecks(bsPR.HeadSHA, CommitCheck{Name: "ci / test", State: CheckPending})
	be.poll()
	be.start(pr)
	be.step()
	b := be.babysit()
	if len(be.jobs()) != 0 || !strings.Contains(b.Wait, "a review is running") {
		t.Fatalf("round while a review runs: %d jobs, %s %q", len(be.jobs()), b.State, b.Wait)
	}
	id, _ := res.LastInsertId()
	be.exec(`UPDATE bot_runs SET status = 'done', finished_at = created_at WHERE id = ` + itoa64(id))
	be.step()
	if len(be.jobs()) != 1 {
		b = be.babysit()
		t.Fatalf("no round once the review ended: %s %q", b.State, b.Wait)
	}
}
