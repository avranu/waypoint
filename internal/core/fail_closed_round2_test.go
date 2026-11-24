package core

import (
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
)

// With none of the rows a hold looks for (no babysit, no audit, no merge
// policy row), reviewHolds holds nothing for them and schedules no recheck.
func TestReviewHoldsNoRowsHoldNothing(t *testing.T) {
	be := newBabysitEnv(t)
	logs := &lockedBuf{}
	be.s.log = slog.New(slog.NewTextHandler(logs, nil))
	pr := be.openMine()
	cur := be.pr(pr.Number)
	var rd PRReadiness
	reasons, err := be.s.reviewHolds(be.ctx, cur, true, &rd)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range reasons {
		if strings.Contains(r, "couldn't be read") {
			t.Errorf("a hold for a read that found no row: %q", r)
		}
	}
	if rd.wake != nil {
		t.Errorf("a recheck is scheduled for a normal answer: %v", rd.wake)
	}
	be.exec(`DELETE FROM babysits`)
	if be.s.babysitBlocks(be.ctx, cur.ID) {
		t.Error("babysitBlocks holds a pull request that was never babysat")
	}
	if _, _, err := be.s.auditGoingErr(be.ctx, cur.ID); err != nil {
		t.Errorf("auditGoingErr with no audits: %v", err)
	}
	for _, l := range strings.Split(logs.String(), "\n") {
		// Forge-webhook warnings come from the fake forge fixture, not from a hold.
		if strings.Contains(l, "WARN") && !strings.Contains(l, "forge webhook") {
			t.Errorf("a normal outcome was logged as a warning: %s", l)
		}
	}
}

// An install with no AI model set up is normal: the sweeps and the prep
// reads say nothing at Warn.
func TestNoAIModelLogsNoWarn(t *testing.T) {
	e := newEnv(t)
	logs := &lockedBuf{}
	e.s.log = slog.New(slog.NewTextHandler(logs, nil))
	if ok, err := e.s.estimateConfigured(e.ctx); ok || err != nil {
		t.Fatalf("estimateConfigured = %v, %v in a fresh install; want false, nil", ok, err)
	}
	e.s.sweepEstimates(e.ctx)
	e.s.RunPrepTrials(e.ctx)
	if _, err := e.s.PrepDue(e.ctx, e.admin, "m1", 5); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.s.aiTaskSetUp(e.ctx, "prep"); ok || err != nil {
		t.Errorf("aiTaskSetUp(prep) = %v, %v; want false, nil", ok, err)
	}
	if strings.Contains(logs.String(), "level=WARN") || strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("no model set up was logged as a problem:\n%s", logs.String())
	}
}

// A project that can't be read doesn't lose the question: it is filed for
// the person with no project (finding A1).
func TestParkQuestionFiledWhenProjectUnreadable(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	b := be.start(pr)
	defer be.breakTable("project_repos")()
	r := &BabysitRound{N: 1, Because: "which way is right?"}
	be.s.askAboutPark(be.ctx, b, be.pr(7), r, "Which way?")
	var n int
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT count(*) FROM requests WHERE tool_use_id = ?`, parkRequestKey+strconv.FormatInt(b.ID, 10)+":1").Scan(&n); err != nil || n != 1 {
		t.Fatalf("park question filed %d times (err %v), want once despite the unreadable project", n, err)
	}
}

// A CI runner fault is still asked about when the project can't be read,
// and says so: told is only set once it was (finding A2).
func TestRunnerFaultAskedWhenProjectUnreadable(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	f := &RunnerFault{What: "the runner ran out of disk space", Line: "no space left on device", Runner: "r1"}
	defer be.breakTable("project_repos")()
	at := prFaultSubject(be.pr(pr.Number))
	if !be.s.askAboutRunner(be.ctx, be.admin.User.ID, at, map[string]*RunnerFault{"ci / test": f}, "It needs a person.") {
		t.Fatal("askAboutRunner gave up on a project that couldn't be read")
	}
	var n int
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT count(*) FROM requests WHERE tool_use_id = ?`, runnerFaultKey(at.forgeID, f)).Scan(&n); err != nil || n != 1 {
		t.Fatalf("fault request filed %d times (err %v), want once", n, err)
	}
}

// An approved held change whose project can't be read is still pushed, not
// failed (finding A3).
func TestHeldChangeApprovedProjectUnreadableIsNotFailed(t *testing.T) {
	be, pr, job := protHeldEnv(t)
	be.llm.answers["opencode/muse-spark-1.3-contributor-free"] = `{"weakens": false, "reason": "A cleaned-up command."}`
	defer be.breakTable("project_repos")()
	be.finishHeld(job, protHeldCI, false)
	h := be.heldOf()
	if h == nil || h.State == heldFailed {
		t.Fatalf("held change failed on an unreadable project: %+v", h)
	}
	if h.State != heldApplying || len(be.heldJobs()) != 1 {
		t.Fatalf("held %+v jobs %v, want it applying", h, be.heldJobs())
	}
	_ = pr
}

// A repo whose pull requests cannot be listed is the refresh failing, so it
// warns: at Debug the job read as clean while it refreshed nothing.
func TestRefreshWarnsWhenListingFails(t *testing.T) {
	pe := newPREnv(t, "github")
	logs := &lockedBuf{}
	pe.s.log = slog.New(slog.NewTextHandler(logs, nil))
	pe.fg.mu.Lock()
	pe.fg.listErr = errors.New("forge is down")
	pe.fg.mu.Unlock()
	pe.s.RefreshPullRequests(pe.ctx)
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "list pull requests") {
		t.Errorf("a failed listing was not warned:\n%s", out)
	}
}
