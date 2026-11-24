package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core/udiff"
)

// runnersFor sets up ana's runner on the session's machine, another of hers
// on a different machine, and the admin's on the same machine name.
func (r *reqEnv) runnersFor() (box, other, adminBox *Runner) {
	r.t.Helper()
	box = r.env.runner(r.ana, "box", nil)
	other = r.env.runner(r.ana, "elsewhere", nil)
	adminBox = r.env.runner(r.admin, "box", nil)
	return
}

func TestCommandCellRunsOnTheDecidersRunner(t *testing.T) {
	r := newReqEnv(t)
	box, other, adminBox := r.runnersFor()
	q := r.ask(RequestInput{Kind: RequestCommand, Title: "Run the migration", Cells: []CellInput{{Command: "make migrate", Dir: "/work/app/db"}, {Command: "make seed"}}})

	got, err := r.s.RunCell(r.ctx, r.ana, q.Ref, 1, "make migrate ARGS=--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if run := got.Cells[0].Runs[0]; run.State != "queued" {
		t.Fatalf("run after queueing: %+v", run)
	}
	if w := r.poll(r.ana, other, RunnerPoll{}); w.Job != nil {
		t.Fatalf("ana's runner on another machine took it: %+v", w.Job)
	}
	if w := r.poll(r.admin, adminBox, RunnerPoll{}); w.Job != nil {
		t.Fatalf("the admin's runner of the same name took it: %+v", w.Job)
	}
	w := r.poll(r.ana, box, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobCommand {
		t.Fatalf("ana's runner on box didn't get it: %+v", w)
	}
	var cj CommandJob
	if err := json.Unmarshal(w.Job.Input, &cj); err != nil || cj.Command != "make migrate ARGS=--dry-run" || cj.Dir != "/work/app/db" {
		t.Fatalf("job input: %+v %v", cj, err)
	}
	if mid, _ := r.s.GetRequest(r.ctx, r.ana, q.Ref, 0); mid.Cells[0].Runs[0].State != "running" || mid.Cells[0].Runs[0].FinishedAt != nil {
		t.Errorf("run while the job runs: %+v", mid.Cells[0].Runs[0])
	}

	if _, err := r.s.AppendRunnerLog(r.ctx, r.ana, w.Job.ID, 0, "migrating…\nboom\n"); err != nil {
		t.Fatal(err)
	}
	r.setClock(r.clockNow().Add(7 * time.Second))
	three := 3
	if _, err := r.s.FinishRunnerJob(r.ctx, r.ana, w.Job.ID, RunnerFinish{State: "failed", ExitCode: &three, Error: "exit status 3"}); err != nil {
		t.Fatal(err)
	}
	done, err := r.s.GetRequest(r.ctx, r.ana, q.Ref, 0)
	if err != nil {
		t.Fatal(err)
	}
	cr := done.Cells[0].Runs[0]
	if cr.State != "failed" || cr.ExitCode == nil || *cr.ExitCode != 3 || cr.Output != "migrating…\nboom\n" || cr.DurationMS != 7000 || cr.FinishedAt == nil {
		t.Fatalf("saved run: %+v", cr)
	}
	if _, err := r.s.RunCell(r.ctx, r.ana, q.Ref, 2, ""); err != nil {
		t.Fatal(err)
	}
	w2 := r.poll(r.ana, box, RunnerPoll{})
	if w2.Job == nil {
		t.Fatal("second command wasn't handed out")
	}
	_ = json.Unmarshal(w2.Job.Input, &cj)
	if cj.Command != "make seed" || cj.Dir != "/work/app" {
		t.Errorf("second job: %+v", cj)
	}
	zero := 0
	if _, err := r.s.AppendRunnerLog(r.ctx, r.ana, w2.Job.ID, 0, "seeded\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.FinishRunnerJob(r.ctx, r.ana, w2.Job.ID, RunnerFinish{State: "done", ExitCode: &zero}); err != nil {
		t.Fatal(err)
	}
	done, _ = r.s.GetRequest(r.ctx, r.ana, q.Ref, 0)
	if c2 := done.Cells[1].Runs[0]; c2.State != "done" || *c2.ExitCode != 0 || c2.Output != "seeded\n" || len(done.Cells[0].Runs) != 1 {
		t.Errorf("second run: %+v", c2)
	}
}

func TestCommandCellNeedsTheDecidersOwnRunner(t *testing.T) {
	r := newReqEnv(t)
	r.env.runner(r.admin, "box", nil)
	q := r.ask(RequestInput{Kind: RequestCommand, Title: "x", Cells: []CellInput{{Command: "ls"}}})
	_, err := r.s.RunCell(r.ctx, r.ana, q.Ref, 1, "")
	if !IsInvalid(err) || !strings.Contains(err.Error(), "No runner online on box") {
		t.Fatalf("ana ran it on the admin's runner: %v", err)
	}
	got, _ := r.s.GetRequest(r.ctx, r.ana, q.Ref, 0)
	if len(got.Cells[0].Runs) != 1 || got.Cells[0].Runs[0].State != "failed" {
		t.Errorf("runs: %+v", got.Cells[0].Runs)
	}
}

func TestApprovedChangeAppliesThroughARealRunnerJob(t *testing.T) {
	r := newReqEnv(t)
	box, _, _ := r.runnersFor()
	base := "# Rules\n"
	q := claudeMDChange(r, base, "# Rules\nbe brief\n")
	if got, _ := r.s.GetRequest(r.ctx, r.ana, q.Ref, 0); got.Runner == nil || !got.Runner.Online || got.Runner.Machine != "box" {
		t.Fatalf("runner info: %+v", got.Runner)
	}
	got, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "approve"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != RequestApplying || got.ApplyPath != "runner" {
		t.Fatalf("after approving: %s via %q", got.State, got.ApplyPath)
	}
	w := r.poll(r.ana, box, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobApply {
		t.Fatalf("the runner got %+v", w)
	}
	var in ApplyJob
	if err := json.Unmarshal(w.Job.Input, &in); err != nil || in.Dir != "/work/app" || in.Commit != "Tighten the rules" || len(in.Files) != 2 || in.Files[0].BaseSHA != udiff.GitBlobSHA(base) {
		t.Fatalf("apply job: %+v %v", in, err)
	}
	if res := r.stop(); len(res.Deliver) != 0 {
		t.Fatal("told the agent before it was applied")
	}
	if _, err := r.s.FinishRunnerJob(r.ctx, r.ana, w.Job.ID, RunnerFinish{State: "done", Result: json.RawMessage(`{"files":["CLAUDE.md","docs/new.md"],"commit":"abcdef0123456789"}`)}); err != nil {
		t.Fatal(err)
	}
	got, _ = r.s.GetRequest(r.ctx, r.ana, q.Ref, 0)
	if got.State != RequestApplied || got.Result.Commit != "abcdef0123456789" || got.ApplyPath != "runner" {
		t.Fatalf("after the job: %s %+v", got.State, got.Result)
	}
	res := r.stop()
	if len(res.Deliver) != 1 || !strings.Contains(res.Text, "applied in your worktree as commit abcdef01") {
		t.Fatalf("delivery: %q", res.Text)
	}
}

func TestApplyJobBaseMismatchEndsAsConflict(t *testing.T) {
	r := newReqEnv(t)
	box, _, _ := r.runnersFor()
	q := claudeMDChange(r, "# a\n", "# b\n")
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q.Ref, RequestDecision{Decision: "approve", ApplyPath: "runner"}); err != nil {
		t.Fatal(err)
	}
	w := r.poll(r.ana, box, RunnerPoll{})
	if w.Job == nil {
		t.Fatal("no job")
	}
	result := json.RawMessage(`{"conflicts":[{"path":"CLAUDE.md","reason":"changed"}]}`)
	if _, err := r.s.FinishRunnerJob(r.ctx, r.ana, w.Job.ID, RunnerFinish{State: "failed", Result: result,
		Error: "Not applied, nothing was written: CLAUDE.md (changed) differs from what the change was made against."}); err != nil {
		t.Fatal(err)
	}
	got, _ := r.s.GetRequest(r.ctx, r.ana, q.Ref, 0)
	if got.State != RequestConflict || len(got.Result.Conflicts) != 1 || got.Result.Conflicts[0] != "CLAUDE.md" || !strings.Contains(got.Result.Reason, "CLAUDE.md (changed)") {
		t.Fatalf("conflict: %s %+v", got.State, got.Result)
	}
	if res := r.stop(); len(res.Deliver) != 1 || !strings.Contains(res.Text, "files changed since you made it") {
		t.Fatalf("delivery: %q", res.Text)
	}
	q2 := claudeMDChange(r, "# a\n", "# c\n")
	if _, err := r.s.DecideRequest(r.ctx, r.ana, q2.Ref, RequestDecision{Decision: "approve", ApplyPath: "runner"}); err != nil {
		t.Fatal(err)
	}
	w2 := r.poll(r.ana, box, RunnerPoll{})
	if _, err := r.s.FinishRunnerJob(r.ctx, r.ana, w2.Job.ID, RunnerFinish{State: "failed", Error: "The files are written and staged but the commit failed: no identity"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.s.GetRequest(r.ctx, r.ana, q2.Ref, 0); got.State != RequestFailed || !strings.Contains(got.Error, "commit failed") {
		t.Fatalf("failure: %s %q", got.State, got.Error)
	}
}
