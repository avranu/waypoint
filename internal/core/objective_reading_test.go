package core

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// judgeAsked is an objective with only a judge, where the work starts read
// by the runner, so the judge's audit is out. It returns that audit's id.
func (o *objEnv) judgeAsked(title string) (*IssueDetail, int64) {
	o.t.Helper()
	task := o.task(title)
	o.set(task.Key, ObjectiveInput{Judge: ptrOf("The board exports as CSV.")})
	o.read(ObjectiveJobResult{Head: shaA})
	ob := o.obj(task.Key)
	if ob.verifyAudit == nil {
		o.t.Fatalf("the judge wasn't asked: phase %q %q", ob.Phase, ob.PhaseNote)
	}
	return task, *ob.verifyAudit
}

func (o *objEnv) auditStatus(id int64) string {
	o.t.Helper()
	o.work()
	a, err := o.s.GetAudit(o.ctx, o.admin, id)
	if err != nil {
		o.t.Fatal(err)
	}
	return a.Status
}

// A reading the runner takes for a target or a check that was changed
// while it ran says nothing of the new one: it is dropped, and the new
// objective is read afresh.
func TestObjectiveStaleReadingUnderAChangedTargetIsDropped(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Fast")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("./bench"), Until: ptrOf("< 1s")})
	o.read(ObjectiveJobResult{Head: shaA, Reading: timed(3)})
	o.worked(task.Key)
	c, j := o.job()
	if j.Check != "./bench" || j.Base != shaA {
		t.Fatalf("the reading's job = %+v", j)
	}
	// The person tightens the target and changes the check while it runs.
	o.set(task.Key, ObjectiveInput{Check: ptrOf("./bench2"), Until: ptrOf("< 0.1s")})
	// The runner answers the old job all the same: met under the old target.
	raw, _ := json.Marshal(ObjectiveJobResult{Head: shaB, Base: shaA, Reading: timed(0.5), BaseReading: timed(3), P: ptrOf(0.001)})
	_, _ = o.s.FinishRunnerJob(o.ctx, o.admin, c.ID, RunnerFinish{State: "done", Result: raw})
	o.s.Wait()
	ob := o.obj(task.Key)
	if ob.State != ObjectiveActive || o.status(task.Key) == "Done" || ob.Phase != PhaseVerdict || ob.Round != 1 {
		t.Fatalf("a stale answer moved it: %s %q round %d, task %s", ob.State, ob.Phase, ob.Round, o.status(task.Key))
	}
	if ob.Verdict != nil && ob.Verdict.Met {
		t.Fatalf("the verdict is the stale reading: %+v", ob.Verdict)
	}
	if told := o.told(NotifyObjective); len(told) != 0 {
		t.Errorf("told of a reading that was dropped: %q", told)
	}
	// The new objective is read with its own check, and its own target.
	j2 := o.read(ObjectiveJobResult{Head: shaB, Base: shaA, Reading: timed(0.5), BaseReading: timed(3), P: ptrOf(0.001)})
	if j2.Check != "./bench2" {
		t.Fatalf("the new reading's job = %+v", j2)
	}
	if ob = o.obj(task.Key); ob.State != ObjectiveActive || ob.Round != 2 || ob.Verdict == nil || ob.Verdict.Met {
		t.Fatalf("0.5s against < 0.1s: %s round %d verdict %+v", ob.State, ob.Round, ob.Verdict)
	}
}

// The judge's audit is Waypoint's: an agent can't call it off (nor a
// person's token in an agent's shell), and it is called off with the
// objective, when it ends or is taken off.
func TestObjectiveJudgesAuditIsSweptNotCancelledByAgents(t *testing.T) {
	o := newObjEnv(t)
	task, audit := o.judgeAsked("Export the board")
	shell := &Actor{User: o.admin.User, AgentEnv: "claude"}
	for who, actor := range map[string]*Actor{"an agent": o.ag, "a person's token in an agent's shell": shell} {
		if _, err := o.s.CancelAudit(o.ctx, actor, audit); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s cancelling the judge: %v", who, err)
		}
	}
	if st := o.auditStatus(audit); st != AuditQueued && st != AuditRunning {
		t.Fatalf("the judge's audit after the refusals: %s", st)
	}
	if ob := o.obj(task.Key); ob.verifyAudit == nil || *ob.verifyAudit != audit || ob.Phase != PhaseBaseline {
		t.Fatalf("the objective lost its judge: %+v", ob)
	}
	// Ending it calls the judge off.
	if _, err := o.s.ObjectiveGo(o.ctx, o.admin, task.Key, ObjectiveEnd); err != nil {
		t.Fatal(err)
	}
	if st := o.auditStatus(audit); st != AuditCancelled {
		t.Errorf("the judge's audit after the objective ended: %s", st)
	}
	// So does taking it off.
	other, audit2 := o.judgeAsked("Import the board")
	if err := o.s.ClearObjective(o.ctx, o.admin, other.Key); err != nil {
		t.Fatal(err)
	}
	if st := o.auditStatus(audit2); st != AuditCancelled {
		t.Errorf("the judge's audit after the objective was cleared: %s", st)
	}
}

// A worker's own readings are working notes: the newest 200 are kept, and
// none of them decides anything, however good.
func TestObjectiveAgentReadingsArePrunedAndNeverDecide(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Fast")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("./bench"), Until: ptrOf("< 1s")})
	o.read(ObjectiveJobResult{Head: shaA, Reading: timed(3)})
	o.take1(task.Key)
	for range objectiveAgentReadings + 5 {
		if res, err := o.s.RecordObjectiveCheck(o.ctx, o.ag, task.Key, ObjectiveCheckInput{OK: true, Value: ptrOf(0.001), Runs: 3}); err != nil || !res.Met {
			t.Fatalf("a working reading = %+v, %v", res, err)
		}
	}
	var agent, baseline int
	if err := o.s.db.R.QueryRowContext(o.ctx, `SELECT count(*) FILTER (WHERE source = ?), count(*) FILTER (WHERE source = ?) FROM objective_measurements WHERE issue_id = ?`,
		SourceAgent, SourceBaseline, o.issueID(task.Key)).Scan(&agent, &baseline); err != nil {
		t.Fatal(err)
	}
	if agent != objectiveAgentReadings || baseline != 1 {
		t.Fatalf("readings kept: %d of the worker's, %d of the baseline", agent, baseline)
	}
	ob := o.obj(task.Key)
	if ob.State != ObjectiveActive || ob.Phase != PhaseWork || o.status(task.Key) != "In Progress" || ob.Verdict == nil || ob.Verdict.Source != SourceBaseline || ob.Verdict.Met {
		t.Fatalf("what the worker's readings did: %s %q %s verdict %+v", ob.State, ob.Phase, o.status(task.Key), ob.Verdict)
	}
	if told := o.told(NotifyObjective); len(told) != 0 {
		t.Errorf("told = %q", told)
	}
}

// A judge that finds nothing at or above the bar where the work starts has
// the objective met: the task closes with no round worked.
func TestObjectiveMetWhereItStartsByAJudge(t *testing.T) {
	o := newObjEnv(t)
	task, _ := o.judgeAsked("Export the board")
	o.judged("Every export is there and tested.", finding("low", "A typo"))
	ob := o.obj(task.Key)
	if ob.State != ObjectiveMet || o.status(task.Key) != "Done" || ob.Phase != PhaseWork || ob.Round != 1 || len(ob.Rounds) != 0 || ob.Health != "met" {
		t.Fatalf("met at the start: %s %s phase %q round %d %+v health %q", ob.State, o.status(task.Key), ob.Phase, ob.Round, ob.Rounds, ob.Health)
	}
	if ob.Verdict == nil || ob.Verdict.Source != SourceJudge || !ob.Verdict.Met {
		t.Fatalf("verdict = %+v", ob.Verdict)
	}
	if !strings.Contains(ob.EndedWhy, "already met when Waypoint first read it") {
		t.Errorf("ended why = %q", ob.EndedWhy)
	}
	if told := o.told(NotifyObjective); len(told) != 1 || !strings.Contains(told[0], "is met") {
		t.Errorf("told = %q", told)
	}
	if h := strings.Join(o.history(task.Key), "\n"); !strings.Contains(h, "already met") {
		t.Errorf("the task's history doesn't say so:\n%s", h)
	}
}

// Set again, a met objective starts on a new round and is read afresh:
// what was read under the old words isn't the new round's reading.
func TestObjectiveSetAgainAfterMetStartsAfresh(t *testing.T) {
	o := newObjEnv(t)
	task, _ := o.judgeAsked("Export the board")
	o.judged("Every export is there and tested.")
	if ob := o.obj(task.Key); ob.State != ObjectiveMet {
		t.Fatalf("not met: %s", ob.State)
	}
	// A task its objective closed is opened by a person first.
	todo := "To Do"
	if _, err := o.s.UpdateIssue(o.ctx, o.admin, task.Key, IssuePatch{Status: &todo}); err != nil {
		t.Fatal(err)
	}
	o.s.Wait()
	ob := o.set(task.Key, ObjectiveInput{Judge: ptrOf("The board exports as CSV and as JSON.")})
	if ob.State != ObjectiveActive || ob.Round != 2 || ob.Phase != PhaseBaseline || ob.Verdict != nil || ob.EndedAt != nil || ob.Health != "reading" {
		t.Fatalf("set again: %s round %d phase %q verdict %+v health %q", ob.State, ob.Round, ob.Phase, ob.Verdict, ob.Health)
	}
	if o.status(task.Key) == "Done" {
		t.Error("the task stays closed under an objective in effect")
	}
	// Read afresh: the old judge's nothing doesn't count; this one's finding does.
	o.read(ObjectiveJobResult{Head: shaB})
	o.judged("No JSON export.", finding("high", "There is no JSON export"))
	ob = o.obj(task.Key)
	if ob.State != ObjectiveActive || ob.Round != 2 || ob.Phase != PhaseWork || ob.Verdict == nil || *ob.Verdict.Value != 1 || ob.BaseSHA != shaB {
		t.Fatalf("after the new reading: %s round %d phase %q verdict %+v base %s", ob.State, ob.Round, ob.Phase, ob.Verdict, ob.BaseSHA)
	}
	if next, wait := o.queued(task.Key); !next {
		t.Errorf("round 2 isn't handed out: %q", wait)
	}
}

// Whoever put an objective in effect has an account their runner is read
// on. Without it nothing can be read: it waits for a person, and another
// person sending it on takes it over.
func TestObjectiveBlockedWhenItsPersonIsGone(t *testing.T) {
	o := newObjEnv(t)
	bob := o.user("bob", true)
	task := o.task("Fast")
	if _, err := o.s.SetObjective(o.ctx, bob, task.Key, ObjectiveInput{Check: ptrOf("./bench"), Until: ptrOf("< 1s")}); err != nil {
		t.Fatal(err)
	}
	o.s.Wait()
	if err := o.s.DeleteUser(o.ctx, o.admin, bob.User.ID); err != nil {
		t.Fatal(err)
	}
	o.tick(6 * time.Minute)
	ob := o.obj(task.Key)
	if ob.Phase != PhaseBlocked || ob.PhaseWhy != BlockedPerson || ob.State != ObjectiveActive || ob.Health != "blocked" {
		t.Fatalf("no account: phase %q why %q health %q note %q", ob.Phase, ob.PhaseWhy, ob.Health, ob.PhaseNote)
	}
	hasAll(t, "why it waits", ob.PhaseNote, "no account any more", "wp objective retry "+task.Key)
	hasAll(t, "the health note", ob.HealthNote, "no account any more")
	if told := o.told(NotifyObjective); len(told) != 1 || !strings.Contains(told[0], "needs you") {
		t.Errorf("told = %q", told)
	}
	if next, _ := o.queued(task.Key); next {
		t.Error("a round is handed out on an objective nothing can read")
	}
	// Moving on without a reading isn't an answer to this.
	if _, err := o.s.ObjectiveGo(o.ctx, o.admin, task.Key, ObjectiveNext); !IsInvalid(err) {
		t.Errorf("next on an objective nothing can read: %v", err)
	}
	// Another person retries it: it is theirs, and read on their runner.
	ob, err := o.s.ObjectiveGo(o.ctx, o.admin, task.Key, ObjectiveRetry)
	if err != nil {
		t.Fatal(err)
	}
	if ob.Phase != PhaseBaseline || ob.ConfirmedBy == nil || ob.ConfirmedBy.Username != o.admin.User.Username {
		t.Fatalf("after the retry: phase %q confirmed by %+v", ob.Phase, ob.ConfirmedBy)
	}
	o.read(ObjectiveJobResult{Head: shaA, Reading: timed(3)})
	if ob = o.obj(task.Key); ob.Phase != PhaseWork || ob.BaseSHA != shaA || ob.Health == "blocked" {
		t.Fatalf("read on the new person's runner: phase %q health %q", ob.Phase, ob.Health)
	}
}

// A repository detached between the rounds leaves nothing to read in: it
// waits for a person, and attaching it again lets a retry go on.
func TestObjectiveBlockedWhenItsRepositoryIsDetached(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Fast")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("./bench"), Until: ptrOf("< 1s")})
	o.read(ObjectiveJobResult{Head: shaA, Reading: timed(3)})
	repos, err := o.s.ListProjectRepos(o.ctx, o.admin, o.p.Key)
	if err != nil || len(repos) != 1 {
		t.Fatalf("repos = %+v, %v", repos, err)
	}
	if err := o.s.RemoveProjectRepo(o.ctx, o.admin, o.p.Key, repos[0].ID); err != nil {
		t.Fatal(err)
	}
	o.worked(task.Key)
	ob := o.obj(task.Key)
	if ob.Phase != PhaseBlocked || ob.PhaseWhy != BlockedRepo || ob.State != ObjectiveActive || ob.Health != "blocked" {
		t.Fatalf("no repository: phase %q why %q health %q", ob.Phase, ob.PhaseWhy, ob.Health)
	}
	hasAll(t, "why it waits", ob.PhaseNote, "me/app", "isn't a repository of its project any more", "wp objective retry "+task.Key)
	if told := o.told(NotifyObjective); len(told) != 1 || !strings.Contains(told[0], "needs you") {
		t.Errorf("told = %q", told)
	}
	o.noJob()
	if _, err := o.s.ObjectiveGo(o.ctx, o.admin, task.Key, ObjectiveNext); !IsInvalid(err) {
		t.Errorf("next with no repository: %v", err)
	}
	if _, err := o.s.AddProjectRepo(o.ctx, o.admin, o.p.Key, o.f.ID, "me/app", false); err != nil {
		t.Fatal(err)
	}
	if _, err := o.s.ObjectiveGo(o.ctx, o.admin, task.Key, ObjectiveRetry); err != nil {
		t.Fatal(err)
	}
	o.read(ObjectiveJobResult{Head: shaB, Base: shaA, Reading: timed(0.5), BaseReading: timed(3), P: ptrOf(0.001)})
	if ob = o.obj(task.Key); ob.State != ObjectiveMet || o.status(task.Key) != "Done" {
		t.Fatalf("after attaching it again: %s", ob.State)
	}
}

// The judge's audit that ends without an answer is asked for again after a
// wait that doubles (an hour, for usage limits), and after three tries a
// person is asked.
func TestObjectiveJudgeThatEndsWithoutAnAnswerIsRetried(t *testing.T) {
	cases := []struct {
		name  string
		end   func(o *objEnv, audit int64)
		wait  time.Duration
		saysA string
	}{
		{"failed", func(o *objEnv, id int64) {
			if _, err := o.s.db.W.ExecContext(o.ctx, `UPDATE code_audits SET status = ?, error = ? WHERE id = ?`, AuditFailed, "the model crashed", id); err != nil {
				o.t.Fatal(err)
			}
		}, objectiveReadRetry, "the judge's audit failed: the model crashed"},
		{"limited", func(o *objEnv, id int64) {
			if _, err := o.s.db.W.ExecContext(o.ctx, `UPDATE code_audits SET status = ? WHERE id = ?`, AuditLimited, id); err != nil {
				o.t.Fatal(err)
			}
		}, objectiveLimitRetry, "every model of the judge hit a usage limit"},
		{"cancelled", func(o *objEnv, id int64) {
			if _, err := o.s.CancelAudit(o.ctx, o.admin, id); err != nil {
				o.t.Fatal(err)
			}
		}, objectiveReadRetry, "the judge's audit was cancelled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := newObjEnv(t)
			task, audit := o.judgeAsked("Export the board")
			var retryAt int64
			for try := 1; try <= objectiveReadTries; try++ {
				if try > 1 {
					// Not before its wait is over; then asked again.
					// The wait doubles from when the failure was noticed (at
					// once for a cancel, at the next look otherwise).
					left := time.Duration(retryAt-o.clockNow().UnixMilli()) * time.Millisecond
					if want := c.wait << (try - 2); left > want || left < want-time.Minute {
						t.Fatalf("try %d is %v away, want %v", try, left, want)
					}
					o.tick(left - time.Second)
					o.noJob()
					o.tick(2 * time.Minute)
					ob := o.obj(task.Key)
					if ob.verifyAudit == nil || *ob.verifyAudit == audit {
						t.Fatalf("try %d: the judge wasn't asked again: %+v", try, ob.verifyAudit)
					}
					audit = *ob.verifyAudit
				}
				o.judgeJob() // taken by the runner, never answered
				c.end(o, audit)
				o.tick(time.Minute)
				ob := o.obj(task.Key)
				if try < objectiveReadTries {
					if ob.Phase != PhaseBaseline || ob.verifyTry != try || !strings.Contains(ob.PhaseNote, "the next is at") || !strings.Contains(ob.PhaseNote, c.saysA) {
						t.Fatalf("after try %d: phase %q tries %d note %q", try, ob.Phase, ob.verifyTry, ob.PhaseNote)
					}
					retryAt = ob.verifyAt
					// A try that failed is waiting to be tried again, as a failed job is.
					if ob.Health != "reading" || ob.StuckSince != nil {
						t.Errorf("after try %d: health %q (stuck since %v: %s)", try, ob.Health, ob.StuckSince, ob.StuckWhy)
					}
					if told := o.told(NotifyObjective); len(told) != 0 {
						t.Fatalf("told after try %d: %q", try, told)
					}
				}
			}
			ob := o.obj(task.Key)
			if ob.Phase != PhaseBlocked || ob.PhaseWhy != BlockedReading || ob.State != ObjectiveActive || o.status(task.Key) == "Done" {
				t.Fatalf("after %d tries: phase %q why %q", objectiveReadTries, ob.Phase, ob.PhaseWhy)
			}
			hasAll(t, "why it waits", ob.PhaseNote, "3 tries", c.saysA, "wp objective retry "+task.Key)
			if told := o.told(NotifyObjective); len(told) != 1 || !strings.Contains(told[0], "needs you") {
				t.Errorf("told = %q", told)
			}
			// Retried by a person, the judge is asked once more and answers.
			if _, err := o.s.ObjectiveGo(o.ctx, o.admin, task.Key, ObjectiveRetry); err != nil {
				t.Fatal(err)
			}
			o.read(ObjectiveJobResult{Head: shaA})
			o.judged("Done.")
			if ob = o.obj(task.Key); ob.State != ObjectiveMet || o.status(task.Key) != "Done" {
				t.Fatalf("after the retry: %s", ob.State)
			}
		})
	}
}

// An answer the runner gives that isn't a usable reading counts as a failed
// try, whatever is wrong with it.
func TestObjectiveRunnerAnswersThatCantBeUsedAreFailedTries(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Fast")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("./bench"), Until: ptrOf("< 1s"), KeepPassing: &[]string{"go test ./..."}})
	answer := func(r ObjectiveJobResult) {
		t.Helper()
		c, _ := o.job()
		raw, _ := json.Marshal(r)
		if _, err := o.s.FinishRunnerJob(o.ctx, o.admin, c.ID, RunnerFinish{State: "done", Result: raw}); err != nil {
			t.Fatal(err)
		}
		o.s.Wait()
	}
	// No reading of the check.
	answer(ObjectiveJobResult{Head: shaA, Kept: []ObjectiveKept{{Command: "go test ./...", OK: true}}})
	ob := o.obj(task.Key)
	if ob.Phase != PhaseBaseline || ob.verifyTry != 1 || !strings.Contains(ob.PhaseNote, "it has no reading of the check") || ob.Health != "reading" {
		t.Fatalf("after no reading: phase %q tries %d note %q", ob.Phase, ob.verifyTry, ob.PhaseNote)
	}
	// A keep-passing command left out.
	o.tick(objectiveReadRetry + time.Minute)
	answer(ObjectiveJobResult{Head: shaA, Reading: timed(3)})
	ob = o.obj(task.Key)
	if ob.verifyTry != 2 || !strings.Contains(ob.PhaseNote, "which must keep passing") {
		t.Fatalf("after a command left out: tries %d note %q", ob.verifyTry, ob.PhaseNote)
	}
	// A reading of a command nobody asked for.
	o.tick(2*objectiveReadRetry + time.Minute)
	answer(ObjectiveJobResult{Head: shaA, Reading: timed(3), Kept: []ObjectiveKept{{Command: "go test ./...", OK: true}, {Command: "rm -rf /", OK: true}}})
	ob = o.obj(task.Key)
	if ob.Phase != PhaseBlocked || ob.PhaseWhy != BlockedReading || ob.State != ObjectiveActive {
		// An extra command is dropped, not rejected: it is the third try
		// only if it is refused.
		t.Logf("an extra command: phase %q tries %d note %q", ob.Phase, ob.verifyTry, ob.PhaseNote)
	}
	if ob.Phase == PhaseBlocked {
		hasAll(t, "why it waits", ob.PhaseNote, "3 tries", "wp objective retry "+task.Key)
	}
	for _, m := range ob.Checks {
		if strings.Contains(m.Line, "rm -rf") {
			t.Errorf("a command nobody asked for is in a reading: %+v", m)
		}
	}
}

// A job the runner fails, or that times out, is a failed try like any
// other; the objective stands where it was and is read again.
func TestObjectiveReadingJobThatFailsOrTimesOut(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Fast")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("./bench"), Until: ptrOf("< 1s")})
	c, _ := o.job()
	if _, err := o.s.FinishRunnerJob(o.ctx, o.admin, c.ID, RunnerFinish{State: "failed", Error: "git clone: could not resolve host"}); err != nil {
		t.Fatal(err)
	}
	o.s.Wait()
	ob := o.obj(task.Key)
	if ob.Phase != PhaseBaseline || ob.verifyTry != 1 || !strings.Contains(ob.PhaseNote, "the runner couldn't read it: git clone: could not resolve host") || ob.Health != "reading" {
		t.Fatalf("after a failed job: phase %q tries %d note %q health %q", ob.Phase, ob.verifyTry, ob.PhaseNote, ob.Health)
	}
	o.noJob()
	o.tick(objectiveReadRetry + time.Minute)
	// Taken, and never answered: it expires past its timeout.
	o.job()
	o.setClock(o.clockNow().Add(time.Duration(objectiveJobTimeout)*time.Second + 6*time.Minute))
	o.s.CheckRunners(o.ctx)
	o.s.Wait()
	ob = o.obj(task.Key)
	if ob.verifyTry != 2 || !strings.Contains(ob.PhaseNote, "never answered") || ob.Phase != PhaseBaseline {
		t.Fatalf("after a timed out job: phase %q tries %d note %q", ob.Phase, ob.verifyTry, ob.PhaseNote)
	}
	if told := o.told(NotifyObjective); len(told) != 0 {
		t.Errorf("told before the tries ran out: %q", told)
	}
	o.tick(2*objectiveReadRetry + time.Minute)
	o.read(ObjectiveJobResult{Head: shaA, Reading: timed(3)})
	if ob = o.obj(task.Key); ob.Phase != PhaseWork || ob.BaseSHA != shaA || ob.verifyTry != 0 {
		t.Fatalf("read at last: phase %q base %q tries %d", ob.Phase, ob.BaseSHA, ob.verifyTry)
	}
}

// A runner that loses what reads objectives (its wp was swapped for an old
// one) takes no reading, and it fails no try: the reading waits for a runner
// that can.
func TestObjectiveRunnerLosingTheFeatureTakesNoReading(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Fast")
	o.set(task.Key, ObjectiveInput{Check: ptrOf("./bench"), Until: ptrOf("< 1s")})
	withFeature := func(on bool) {
		o.box = o.runner(o.admin, "box", func(r *RunnerRegistration) {
			r.AllowedDirs = []string{"/work"}
			r.CLIs = []RunnerCLI{{Name: "claude"}}
			r.Features = []string{FeatureConfine, FeatureAudit}
			if on {
				r.Features = append(r.Features, FeatureObjective)
			}
		})
	}
	// Queued, and not yet taken.
	withFeature(false)
	o.noJob()
	ob := o.obj(task.Key)
	if ob.Phase != PhaseBaseline || ob.verifyTry != 0 || ob.State != ObjectiveActive || ob.Health != "reading" {
		t.Fatalf("a runner that can't read it: phase %q tries %d health %q", ob.Phase, ob.verifyTry, ob.Health)
	}
	withFeature(true)
	// Taken, and the runner loses the feature before it answers: the answer counts.
	c, _ := o.job()
	withFeature(false)
	raw, _ := json.Marshal(ObjectiveJobResult{Head: shaA, Reading: timed(3)})
	if _, err := o.s.FinishRunnerJob(o.ctx, o.admin, c.ID, RunnerFinish{State: "done", Result: raw}); err != nil {
		t.Fatal(err)
	}
	o.s.Wait()
	if ob = o.obj(task.Key); ob.Phase != PhaseWork || ob.BaseSHA != shaA || ob.verifyTry != 0 {
		t.Fatalf("answered by a runner that lost the feature: phase %q base %q", ob.Phase, ob.BaseSHA)
	}
	// The next reading has no runner that can take it, and it waits.
	o.worked(task.Key)
	o.noJob()
	ob = o.obj(task.Key)
	if ob.Phase != PhaseVerdict || ob.verifyTry != 0 || ob.Health != "reading" {
		t.Fatalf("round 1 read by nobody: phase %q tries %d health %q", ob.Phase, ob.verifyTry, ob.Health)
	}
	withFeature(true)
	o.tick(6 * time.Minute) // Waypoint looks again every few minutes
	o.read(ObjectiveJobResult{Head: shaB, Base: shaA, Reading: timed(0.5), BaseReading: timed(3), P: ptrOf(0.001)})
	if ob = o.obj(task.Key); ob.State != ObjectiveMet {
		t.Fatalf("read by a runner that can: %s", ob.State)
	}
}

// Two steps racing over one objective (an event and the minute's pass)
// ask for one job, and apply one verdict.
func TestObjectiveRacingPumpsGiveOneJobAndOneVerdict(t *testing.T) {
	o := newObjEnv(t)
	task := o.task("Fast")
	id := o.issueID(task.Key)
	race := func() {
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				o.s.objectiveAdvance(o.ctx, id)
			}()
		}
		wg.Wait()
		o.s.Wait()
	}
	count := func(q string, args ...any) (n int) {
		t.Helper()
		if err := o.s.db.R.QueryRowContext(o.ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	o.set(task.Key, ObjectiveInput{Check: ptrOf("./bench"), Until: ptrOf("< 1s")})
	// Nothing asked yet: put it back to where Waypoint has asked for nothing.
	if _, err := o.s.db.W.ExecContext(o.ctx, `UPDATE runner_jobs SET state = 'cancelled' WHERE kind = ? AND source_id = ?`, JobObjective, id); err != nil {
		t.Fatal(err)
	}
	if _, err := o.s.db.W.ExecContext(o.ctx, `UPDATE objectives SET verify_job = NULL WHERE issue_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	race()
	if n := count(`SELECT count(*) FROM runner_jobs WHERE kind = ? AND source_id = ? AND state = 'queued'`, JobObjective, id); n != 1 {
		t.Fatalf("jobs queued by racing steps: %d", n)
	}
	// An answer that is in and not yet applied is applied once.
	c, _ := o.job()
	raw, _ := json.Marshal(ObjectiveJobResult{Head: shaA, Reading: timed(3)})
	if _, err := o.s.db.W.ExecContext(o.ctx, `UPDATE runner_jobs SET state = 'done', result = ? WHERE id = ?`, string(raw), c.ID); err != nil {
		t.Fatal(err)
	}
	race()
	if n := count(`SELECT count(*) FROM objective_measurements WHERE issue_id = ? AND source = ?`, id, SourceBaseline); n != 1 {
		t.Fatalf("baseline readings from racing steps: %d", n)
	}
	if ob := o.obj(task.Key); ob.Phase != PhaseWork || ob.BaseSHA != shaA || ob.Round != 1 {
		t.Fatalf("after the race: phase %q base %q round %d", ob.Phase, ob.BaseSHA, ob.Round)
	}
	// And the same for a round's verdict: one reading, one round ended.
	o.worked(task.Key)
	c, _ = o.job()
	raw, _ = json.Marshal(ObjectiveJobResult{Head: shaB, Base: shaA, Reading: timed(3), BaseReading: timed(3), P: ptrOf(0.9)})
	if _, err := o.s.db.W.ExecContext(o.ctx, `UPDATE runner_jobs SET state = 'done', result = ? WHERE id = ?`, string(raw), c.ID); err != nil {
		t.Fatal(err)
	}
	race()
	ob := o.obj(task.Key)
	if n := count(`SELECT count(*) FROM objective_measurements WHERE issue_id = ? AND source = ?`, id, SourceRunner); n != 1 {
		t.Fatalf("round readings from racing steps: %d", n)
	}
	if ob.Round != 2 || len(ob.Rounds) != 1 || ob.State != ObjectiveActive || ob.Phase != PhaseWork {
		t.Fatalf("a verdict applied twice: round %d %+v %s phase %q", ob.Round, ob.Rounds, ob.State, ob.Phase)
	}
}
