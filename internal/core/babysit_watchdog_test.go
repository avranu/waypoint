package core

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// disarm leaves #n as a pull request nobody asked to merge.
func (be *bsEnv) disarm(n int) {
	be.t.Helper()
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET armed_at = NULL, arm_declined = 1 WHERE number = ?`, n); err != nil {
		be.t.Fatal(err)
	}
}

// rested says #7's babysit has waited on the same thing for d.
func (be *bsEnv) rested(d time.Duration) {
	be.t.Helper()
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE babysits SET wait_at = ? WHERE pr_id = ?`, be.s.now().Add(-d).UnixMilli(), be.pr(7).ID); err != nil {
		be.t.Fatal(err)
	}
}

func (be *bsEnv) revive() {
	be.t.Helper()
	be.s.reviveStaleBabysits(be.ctx)
	be.s.Wait()
}

func (be *bsEnv) personRequests() int {
	be.t.Helper()
	var n int
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT count(*) FROM requests WHERE tool_use_id LIKE ?`, personRequestKey+"%").Scan(&n); err != nil {
		be.t.Fatal(err)
	}
	return n
}

// startRested is #7 under a babysit that has held the given wait for d.
func startRested(t *testing.T, state, wait string, d time.Duration) *bsEnv {
	t.Helper()
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.setBabysitState(state, wait, false)
	be.rested(d)
	return be
}

// A babysit left waiting for nobody gets the assessor once; when the
// assessor's round moves nothing it rests out of the merge line, nobody
// asked (WP-930), and no round runs before the rest is over.
func TestWatchdogSendsTheAssessorThenRests(t *testing.T) {
	be := startRested(t, BabysitNeedsPerson, "a round found nothing it could change", 7*time.Hour)
	be.revive()
	b := be.babysit()
	jobs := be.jobs()
	if len(jobs) != 1 || len(b.Rounds) != 1 || !b.Rounds[0].Assessor {
		t.Fatalf("no assessor round: %+v jobs %v", b, jobs)
	}
	be.finish(jobs[0], false)
	be.setBabysitState(BabysitNeedsPerson, "a round found nothing it could change", false)
	be.rested(7 * time.Hour)
	be.revive()
	b = be.babysit()
	if len(be.jobs()) != 1 || !strings.HasPrefix(b.Wait, restWait) || !resting(lastRound(b)) {
		t.Fatalf("a second assessor on the same head: %+v jobs %v", b, be.jobs())
	}
	var asked int
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT count(*) FROM requests WHERE tool_use_id LIKE ?`, parkRequestKey+"%").Scan(&asked); err != nil || asked != 0 {
		t.Fatalf("park questions = %d, %v", asked, err)
	}
	be.rested(48 * time.Hour)
	be.revive()
	if len(be.jobs()) != 1 {
		t.Fatalf("a resting babysit got another round: %v", be.jobs())
	}
}

// Comments only a person can settle get one request, with no agent sent.
func TestWatchdogAsksAboutPersonComments(t *testing.T) {
	be, ref := restedOnComment(t, func(be *bsEnv) *Actor { return be.admin })
	be.revive()
	be.rested(30 * time.Hour)
	be.revive()
	if n := be.personRequests(); n != 1 {
		t.Fatalf("requests = %d", n)
	}
	if len(be.jobs()) != 0 {
		t.Errorf("an agent was sent to a person's own verdict: %v", be.jobs())
	}
	var body string
	if err := be.s.db.R.QueryRow(`SELECT body FROM requests WHERE tool_use_id LIKE ?`, personRequestKey+"%").Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "Handle empty rows.") || !strings.Contains(body, "app/export.go:4") || strings.Contains(body, ref) || strings.Contains(body, "An agent") || !strings.Contains(body, "/agents?tab=pulls&pr=") || !strings.Contains(body, "Dismiss") {
		t.Errorf("request body = %q", body)
	}
}

// A wait from before WP-759 on a comment an agent left goes to the assessor,
// with no request for its owner.
func TestWatchdogSendsTheAssessorToAnAgentsHold(t *testing.T) {
	be, ref := restedOnComment(t, func(be *bsEnv) *Actor { return be.agent })
	be.revive()
	b := be.babysit()
	if len(be.jobs()) != 1 || b.State != BabysitWorking || !b.Rounds[0].Assessor || !slices.Contains(b.Rounds[0].Comments, ref) {
		t.Fatalf("no assessor round: %+v", b)
	}
	if be.personRequests() != 0 {
		t.Error("a person was asked to do an agent's work")
	}
}

// restedOnComment is #7's babysit resting 7 hours on a review comment that
// actor left for a person.
func restedOnComment(t *testing.T, actor func(*bsEnv) *Actor) (*bsEnv, string) {
	t.Helper()
	be := newBabysitEnv(t)
	be.sg.threads = humanThread()
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	ref := be.comment("501").Ref
	if _, err := be.s.AckReviewComments(be.ctx, actor(be), AckInput{IDs: []string{ref}, Verdict: "needs-person", Note: "A product call."}); err != nil {
		t.Fatal(err)
	}
	be.s.Wait()
	be.setBabysitState(BabysitNeedsPerson, "1 review comment waits for a person: "+ref, false)
	be.rested(7 * time.Hour)
	return be, ref
}

// refusedAndRested is #7's babysit resting 7 hours on round 1's fix refused
// for a protected file; mine says the pull request changes that file itself.
func refusedAndRested(t *testing.T, mine bool) *bsEnv {
	t.Helper()
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	if mine {
		be.sg.mu.Lock()
		be.sg.diffs["7777777aaaa"] = "diff --git a/.github/scripts/run.py b/.github/scripts/run.py\n--- a/.github/scripts/run.py\n+++ b/.github/scripts/run.py\n@@ -1 +1 @@\n-a\n+b\n"
		be.sg.mu.Unlock()
		if _, err := be.s.db.W.Exec(`UPDATE pull_requests SET files_sha = '' WHERE id = ?`, pr.ID); err != nil {
			t.Fatal(err)
		}
	}
	be.start(pr)
	be.ci("7777777aaaa", CheckFailed)
	jobs := be.jobs()
	if len(jobs) != 1 {
		t.Fatalf("jobs %v", jobs)
	}
	be.refuseProtected(jobs[0], ".github/scripts/run.py")
	if b := be.babysit(); b.State != BabysitNeedsPerson || !waitsForSession(b) {
		t.Fatalf("not handed to a session: %+v", b)
	}
	be.rested(7 * time.Hour)
	return be
}

// A fix refused for protected files the pull request doesn't change itself
// gets the assessor's round once rested (WP-1043): before WP-750 a base
// merge's CI files were refused that way and the handoff sat for a week.
func TestWatchdogRevivesRefusalOutsideDiff(t *testing.T) {
	be := refusedAndRested(t, false)
	be.revive()
	b := be.babysit()
	jobs := be.jobs()
	if len(jobs) != 2 || len(b.Rounds) != 2 || !b.Rounds[1].Assessor {
		t.Fatalf("no assessor round: %+v jobs %v", b, jobs)
	}
	be.refuseProtected(jobs[1], ".github/scripts/run.py")
	be.rested(7 * time.Hour)
	be.revive()
	if len(be.jobs()) != 2 {
		t.Fatalf("a second assessor on the same head: %v", be.jobs())
	}
}

// One the pull request changes itself stays with the session's task.
func TestWatchdogLeavesRefusalInDiff(t *testing.T) {
	be := refusedAndRested(t, true)
	be.revive()
	if len(be.jobs()) != 1 || !waitsForSession(be.babysit()) {
		t.Fatalf("acted: jobs %v, %+v", be.jobs(), be.babysit())
	}
}

// What is someone else's to lift, or hasn't rested long, is left alone.
func TestWatchdogLeavesAlone(t *testing.T) {
	for _, tc := range []struct {
		name, state, wait string
		rest              time.Duration
	}{
		{"not long enough", BabysitNeedsPerson, "a round found nothing it could change", 2 * time.Hour},
		{"parked", BabysitNeedsPerson, parkWait + " on the assessor's question", 30 * time.Hour},
		{"protected files", BabysitNeedsPerson, "a round needed protected files changed", 30 * time.Hour},
		{"review gate", BabysitNeedsPerson, "review gate: audit #3 found something", 30 * time.Hour},
		{"change request", BabysitNeedsPerson, "carol asked for changes", 30 * time.Hour},
		{"watching for something else", BabysitWatching, "waiting for a runner", 30 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := startRested(t, tc.state, tc.wait, tc.rest)
			be.revive()
			if len(be.jobs()) != 0 || be.personRequests() != 0 {
				t.Fatalf("acted: jobs %v, requests %d, %+v", be.jobs(), be.personRequests(), be.babysit())
			}
		})
	}
}

// A babysit whose checks seem to have stopped reads them again, hourly: a
// stale read kept a pull request saying "checks running" for good.
func TestWatchdogReadsStaleChecks(t *testing.T) {
	for _, wait := range []string{checksWait + " on 7777777", "checks haven't been read yet"} {
		t.Run(wait, func(t *testing.T) { testWatchdogReadsStaleChecks(t, wait) })
	}
}

func testWatchdogReadsStaleChecks(t *testing.T, wait string) {
	be := startRested(t, BabysitWatching, wait, 2*time.Hour)
	state := func() string {
		var s string
		if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT state FROM ci_heads WHERE sha = '7777777aaaa'`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	be.sg.setChecks("7777777aaaa", CommitCheck{Name: "ci / test", State: CheckSuccess})
	be.revive()
	if got := state(); got != "passed" {
		t.Fatalf("checks weren't read again: %q", got)
	}
	be.setBabysitState(BabysitWatching, wait, false)
	be.rested(2 * time.Hour)
	be.sg.setChecks("7777777aaaa", CommitCheck{Name: "ci / test", State: CheckFailed})
	be.revive()
	if got := state(); got != "passed" {
		t.Errorf("read again within the hour: %q", got)
	}
}

// A ready pull request nobody armed is brought up to date with its base,
// and its checks are watched again.
func TestReadyCatchUp(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.disarm(7)
	be.sg.behind = map[string]bool{"7777777aaaa": true}
	be.ci("7777777aaaa", CheckSuccess)
	b := be.babysit()
	if be.sg.updated() != 1 || b.State != BabysitWatching || !strings.HasPrefix(b.Wait, checksWait+" after merging") {
		t.Fatalf("updates %d: %+v", be.sg.updated(), b)
	}
	if len(be.sg.merged()) != 0 {
		t.Errorf("merged an unarmed pull request: %+v", be.sg.merged())
	}
}

// Only a few at a time per base: a run of them mustn't take the CI pool.
func TestReadyCatchUpLimit(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.disarm(7)
	for _, n := range []int{8, 9} {
		p := bsPR
		p.Number, p.HeadRef, p.HeadSHA = n, fmt.Sprintf("other-%d", n), fmt.Sprintf("%d%d%d%daaaa", n, n, n, n)
		be.pullHook("opened", p)
		be.s.Wait()
		be.drain()
		if _, err := be.s.StartBabysit(be.ctx, be.agent, be.pr(n).ID, BabysitInput{Via: "ship"}); err != nil {
			t.Fatal(err)
		}
		be.s.Wait()
		be.disarm(n)
		if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE babysits SET state = ?, wait = ?, wait_at = ?, job_id = NULL WHERE pr_id = ?`,
			BabysitWatching, checksWait+" after merging main into it", be.s.nowMS(), be.pr(n).ID); err != nil {
			t.Fatal(err)
		}
	}
	be.sg.behind = map[string]bool{"7777777aaaa": true}
	be.sg.setChecks("7777777aaaa", CommitCheck{Name: "ci / test", State: CheckSuccess})
	be.poll()
	be.s.Wait()
	be.step()
	if be.sg.updated() != 0 || be.babysit().State != BabysitReady {
		t.Fatalf("updates %d: %+v", be.sg.updated(), be.babysit())
	}
}

// A conflict found on the way sends a round to settle it.
func TestReadyCatchUpConflict(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.disarm(7)
	be.sg.behind = map[string]bool{"7777777aaaa": true}
	be.sg.updateErr = &forgeAPIError{method: "POST", path: "/repos/me/app/pulls/7/update", status: 409, body: "merge conflict"}
	be.ci("7777777aaaa", CheckSuccess)
	be.step()
	if len(be.jobs()) != 1 {
		t.Fatalf("no round for the conflict: %+v", be.babysit())
	}
}

// With no merge coming, a late minor finding gets one closing round.
func TestLateMinorClosingRoundWhenUnarmed(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT OR IGNORE INTO forge_people(forge_id, login, user_id) VALUES (?, 'mirror', NULL)`, be.f.ID); err != nil {
		t.Fatal(err)
	}
	carol := PRNote{ID: "601", Kind: "comment", Author: "carol", Body: "Should exports include archived rows?", At: be.clockNow()}
	be.sg.setNotes(carol)
	pr := be.open()
	be.start(pr)
	be.disarm(7)
	be.ci("7777777aaaa", CheckSuccess)
	jobs := be.jobs()
	if len(jobs) != 1 {
		t.Fatalf("no round for carol's comment: %+v", be.babysit())
	}
	if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{be.comment("601").Ref}, Verdict: VerdictRejected, Note: "Archived rows stay out, as the spec says."}); err != nil {
		t.Fatal(err)
	}
	be.setClock(be.clockNow().Add(10 * time.Minute))
	be.sg.setNotes(carol, PRNote{ID: "602", Kind: "comment", Author: "mirror", At: be.clockNow(),
		Body: "**coderabbitai[bot]** commented on GitHub:\n\n_⚠️ Potential issue_ | _🟡 Minor_\n\nTrim the name before comparing.\n\n<!-- mirror:comment github 9 -->"})
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE runner_jobs SET state = 'running' WHERE id = ?`, jobs[0]); err != nil {
		t.Fatal(err)
	}
	be.push("8888888bbbb")
	be.finish(jobs[0], true)
	be.ci("8888888bbbb", CheckSuccess)
	be.step()
	jobs = be.jobs()
	if len(jobs) != 2 {
		t.Fatalf("no closing round: %+v", be.babysit())
	}
	if b := be.babysit(); !slices.Contains(b.Rounds[len(b.Rounds)-1].Comments, be.comment("602").Ref) {
		t.Errorf("the round wasn't given the finding: %+v", b.Rounds)
	}
	be.finish(jobs[1], false)
	be.step()
	if len(be.jobs()) != 2 {
		t.Errorf("offered twice: %v", be.jobs())
	}
}

// A comment verified with no reply gets one, once: Forgejo can't resolve a
// thread, and silence reads as ignored.
func TestVerifiedCommentGetsOneReply(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	be.llm.answers["opencode/muse-spark-1.3-contributor-free"] = oneFinding
	pr := be.open()
	be.start(pr)
	var finding *PRReviewComment
	for _, c := range be.comments() {
		if c.FindingID != 0 {
			finding = c
		}
	}
	if finding == nil {
		t.Fatalf("no finding: %+v", be.comments())
	}
	if err := be.s.write(be.ctx, SystemActor(), func(t *tx) error {
		_, err := be.s.setFindingState(be.ctx, t, finding.FindingID, FindingFixed, byVerify("m"), "8888888bbbb", "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		n := 0
		for _, r := range be.sg.replies {
			if strings.Contains(r, verifiedReply) {
				n++
			}
		}
		return n
	}
	be.step()
	if c := be.comment(finding.extID); c.Verdict != VerdictVerified || count() != 1 {
		t.Fatalf("verified = %+v replies %v", c, be.sg.replies)
	}
	be.step()
	if count() != 1 {
		t.Errorf("replied again: %v", be.sg.replies)
	}
}

// A round's own job token records verdicts on its pull request's audit,
// and nothing else: it still can't ask for audits, or touch another's.
func TestRoundMarksItsOwnAuditFindings(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	box := be.auditBox()
	be.auditOn()
	pr := be.openMine()
	be.start(pr)
	be.s.Wait()
	running := be.env.poll(be.admin, box, RunnerPoll{}).Job
	if running == nil {
		t.Fatal("no audit job")
	}
	be.ci("7777777aaaa", CheckSuccess)
	be.step()
	id := be.auditID()
	be.finishAudit(running, "done", [2]string{"high", "Anyone exports"})
	b := be.babysit()
	if b.JobID == 0 {
		t.Fatalf("no round: %+v", b)
	}
	token := func(job int64) *Actor {
		a := *be.agent
		a.RunnerJob, a.Permissions = job, permissionSet(jobTokenPermissions)
		return &a
	}
	mark := MarkFindings{IDs: []string{itoa64(id) + ".1"}, State: "fixed", Note: "Reverted."}
	var pe *PermissionError
	if _, err := be.s.MarkAuditFindings(be.ctx, token(b.JobID+999), mark); !errors.As(err, &pe) {
		t.Fatalf("another job's token: %v", err)
	}
	if _, err := be.s.MarkAuditFindings(be.ctx, token(b.JobID), mark); err != nil {
		t.Fatalf("the round's own token: %v", err)
	}
	if todo := be.s.auditTodo(be.ctx, pr.ID); len(todo) != 0 {
		t.Errorf("still to do: %+v", todo)
	}
}
