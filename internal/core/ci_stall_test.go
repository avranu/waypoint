package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

// histTask builds a pull request task of a run that started startMin minutes
// into the day and took durMin.
func histTask(run int64, name, status string, startMin, durMin int) ActionTask {
	base := time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)
	st := base.Add(time.Duration(startMin) * time.Minute)
	return ActionTask{ID: run*10 + int64(len(name)), Name: name, RunNumber: run, WorkflowID: "ci.yml", Event: "pull_request",
		Status: status, Started: st, Updated: st.Add(time.Duration(durMin) * time.Minute)}
}

// fullAndDraft: drafts draft-tier runs (lint, backend; e2e skipped) and one
// full run (lint, backend, e2e) for each wall time.
func fullAndDraft(drafts int, fulls ...int) []ActionTask {
	var ts []ActionTask
	run := int64(1)
	for i := 0; i < drafts; i++ {
		ts = append(ts, histTask(run, "lint", "success", int(run)*100, 2), histTask(run, "backend", "success", int(run)*100, 5),
			histTask(run, "e2e", "skipped", int(run)*100, 0))
		run++
	}
	for _, wall := range fulls {
		ts = append(ts, histTask(run, "lint", "success", int(run)*100, 6), histTask(run, "backend", "success", int(run)*100, wall-1),
			histTask(run, "e2e", "success", int(run)*100, wall))
		run++
	}
	return ts
}

func TestStallFromHistory(t *testing.T) {
	min := time.Minute
	after, basis := stallFromHistory(fullAndDraft(30, 10, 11, 12, 13, 14))
	if after != 20*min || !strings.Contains(basis, "last 5 full runs") {
		t.Errorf("fast repo: %s %q", after, basis)
	}
	after, basis = stallFromHistory(fullAndDraft(40, 33, 36, 37, 38, 40, 44, 45, 46))
	if after < 60*min || after > 72*min || !strings.Contains(basis, "1.5 ×") {
		t.Errorf("slow repo: %s %q", after, basis)
	}
	after, basis = stallFromHistory(fullAndDraft(10, 5, 6, 6, 7, 7, 8))
	if after != stallFloor || !strings.Contains(basis, "20m minimum") {
		t.Errorf("floor: %s %q", after, basis)
	}
	after, basis = stallFromHistory(fullAndDraft(30, 30, 31))
	if after != fullRunStall || !strings.Contains(basis, "fewer than 5") {
		t.Errorf("too few: %s %q", after, basis)
	}
	if after, _ = stallFromHistory(nil); after != fullRunStall {
		t.Errorf("no history: %s", after)
	}
	var plain []ActionTask
	for i := int64(1); i <= 6; i++ {
		plain = append(plain, histTask(i, "test", "success", int(i)*100, 30))
	}
	if after, _ = stallFromHistory(plain); after != 45*min {
		t.Errorf("plain repo: %s", after)
	}
	odd := fullAndDraft(0, 10, 10, 10, 10)
	for i := int64(50); i < 60; i++ {
		odd = append(odd, histTask(i, "lint", "success", int(i)*100, 5), histTask(i, "backend", "success", int(i)*100, 90),
			histTask(i, "e2e", "failure", int(i)*100, 90))
		p := histTask(i+100, "backend", "success", int(i)*100, 120)
		p.Event = "push"
		odd = append(odd, p)
	}
	if after, _ = stallFromHistory(odd); after != fullRunStall {
		t.Errorf("odd runs: %s", after)
	}
}

// actionsForge gives the shipping fake a CI history.
type actionsForge struct {
	*shipForge
	tasks []ActionTask
	reads int
	fail  bool
}

func (a *actionsForge) ActionRuns(context.Context, *Forge, string, []string, int, int) ([]ActionRun, error) {
	return nil, nil
}

func (a *actionsForge) ActionJobs(context.Context, *Forge, string, int64) ([]ActionJob, error) {
	return nil, nil
}

func (a *actionsForge) ActionRunners(context.Context, *Forge) ([]ActionRunner, error) {
	return nil, errRunnersHidden
}

func (a *actionsForge) ActionTasks(_ context.Context, _ *Forge, _ string, _ []string, page, _ int) ([]ActionTask, error) {
	if page == 1 {
		a.reads++
	}
	if a.fail {
		return nil, ErrNotFound
	}
	if page > 1 {
		return nil, nil
	}
	return a.tasks, nil
}

// A repo's measured full runs set when its ready pull requests count as
// stalled, and the stall message names the threshold.
func TestDraftFirstStallFollowsMeasuredRuns(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.sg.files[".forgejo/workflows/ci.yml"] = ciOnEdit
	be.runnerOnline()
	p := bsPR
	p.Title = "WIP: Add export"
	be.pullHook("opened", p)
	be.s.Wait()
	be.drain()
	pr := be.pr(7)
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET wip_auto = 1 WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.MergePR(be.ctx, be.agent, pr.ID, MergeInput{WhenReady: true, Style: "squash"}); err != nil {
		t.Fatal(err)
	}
	be.start(pr)
	be.draftChecks("7777777aaaa", CommitCheck{State: CheckSuccess, ID: 5, Description: "Successful in 4m"}, CommitCheck{State: CheckSuccess, ID: 6, Description: "Has been skipped"})
	if be.pr(7).ReadyAt == nil {
		t.Fatal("not ready")
	}
	af := &actionsForge{shipForge: be.sg, tasks: fullAndDraft(20, 14, 15, 15, 16, 16, 17)}
	be.s.Forges = af
	if m := be.s.fullRunStalled(be.ctx, be.pr(7)); m != "" {
		t.Fatalf("stalled the moment it was ready: %s", m)
	}
	be.setClock(be.clockNow().Add(22 * time.Minute))
	if m := be.s.fullRunStalled(be.ctx, be.pr(7)); m != "" {
		t.Fatalf("stalled at 22m: %s", m)
	}
	be.setClock(be.clockNow().Add(4 * time.Minute))
	m := be.s.fullRunStalled(be.ctx, be.pr(7))
	if !strings.Contains(m, "full run hasn't finished") || !strings.Contains(m, " in 25m (1.5 × the 17m p90 of its last 6 full runs)") {
		t.Fatalf("message %q", m)
	}
	reads := af.reads
	be.s.fullRunStalled(be.ctx, be.pr(7))
	if af.reads != reads {
		t.Errorf("history read again: %d → %d", reads, af.reads)
	}
	be.runnerOnline()
	be.step()
	if b := be.babysit(); b.State != BabysitNeedsPerson || !strings.Contains(b.Wait, "in 25m") {
		t.Fatalf("%+v", b)
	}

	af.fail = true
	be.setClock(be.clockNow().Add(ciStallFresh + time.Minute))
	if m := be.s.fullRunStalled(be.ctx, be.pr(7)); m != "" {
		t.Errorf("stalled at 37m on the default: %s", m)
	}
	af.reads = 0
	be.s.fullRunStalled(be.ctx, be.pr(7))
	if af.reads != 0 {
		t.Errorf("a failed read was tried at once again: %d", af.reads)
	}
	af.fail = false
	be.setClock(be.clockNow().Add(ciStallRetry + time.Second))
	be.s.fullRunStalled(be.ctx, be.pr(7))
	if af.reads == 0 {
		t.Error("a failed read wasn't tried again")
	}
}
