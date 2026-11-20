package core

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// The CI check of 2026-11-05 (WP-586): Robin/waypoint's e2e shard 2 failed
// in runs 1680, 1681, 1686 and 1695, each with #272 (WP-503) in it: its own
// run, and the stacks and batches for #272, #273 and #274 behind it. The
// doctor called the job unreliable and sent next: to run 1696, where the job
// was only cancelled.
func TestDoctorJobFailsWithOnePR(t *testing.T) {
	at := time.Date(2026, 11, 5, 21, 35, 0, 0, time.UTC)
	m := func(n int) time.Time { return at.Add(time.Duration(n) * time.Minute) }
	f := &Forge{ID: 3, token: "t"}
	const repo, job = "Robin/waypoint", "e2e shard 2"
	var tasks []ActionTask
	var recent []CIJob
	task := func(run int64, sha, branch, status string, end int) {
		tasks = append(tasks, ActionTask{ID: run * 10, RunNumber: run, WorkflowID: "ci.yml", Name: job, SHA: sha, Branch: branch,
			Event: "push", Status: status, Started: m(end - 20), Updated: m(end)})
		recent = append(recent, CIJob{CIRef: CIRef{Repo: repo, Workflow: "ci.yml", RunNumber: run}, Name: job, Status: status})
	}
	// Passes, of other pull requests' heads and stacks.
	for i := range 7 {
		task(int64(1670+i), fmt.Sprintf("other%d", i), "feature", "success", -600+10*i)
	}
	task(1680, "pr272a", "worktree-wp-503", "failure", -300)       // #272's own run
	task(1681, "stack272", "wp-queue/main/272-a", "failure", -280) // #272's stack: [#271, #272]
	task(1686, "stack273", "wp-queue/main/273-b", "failure", -200) // #273's: [#272, #273]
	task(1695, "batch274", "wp-queue/main/274-c", "failure", -60)  // a batch: [#272, #273, #274]
	for i := range 5 {                                             // cancels: newer pushes
		task(int64(1690+i), fmt.Sprintf("gone%d", i), "feature", "cancelled", -150+i)
	}
	task(1696, "batch274b", "wp-queue/main/274-d", "cancelled", -10) // the newest bad run: only cancelled
	slices.Reverse(recent)                                           // newest first
	contains := map[string][]int{
		stackConflictKey(3, repo, "pr272a"):    {272},
		stackConflictKey(3, repo, "stack272"):  {271, 272},
		stackConflictKey(3, repo, "stack273"):  {272, 273},
		stackConflictKey(3, repo, "batch274"):  {272, 273, 274},
		stackConflictKey(3, repo, "batch274b"): {272, 273, 274},
	}
	r := &ciRepoData{ref: ciRepoRef{forge: f, repo: repo}, hist: &ciHistory{tasks: tasks}}
	b := &CIBoard{Recent: recent, Stats: []CIJobStats{{Repo: repo, Workflow: "ci.yml", Name: job, Runs: 17, Success: 7, Failure: 4, Cancelled: 6}}}
	snap := &ciSnapshot{repos: []*ciRepoData{r}}
	visible := func(int64, string) (string, bool) { return "WP", true }

	run := func(contains map[string][]int) *CIDoctor {
		d := &CIDoctor{}
		doctorJobs(d, b, snap, nil, nil, contains, at, visible)
		if len(d.Jobs) != 1 {
			t.Fatalf("jobs = %+v", d.Jobs)
		}
		return d
	}

	// Without knowing what the runs held: unreliable, but next: points at
	// the latest failure, not the cancel after it.
	d := run(nil)
	if j := d.Jobs[0]; !j.Unreliable || j.OnePR != 0 || j.LastBad != 1695 {
		t.Errorf("unknown heads: %+v", j)
	}

	d = run(contains)
	j := d.Jobs[0]
	if j.Unreliable || j.OnePR != 272 || j.LastBad != 1695 || j.lastFail.task != 16950 {
		t.Fatalf("job = %+v", j)
	}

	// The doctor reads that failure's log for the test it names.
	logs := &doctorLogForge{jobs: map[int64][]ActionJob{1695: {{ID: 77, Name: job, TaskID: 16950}}},
		logs: map[int64]string{77: "== 61-rules.mjs\nok   rules\n== 62-audits.mjs\nok   the audit dialog opens\n" +
			"FAIL the runner gets an audit job (command)\nFAILED: 62-audits.mjs\n-- 62-audits.mjs took 40s\n"}}
	s := &Service{}
	hist := map[string]*ciRepoData{ciRepoKey(3, repo): r}
	s.readDoctorTests(context.Background(), logs, d, hist)
	if want := []string{"62-audits.mjs: the runner gets an audit job (command)"}; !slices.Equal(d.Jobs[0].Tests, want) {
		t.Fatalf("tests = %q, want %q", d.Jobs[0].Tests, want)
	}
	// Kept: a second call reads nothing.
	d.Jobs[0].Tests = nil
	logs.reads = 0
	s.readDoctorTests(context.Background(), logs, d, hist)
	if logs.reads != 0 || len(d.Jobs[0].Tests) != 1 {
		t.Errorf("read again: %d reads, tests %q", logs.reads, d.Jobs[0].Tests)
	}

	doctorFindings(d, &CIBoard{}, at)
	if slices.ContainsFunc(d.Findings, func(f CIFinding) bool { return f.Kind == "unreliable_job" }) {
		t.Errorf("still unreliable: %+v", d.Findings)
	}
	i := slices.IndexFunc(d.Findings, func(f CIFinding) bool { return f.Kind == "job_fails_with_pr" })
	if i < 0 {
		t.Fatalf("findings = %+v", d.Findings)
	}
	got := d.Findings[i]
	if want := "Robin/waypoint e2e shard 2 fails only with #272 in it: 62-audits.mjs: the runner gets an audit job (command)"; got.Title != want {
		t.Errorf("title = %q, want %q", got.Title, want)
	}
	if got.PR != 272 || got.Next != `wp ci log --repo Robin/waypoint --run 1695 --job "e2e shard 2"` || !strings.Contains(got.Detail, "not a flaky job") {
		t.Errorf("finding = %+v", got)
	}

	// A failure without #272 in it: the job is unreliable again.
	tasks = append(tasks, ActionTask{ID: 16970, RunNumber: 1697, WorkflowID: "ci.yml", Name: job, SHA: "other9", Event: "push", Status: "failure", Started: m(-9), Updated: m(-5)})
	r.hist.tasks = tasks
	b.Stats[0].Runs, b.Stats[0].Failure = 18, 5
	if j := run(contains).Jobs[0]; !j.Unreliable || j.OnePR != 0 || j.LastBad != 1697 {
		t.Errorf("another failure: %+v", j)
	}
}

// A flaky job while #1 sits at the queue's front: every stack behind it holds
// #1, so each failure does, but passes do too. It stays unreliable, not
// blamed on #1 (audit 481.1).
func TestDoctorJobFlakyBehindTheFront(t *testing.T) {
	at := time.Date(2026, 11, 5, 21, 35, 0, 0, time.UTC)
	m := func(n int) time.Time { return at.Add(time.Duration(n) * time.Minute) }
	f := &Forge{ID: 3, token: "t"}
	const repo, job = "Robin/waypoint", "e2e shard 2"
	var tasks []ActionTask
	var recent []CIJob
	contains := map[string][]int{}
	for i := range 10 {
		run, sha, status := int64(1700+i), fmt.Sprintf("stack%d", i), "success"
		if i%3 == 0 { // 4 failures, 6 passes, all with #1 at the front
			status = "failure"
		}
		contains[stackConflictKey(3, repo, sha)] = []int{1, 10 + i}
		tasks = append(tasks, ActionTask{ID: run * 10, RunNumber: run, WorkflowID: "ci.yml", Name: job, SHA: sha, Branch: fmt.Sprintf("wp-queue/main/%d", 10+i),
			Event: "push", Status: status, Started: m(-300 + 20*i), Updated: m(-290 + 20*i)})
		recent = append(recent, CIJob{CIRef: CIRef{Repo: repo, Workflow: "ci.yml", RunNumber: run}, Name: job, Status: status})
	}
	slices.Reverse(recent)
	r := &ciRepoData{ref: ciRepoRef{forge: f, repo: repo}, hist: &ciHistory{tasks: tasks}}
	b := &CIBoard{Recent: recent, Stats: []CIJobStats{{Repo: repo, Workflow: "ci.yml", Name: job, Runs: 10, Success: 6, Failure: 4}}}
	d := &CIDoctor{}
	doctorJobs(d, b, &ciSnapshot{repos: []*ciRepoData{r}}, nil, nil, contains, at, func(int64, string) (string, bool) { return "WP", true })
	if len(d.Jobs) != 1 || !d.Jobs[0].Unreliable || d.Jobs[0].OnePR != 0 {
		t.Fatalf("jobs = %+v", d.Jobs)
	}
}

// Blamed only when one pull request alone fits: a single failure proves
// nothing, and two pull requests in every failure can't be told apart (a
// stack's front isn't the culprit for being first, nor the lower number).
func TestDoctorJobOnePRNeedsOneSuspect(t *testing.T) {
	at := time.Date(2026, 11, 5, 21, 35, 0, 0, time.UTC)
	m := func(n int) time.Time { return at.Add(time.Duration(n) * time.Minute) }
	f := &Forge{ID: 3, token: "t"}
	const repo, job = "Robin/waypoint", "e2e shard 2"
	type run struct {
		status string
		heads  []int
	}
	doctor := func(runs []run) CIDoctorJob {
		t.Helper()
		var tasks []ActionTask
		var recent []CIJob
		contains := map[string][]int{}
		st := CIJobStats{Repo: repo, Workflow: "ci.yml", Name: job, Runs: len(runs)}
		for i, x := range runs {
			n, sha, branch := int64(1800+i), fmt.Sprintf("sha%d", i), "wp-queue/main/"+fmt.Sprint(i)
			if x.status == "cancelled" {
				branch = "feature" // a newer push's, not the queue's
			}
			contains[stackConflictKey(3, repo, sha)] = x.heads
			tasks = append(tasks, ActionTask{ID: n * 10, RunNumber: n, WorkflowID: "ci.yml", Name: job, SHA: sha, Branch: branch,
				Event: "push", Status: x.status, Started: m(-300 + 10*i), Updated: m(-295 + 10*i)})
			recent = append(recent, CIJob{CIRef: CIRef{Repo: repo, Workflow: "ci.yml", RunNumber: n}, Name: job, Status: x.status})
			switch x.status {
			case "success":
				st.Success++
			case "failure":
				st.Failure++
			default:
				st.Cancelled++
			}
		}
		slices.Reverse(recent)
		r := &ciRepoData{ref: ciRepoRef{forge: f, repo: repo}, hist: &ciHistory{tasks: tasks}}
		d := &CIDoctor{}
		doctorJobs(d, &CIBoard{Recent: recent, Stats: []CIJobStats{st}}, &ciSnapshot{repos: []*ciRepoData{r}}, nil, nil, contains, at,
			func(int64, string) (string, bool) { return "WP", true })
		if len(d.Jobs) != 1 {
			t.Fatalf("jobs = %+v", d.Jobs)
		}
		return d.Jobs[0]
	}
	pass := func(n int) []run {
		rs := make([]run, n)
		for i := range rs {
			rs[i] = run{"success", []int{100 + i}}
		}
		return rs
	}

	// Two failures, both in the stack [#5, #3] and nothing else: either may
	// be the culprit.
	j := doctor(append(pass(6), run{"failure", []int{5, 3}}, run{"failure", []int{5, 3}}))
	if !j.Unreliable || j.OnePR != 0 {
		t.Errorf("two suspects: %+v", j)
	}
	// #3's own run fails too: only #3 is in every failure.
	j = doctor(append(pass(6), run{"failure", []int{5, 3}}, run{"failure", []int{5, 3}}, run{"failure", []int{3}}))
	if j.Unreliable || j.OnePR != 3 {
		t.Errorf("#3 alone: %+v", j)
	}
	// One failure, among cancels that make the job unreliable anyway.
	rs := append(pass(4), run{"failure", []int{3}})
	for range 4 {
		rs = append(rs, run{"cancelled", []int{200}})
	}
	if j = doctor(rs); !j.Unreliable || j.OnePR != 0 {
		t.Errorf("one failure: %+v", j)
	}
}

type doctorLogForge struct {
	jobs  map[int64][]ActionJob // by run number
	logs  map[int64]string      // by job id
	reads int
}

func (f *doctorLogForge) ActionRunNumber(_ context.Context, _ *Forge, _ string, n int64) (*ActionRun, error) {
	if _, ok := f.jobs[n]; !ok {
		return nil, ErrNotFound
	}
	return &ActionRun{ID: n, Index: n}, nil
}

func (f *doctorLogForge) ActionJobs(_ context.Context, _ *Forge, _ string, runID int64) ([]ActionJob, error) {
	return f.jobs[runID], nil
}

func (f *doctorLogForge) ActionJobLog(_ context.Context, _ *Forge, _ string, jobID int64) (string, error) {
	f.reads++
	return f.logs[jobID], nil
}

// commitHeads finds a pull request's heads, its pushed ones too, and a
// stack's or batch's heads by the commit its run builds.
func TestCommitHeads(t *testing.T) {
	be, pr := armedBabysit(t)
	now := be.s.now()
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO pr_events(pr_id, kind, sha, created_at) VALUES (?, 'pushed', 'aaaa111', ?)`, pr.ID, now.Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	heads := fmt.Sprintf(`[{"pr_id":1,"number":271,"sha":"x"},{"pr_id":%d,"number":%d,"sha":"y"}]`, pr.ID, pr.Number)
	for _, pos := range []int{0, 1} { // a batch: a row for each pull request, one commit
		if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO queue_stacks(forge_id, repo, base_ref, pr_id, base_sha, heads, pos, branch, commit_sha, state, created_at)
			VALUES (?,?,?,?,'b',?,?,'wp-queue/main/1','Stack1','failed',?)`, pr.ForgeID, pr.Repo, pr.BaseRef, pr.ID, heads, pos, now.Add(-time.Hour).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	got := be.s.commitHeads(be.ctx, now.Add(-2*ciHistoryWindow))
	for sha, want := range map[string][]int{pr.HeadSHA: {pr.Number}, "aaaa111": {pr.Number}, "stack1": {271, pr.Number}} {
		if g := got[stackConflictKey(pr.ForgeID, strings.ToUpper(pr.Repo), sha)]; !slices.Equal(g, want) {
			t.Errorf("%s: %v, want %v (all: %v)", sha, g, want, got)
		}
	}
}
