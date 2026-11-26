package core

import (
	"strings"
	"testing"
	"time"
)

const passingFault = "the model answered twice without opening any file"

// retryEnv is a pull request whose own audit was just asked for, with a
// runner that can take it.
func retryEnv(t *testing.T) (*bsEnv, *Runner, *PullRequest) {
	t.Helper()
	be := newBabysitEnv(t)
	be.runnerOnline()
	box := be.auditBox()
	be.auditOn()
	pr := be.openMine()
	be.s.Wait()
	return be, box, pr
}

// takeAudit is the box taking the queued audit job.
func (be *bsEnv) takeAudit(box *Runner) *RunnerClaim {
	be.t.Helper()
	be.s.Wait()
	w := be.env.poll(be.admin, box, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobAudit {
		be.t.Fatalf("no audit job to take: %+v", w.Job)
	}
	return w.Job
}

// failJob ends a taken audit job on an error.
func (be *bsEnv) failJob(job *RunnerClaim, why string) {
	be.t.Helper()
	if _, err := be.s.FinishRunnerJob(be.ctx, be.admin, job.ID, RunnerFinish{State: "failed", Result: []byte("{}"), Error: why}); err != nil {
		be.t.Fatal(err)
	}
	be.s.Wait()
	be.s.Wait()
}

// failChain fails the audit's job at each model until the audit stops asking
// for one.
func (be *bsEnv) failChain(box *Runner, why string) *CodeAudit {
	be.t.Helper()
	for range 8 {
		a := be.ownAudit()
		if a.Status != AuditRunning {
			return a
		}
		be.failJob(be.takeAudit(box), why)
	}
	be.t.Fatal("the chain never ended")
	return nil
}

func (be *bsEnv) ownAudit() *CodeAudit {
	be.t.Helper()
	a, err := be.s.auditRow(be.ctx, be.auditID())
	if err != nil {
		be.t.Fatal(err)
	}
	return a
}

// rerunDue moves the clock to the rerun's time and starts it.
func (be *bsEnv) rerunDue() {
	be.t.Helper()
	be.setClock(be.clockNow().Add(auditRetryStep + time.Minute))
	be.runnerOnline()
	be.s.CheckAudits(be.ctx)
	be.s.Wait()
}

// A pull request's own audit that failed on a passing fault runs its chain
// once more, later, and fails for good after the second pass (WP-1916).
func TestPRAuditRetriesOnceAfterAPassingFault(t *testing.T) {
	be, box, _ := retryEnv(t)
	got := be.failChain(box, passingFault)
	if got.Status != AuditQueued || got.Retries != 1 || got.RetryAt == nil || got.Slot != 0 {
		t.Fatalf("after the first pass: %s retries %d at %v slot %d %q", got.Status, got.Retries, got.RetryAt, got.Slot, got.Error)
	}
	be.rerunDue()
	got = be.failChain(box, passingFault)
	if got.Status != AuditFailed || got.Retries != 1 || !strings.Contains(got.Error, "tried 2 times") {
		t.Fatalf("after the second pass: %s retries %d %q %+v", got.Status, got.Retries, got.Error, got.Attempts)
	}
}

// The merge waits for the rerun, from the failure that queued it, however
// old the audit is by then, until its time to be taken has passed.
func TestPRAuditRerunHoldsTheMerge(t *testing.T) {
	be, box, pr := retryEnv(t)
	job := be.takeAudit(box)
	be.setClock(be.clockNow().Add(auditQueueWait + time.Minute))
	be.failJob(job, passingFault)
	got := be.failChain(box, passingFault)
	if got.Status != AuditQueued {
		t.Fatalf("no rerun: %s %q", got.Status, got.Error)
	}
	holds := func(when string, want bool) {
		t.Helper()
		_, going := be.s.auditGoing(be.ctx, pr.ID)
		_, other, err := be.s.auditGoingExceptFootprint(be.ctx, pr.ID)
		if err != nil || going != want || other != want {
			t.Fatalf("%s: holds %v and %v (%v), want %v", when, going, other, err, want)
		}
	}
	holds("waiting for its time", true)
	failed := be.clockNow()
	if at, err := be.s.auditDeadline(be.ctx, got.ID); err != nil || at == nil || at.Sub(failed.Add(auditRerunWait)).Abs() > time.Second {
		t.Errorf("deadline = %v (%v), want about %v", at, err, failed.Add(auditRerunWait))
	}
	be.rerunDue()
	holds("queued for a runner", true)
	be.setClock(failed.Add(auditRerunWait + time.Minute))
	holds("no runner took it", false)
}

// A runner that went silent has held the pull request long enough.
func TestPRAuditNotRerunAfterASlowPass(t *testing.T) {
	for name, why := range map[string]string{"timed out": "the audit timed out", "silent": ""} {
		t.Run(name, func(t *testing.T) {
			be, box, _ := retryEnv(t)
			var got *CodeAudit
			if why != "" {
				got = be.failChain(box, why)
			} else {
				for range 8 {
					job := be.takeAudit(box)
					if err := be.s.write(be.ctx, SystemActor(), func(tx *tx) error {
						return tx.finishRunnerJob(be.ctx, job.ID, "expired", nil, nil, "The runner didn't report back.")
					}); err != nil {
						t.Fatal(err)
					}
					be.s.CheckAudits(be.ctx)
					be.s.Wait()
					if got = be.ownAudit(); got.Status != AuditRunning {
						break
					}
				}
			}
			if got.Status != AuditFailed || got.Retries != 0 {
				t.Fatalf("%s retries %d %q %+v", got.Status, got.Retries, got.Error, got.Attempts)
			}
		})
	}
}

// Passes a busy model took don't use up the rerun.
func TestPRAuditRerunAfterABusyPass(t *testing.T) {
	be, box, _ := retryEnv(t)
	if got := be.failChain(box, "429 too many requests"); got.Status != AuditQueued || got.Retries != 1 {
		t.Fatalf("busy pass: %s retries %d %q", got.Status, got.Retries, got.Error)
	}
	be.rerunDue()
	if got := be.failChain(box, passingFault); got.Status != AuditQueued || got.Retries != 2 {
		t.Fatalf("passing fault after a busy pass: %s retries %d %q", got.Status, got.Retries, got.Error)
	}
}

func TestPRAuditNotRerunForAReplacedHead(t *testing.T) {
	be, box, _ := retryEnv(t)
	job := be.takeAudit(box)
	be.pushMine("8888888bbbb")
	be.failJob(job, passingFault)
	if got := be.failChain(box, passingFault); got.Status != AuditFailed {
		t.Fatalf("the old head's audit: %s retries %d", got.Status, got.Retries)
	}
}

// A rerun of an old head that ends after the new head's audit doesn't take
// its place: the new head's findings go on holding.
func TestStaleRerunDoesNotReplaceTheNewerAudit(t *testing.T) {
	be, box, pr := retryEnv(t)
	if got := be.failChain(box, passingFault); got.Status != AuditQueued {
		t.Fatalf("no rerun: %s %q", got.Status, got.Error)
	}
	old := be.auditID()
	be.rerunDue()
	oldJob := be.takeAudit(box)
	be.pushMine("8888888bbbb")
	be.ready(be.pr(7))
	if be.auditID() == old {
		t.Fatal("no audit of the new head")
	}
	be.endAudit(box, "done", [2]string{"high", "A stranger can export"})
	newer := be.auditID()
	be.setClock(be.clockNow().Add(time.Minute))
	be.finishAudit(oldJob, "done")
	if id, status, err := be.s.newestAudit(be.ctx, pr.ID); err != nil || id != newer || status != AuditDone {
		t.Fatalf("newest audit = %d %s (%v), want %d", id, status, err, newer)
	}
	if todo := be.s.auditTodo(be.ctx, pr.ID); len(todo) != 1 || todo[0].Audit != newer {
		t.Errorf("findings to do: %+v", todo)
	}
}

func TestAuditByHandNotRetriedAfterAPassingFault(t *testing.T) {
	ae := newAgentEnv(t)
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: []string{"grok-4.7-high"}})
	if err != nil {
		t.Fatal(err)
	}
	ae.work()
	ae.finish(ae.take().ID, "failed", map[string]any{}, passingFault)
	if got := ae.audit(a.ID); got.Status != AuditFailed || got.Retries != 0 || got.RetryAt != nil {
		t.Fatalf("audit by hand: %s retries %d at %v", got.Status, got.Retries, got.RetryAt)
	}
}

// A job that expired after a runner took it says the runner went silent, and
// the chain goes on to its next model (WP-1916).
func TestAuditJobExpiredAfterATakeGoesToTheNextModel(t *testing.T) {
	ae := newAgentEnv(t)
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: []string{"grok-4.7-high", "grok-4.7-medium"}})
	if err != nil {
		t.Fatal(err)
	}
	ae.work()
	claim := ae.take()
	if err := ae.s.write(ae.ctx, SystemActor(), func(tx *tx) error {
		return tx.finishRunnerJob(ae.ctx, claim.ID, "expired", nil, nil, "The runner didn't report back before the job's timeout (plus 5 minutes).")
	}); err != nil {
		t.Fatal(err)
	}
	ae.work()
	got := ae.audit(a.ID)
	if got.Status == AuditFailed || len(got.Attempts) != 1 || got.Attempts[0].Outcome != "unavailable" || !strings.Contains(got.Attempts[0].Error, "went silent") || got.Slot != 1 {
		t.Fatalf("after the expiry: %s slot %d %+v", got.Status, got.Slot, got.Attempts)
	}
	// The attempt says when its job was queued and when a runner took it, so
	// the wait for a runner can be told from the run.
	if at := got.Attempts[0]; at.QueuedAt == 0 || at.TakenAt < at.QueuedAt {
		t.Errorf("queued %d, taken %d", at.QueuedAt, at.TakenAt)
	}
}
