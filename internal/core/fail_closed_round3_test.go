package core

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Round 3 of WP-1406: a read that fails is never the permissive answer.

// failingQueryer fails every read, as a database that went away does.
type failingQueryer struct{ queryer }

func (failingQueryer) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, errors.New("database is gone")
}

// A failed read of the working statuses is neither "handed on" (which
// drops an agent's claim as done and takes the issue out of stranded
// work) nor "still working": the caller gets the error and does nothing.
func TestHandedOnUnreadableIsNotHandedOn(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "App", "")
	d := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Work", Agent: "claude"})
	i, err := e.s.issueByID(e.ctx, e.s.db.R, d.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	handed, err := e.s.handedOn(e.ctx, failingQueryer{e.s.db.R}, i)
	if err == nil {
		t.Fatal("handedOn: an unreadable status table gave an answer")
	}
	if handed {
		t.Error("handedOn: an unreadable status table read as handed on")
	}
	if handed, err := e.s.handedOn(e.ctx, e.s.db.R, i); err != nil || handed {
		t.Errorf("handedOn on a readable table = %v, %v; want false, nil", handed, err)
	}
}

// A forge read that fails is not "not behind": the readiness holds, and a
// base the forge doesn't have is the only answer that is no reason to wait.
func TestBehindBaseForgeFaultHolds(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	pr := &PullRequest{ForgeID: se.f.ID, Repo: "me/app", BaseRef: "main", HeadSHA: "1111111aaaa"}
	logs := &lockedBuf{}
	se.s.log = slog.New(slog.NewTextHandler(logs, nil))

	se.sg.behindErr = &forgeAPIError{method: "GET", path: "/branches/main", status: 502, body: "bad gateway"}
	if _, _, _, err := se.s.behindBaseErr(se.ctx, pr); err == nil {
		t.Fatal("behindBaseErr: a forge that failed to answer read as not behind")
	}
	if _, behind, stale := se.s.behindBase(se.ctx, pr); behind || !stale {
		t.Errorf("behindBase: behind=%v stale=%v, want a base not known yet (waits)", behind, stale)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "number=0") {
		t.Errorf("no Warn naming the pull request:\n%s", logs.String())
	}

	// A base the forge doesn't have is an answer.
	se.sg.behindErr = &forgeAPIError{method: "GET", path: "/branches/main", status: 404, body: "not found"}
	if _, behind, stale, err := se.s.behindBaseErr(se.ctx, pr); err != nil || behind || stale {
		t.Errorf("a 404: behind=%v stale=%v err=%v, want no reason to wait", behind, stale, err)
	}
}

// An armed, otherwise green pull request whose base can't be read from the
// forge is held (not merged on a guess), and is looked at again.
func TestArmedMergeHeldWhenBaseUnreadable(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.noWaits()
	const head = "1111111aaaa"
	se.sg.behindErr = &forgeAPIError{method: "GET", path: "/branches/main", status: 502, body: "bad gateway"}
	se.pullHook("opened", testPR)
	se.drain()
	se.sg.setChecks(head, CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	se.poll()
	id := se.pr(5).ID
	if r := se.reasons(5); !strings.Contains(r, "behind main") || !strings.Contains(r, "couldn't be read") {
		t.Fatalf("reasons = %q, want a hold that the base couldn't be read", r)
	}
	se.s.maybeAutoMerge(se.ctx, id)
	if len(se.sg.merged()) != 0 {
		t.Fatalf("merged on an unreadable base: %+v", se.sg.merged())
	}
	se.sg.behindErr = nil
	se.s.maybeAutoMerge(se.ctx, id)
	if len(se.sg.merged()) != 1 {
		t.Errorf("not merged once the base read again: %+v", se.sg.merged())
	}
}

// The merge step's look at its pull request: a missing one is gone, a read
// that failed is an error, so the step neither reports "it was closed" nor
// takes the arming off on a guess.
func TestMergeStepReloadUnreadableIsNotGone(t *testing.T) {
	ae := newAgentEnv(t)
	pr := ae.openPR5()
	c := &StepCtx{Ctx: ae.ctx, S: ae.s}
	if p, err := c.reloadPR(pr.ID); err != nil || p == nil {
		t.Fatalf("reloadPR of a live pull request = %v, %v", p, err)
	}
	if p, err := c.reloadPR(pr.ID + 1000); err != nil || p != nil {
		t.Fatalf("reloadPR of a missing pull request = %v, %v; want nil, nil (gone)", p, err)
	}
	defer ae.breakTable("pull_requests")()
	if p, err := c.reloadPR(pr.ID); err == nil {
		t.Fatalf("reloadPR with an unreadable table = %v, nil; it read as gone", p)
	}
}

// A merge queue stack whose pull request's babysit can't be read isn't
// discarded as "left the merge queue": it stands, and is looked at again.
func TestSweepStacksKeepsStackWhenQueueUnreadable(t *testing.T) {
	se := newStackEnv(t)
	se.draftGreen(head7s, 5)
	se.draftGreen(head8s, 7)
	se.draftGreen(head9s, 9)
	se.step()
	se.stackRun(0, CheckSuccess, treeA, "7 "+head7s, "8 "+head8s)
	se.stackRun(1, CheckSuccess, treeB, "7 "+head7s, "8 "+head8s, "9 "+head9s)
	se.step()
	before := stackStates(se.stacks())
	se.s.sweepStacks(se.ctx)
	if got := stackStates(se.stacks()); got != before {
		t.Fatalf("a healthy sweep changed the stacks: %s -> %s", before, got)
	}
	restore := se.breakTable("babysits")
	se.s.sweepStacks(se.ctx)
	restore()
	if got := stackStates(se.stacks()); got != before {
		t.Errorf("an unreadable babysit ended a live stack: %s -> %s", before, got)
	}
}

// inQueue and earlyRoundDue say so when the babysit can't be read, instead of
// "left the queue" and "no round due".
func TestInQueueAndEarlyRoundDueUnreadable(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.openMine()
	cur := be.pr(pr.Number)
	now := be.s.now()
	cur.QueuedAt, cur.ArmedAt = &now, &now
	defer be.breakTable("babysits")()
	if in, err := be.s.inQueue(be.ctx, cur); err == nil || in {
		t.Errorf("inQueue = %v, %v with an unreadable babysit; want an error", in, err)
	}
	if due, err := be.s.earlyRoundDue(be.ctx, cur); err == nil || due {
		t.Errorf("earlyRoundDue = %v, %v with an unreadable babysit; want an error", due, err)
	}
}

// A compare that fails keeps the branch (not known to be merged); a repo the
// forge no longer has is an answer, so it isn't warned of every sweep, while
// a real fault is.
func TestBranchPlanCompareFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		warn bool
	}{
		{"fault", &forgeAPIError{method: "GET", path: "/compare", status: 502, body: "bad gateway"}, true},
		{"gone", &forgeAPIError{method: "GET", path: "/compare", status: 404, body: "not found"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			se := newShipEnv(t, "forgejo")
			lf := newLandForge(se)
			logs := &lockedBuf{}
			se.s.log = slog.New(slog.NewTextHandler(logs, nil))
			lf.setBranches(ForgeBranch{Name: "main", SHA: "aaaaaaaa000"}, ForgeBranch{Name: "old", SHA: "bbbbbbbb111"})
			lf.times["bbbbbbbb111"] = se.clockNow().Add(-72 * time.Hour)
			lf.landed("main", "bbbbbbbb111")
			lf.aheadErr = tc.err
			plan, err := se.s.RemoteTidy(se.ctx, se.admin, "", "me/app")
			if err != nil || len(plan) != 1 {
				t.Fatalf("plan %v %v", plan, err)
			}
			for _, r := range plan[0].Rows {
				if r.Branch == "old" && r.Verdict == BranchRemovable {
					t.Errorf("a branch not known to be merged is removable: %+v", r)
				}
			}
			warned := strings.Contains(logs.String(), "branch is ahead")
			if warned != tc.warn {
				t.Errorf("warned=%v, want %v:\n%s", warned, tc.warn, logs.String())
			}
		})
	}
}

// A repo the forge no longer has is an answer for the pull request refresh,
// not a warning every tick.
func TestRefreshGoneRepoIsNotWarned(t *testing.T) {
	pe := newPREnv(t, "github")
	logs := &lockedBuf{}
	pe.s.log = slog.New(slog.NewTextHandler(logs, nil))
	pe.fg.mu.Lock()
	pe.fg.listErr = &forgeAPIError{method: "GET", path: "/repos/me/app/pulls", status: 404, body: "not found"}
	pe.fg.mu.Unlock()
	pe.s.RefreshPullRequests(pe.ctx)
	if out := logs.String(); strings.Contains(out, "level=WARN") && strings.Contains(out, "list pull requests") {
		t.Errorf("a repo gone from the forge was warned of:\n%s", out)
	}
}

// A read that fails inside mergeNow (after the forge was asked, before the
// merge) is a held merge that tries again, not a plain error that
// maybeAutoMerge reports as "couldn't merge" and asks a person about.
func TestMergeNowReadFaultRetries(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	se.noWaits()
	const head = "1111111aaaa"
	se.pullHook("opened", testPR)
	se.drain()
	se.sg.setChecks(head, CommitCheck{Name: "ci / backend (pull_request)", State: CheckSuccess})
	pr := se.pr(5)
	for _, table := range []string{"project_repos", "pull_requests", "forge_people"} {
		t.Run(table, func(t *testing.T) {
			restore := se.breakTable(table)
			_, err := se.s.mergeNow(se.ctx, se.admin, pr, MergeInput{armed: true}, "")
			restore()
			if err == nil {
				t.Fatal("mergeNow went on with a table it couldn't read")
			}
			if !errors.Is(err, ErrUnavailable) || IsInvalid(err) {
				t.Errorf("a read that failed is a plain error: %v", err)
			}
			if len(se.sg.merged()) != 0 {
				t.Errorf("merged: %+v", se.sg.merged())
			}
		})
	}
}

// A verdict whose giver can't be read isn't "nobody's call": an agent may not
// change what might be a person's verdict, and the assessor isn't handed it.
func TestVerdictByPersonUnreadable(t *testing.T) {
	be, ref := restedOnComment(t, func(be *bsEnv) *Actor { return be.admin })
	id := be.comment("501").id
	if by, err := be.s.verdictByPerson(be.ctx, be.s.db.R, id); err != nil || !by {
		t.Fatalf("verdictByPerson = %v, %v; want true, nil", by, err)
	}
	if by, err := be.s.verdictByPerson(be.ctx, be.s.db.R, id+999); err != nil || by {
		t.Fatalf("no events: %v, %v; want false, nil", by, err)
	}
	restore := be.breakTable("review_comment_events")
	defer restore()
	if by, err := be.s.verdictByPerson(be.ctx, be.s.db.R, id); err == nil || by {
		t.Errorf("verdictByPerson = %v, %v with an unreadable table; want an error", by, err)
	}
	if _, _, err := be.s.loadComments(be.ctx, be.agent, []string{ref}, true); err == nil {
		t.Error("an agent was let to change a comment whose verdict giver couldn't be read")
	}
	if got := be.s.heldByAgent(be.ctx, be.babysitOf(7), be.pr(7)); len(got) != 0 {
		t.Errorf("heldByAgent handed %v to the assessor with an unreadable giver", got)
	}
}

// A runner job that can't be looked up isn't a job that finished: the
// babysit stays on its round and starts no second one beside it.
func TestBabysitJobUnreadableIsNotFinished(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.sg.sm.Lock()
	be.sg.logs["ci / test"] = "--- FAIL: TestExport\n"
	be.sg.sm.Unlock()
	be.ci("7777777aaaa", CheckFailed)
	b := be.babysit()
	jobs := be.jobs()
	if b.State != BabysitWorking || len(jobs) != 1 || b.JobID != jobs[0] {
		t.Fatalf("after a failed check: %+v jobs %v", b, jobs)
	}
	old := lookupRunnerJob
	defer func() { lookupRunnerJob = old }()
	lookupRunnerJob = func(context.Context, *Service, int64) (*RunnerJob, error) { return nil, errors.New("database is gone") }
	// The job finished (its event is missed) and the look at it fails.
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE runner_jobs SET state = 'done' WHERE id = ?`, jobs[0]); err != nil {
		t.Fatal(err)
	}
	be.step()
	got := be.babysit()
	if got.JobID != jobs[0] || got.State != BabysitWorking {
		t.Errorf("an unreadable job was taken as finished: state %q job %d (was %d)", got.State, got.JobID, jobs[0])
	}
	if n := len(be.jobs()); n != 1 {
		t.Errorf("%d jobs: a second round started beside the first", n)
	}
}

// A stuck autopilot step whose job can't be looked up isn't failed as "its
// runner job is gone": it waits for the next sweep.
func TestUnstickStepJobUnreadableIsNotGone(t *testing.T) {
	a := newAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.stopWith("Done.")
	a.tick(6 * time.Minute)
	if len(a.jobs) != 1 || a.step().state != StepStarting {
		t.Fatalf("jobs %d, step %+v", len(a.jobs), a.step())
	}
	a.setClock(a.clockNow().Add(autopilotJobStuck + time.Minute))
	old := lookupRunnerJob
	defer func() { lookupRunnerJob = old }()
	lookupRunnerJob = func(context.Context, *Service, int64) (*RunnerJob, error) { return nil, errors.New("database is gone") }
	a.s.unstickSteps(a.ctx)
	if st := a.step(); st.state != StepStarting {
		t.Errorf("an unreadable job failed the step: %+v", st)
	}
	// Truly gone is another answer.
	lookupRunnerJob = func(context.Context, *Service, int64) (*RunnerJob, error) { return nil, notFound("runner job") }
	a.s.unstickSteps(a.ctx)
	if st := a.step(); st.state == StepStarting {
		t.Errorf("a job that is gone left the step starting: %+v", st)
	}
}

// A shutdown (cancelled context) in the middle of a job isn't a fault worth
// a Warn.
func TestPrepTrialsCancelledIsQuiet(t *testing.T) {
	e := newEnv(t)
	logs := &lockedBuf{}
	e.s.log = slog.New(slog.NewTextHandler(logs, nil))
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	e.s.RunPrepTrials(ctx)
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("a cancelled context was warned of:\n%s", logs.String())
	}
}

// The read helpers behind the merge hold and the gate answer an error, never
// the permissive zero, when their table can't be read.
func TestReadHelpersUnreadable(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	cases := []struct {
		table string
		run   func() (bool, error)
	}{
		{"runner_jobs", func() (bool, error) { return se.s.roundRunning(se.ctx, 1) }},
		{"code_audits", func() (bool, error) {
			id, _, err := se.s.newestOf(se.ctx, 1, "")
			return id != 0, err
		}},
		{"code_audit_findings", func() (bool, error) {
			todo, err := se.s.findingsToDo(se.ctx, 1)
			return len(todo) > 0, err
		}},
		{"session_messages", func() (bool, error) { return se.s.hasBlockingBatch(se.ctx, 1) }},
		{"project_repos", func() (bool, error) {
			id, err := se.s.attachedProjectFor(se.ctx, SystemActor(), &PullRequest{ForgeID: se.f.ID, Repo: "me/app"})
			return id != 0, err
		}},
		{"work_log", func() (bool, error) { return se.s.roundsAudit(se.ctx, 1) }},
		{"code_audits", func() (bool, error) {
			at, err := se.s.auditDeadline(se.ctx, 1)
			return at != nil, err
		}},
		{"forges", func() (bool, error) {
			err := se.s.postGateStatus(se.ctx, &PullRequest{ForgeID: se.f.ID, Repo: "me/app", HeadSHA: "1111111aaaa"}, GateSuccess, "ok")
			return err == nil, err
		}},
	}
	for _, c := range cases {
		if _, err := c.run(); err != nil {
			t.Errorf("%s readable: unexpected error %v", c.table, err)
		}
		restore := se.breakTable(c.table)
		ok, err := c.run()
		restore()
		if err == nil {
			t.Errorf("%s unreadable: no error (answer %v)", c.table, ok)
		}
		if ok && c.table != "forges" {
			t.Errorf("%s unreadable: answered true", c.table)
		}
	}
}

// A wait for the review gate whose gate setting can't be read doesn't pass
// as "no gate, nothing to wait for".
func TestWaitForGateUnreadableDoesNotPass(t *testing.T) {
	ae := newAgentEnv(t)
	ae.pullHook("opened", testPR)
	ae.s.Wait()
	gate := ae.addAuto(ae.p, "Gate", prRule(Action{Type: "wait_for", ID: "g", Extra: map[string]any{"what": waitGate, "timeout": "6h", "pr": "{{.Inputs.pr}}"}}))
	restore := ae.breakTable("project_repos")
	run := ae.manual(gate, map[string]any{"pr": "5"})
	ae.work()
	got := ae.runStatus(run)
	restore()
	if got.Status == RunSucceeded {
		t.Errorf("the gate wait passed with its setting unreadable: %s %+v", got.Status, stepByID(got, "g").Outputs)
	}
}
