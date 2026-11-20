package core

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// baseFaultForge is a Forgejo with Actions runs, jobs and logs, that
// re-runs one run's jobs.
type baseFaultForge struct {
	*shipForge
	mu     sync.Mutex
	runs   []ActionRun
	jobs   map[int64][]ActionJob // by run id
	logs   map[int64]string      // by job id
	reruns []int64               // run ids
}

func (a *baseFaultForge) ActionRuns(context.Context, *Forge, string, []string, int, int) ([]ActionRun, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := slices.Clone(a.runs)
	slices.SortFunc(out, func(x, y ActionRun) int { return int(y.Index - x.Index) })
	return out, nil
}

func (a *baseFaultForge) ActionJobs(_ context.Context, _ *Forge, _ string, runID int64) ([]ActionJob, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.jobs[runID]), nil
}

func (a *baseFaultForge) ActionTasks(context.Context, *Forge, string, []string, int, int) ([]ActionTask, error) {
	return nil, nil
}

func (a *baseFaultForge) ActionRunners(context.Context, *Forge) ([]ActionRunner, error) {
	return nil, errRunnersHidden
}

func (a *baseFaultForge) ActionRunsAt(context.Context, *Forge, string, string) ([]ActionRun, error) {
	return nil, nil
}

func (a *baseFaultForge) ActionRunNumber(_ context.Context, _ *Forge, _ string, n int64) (*ActionRun, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.runs {
		if r.Index == n {
			return &r, nil
		}
	}
	return nil, ErrNotFound
}

func (a *baseFaultForge) ActionJobLog(_ context.Context, _ *Forge, _ string, jobID int64) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.logs[jobID], nil
}

func (a *baseFaultForge) RetryRun(_ context.Context, _ *Forge, _ string, runID int64) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reruns = append(a.reruns, runID)
	for i := range a.runs {
		if a.runs[i].ID == runID {
			a.runs[i].Status = "running"
		}
	}
	return 1, nil
}

// add puts a run with one job on the forge.
func (a *baseFaultForge) add(n int64, ref, sha, status, log string, stopped time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := 100 + n
	a.runs = append(a.runs, ActionRun{ID: id, Index: n, WorkflowID: "ci.yml", PrettyRef: ref, CommitSHA: sha, Event: "push", Status: status,
		Created: stopped.Add(-5 * time.Minute), Started: stopped.Add(-4 * time.Minute), Stopped: stopped, HTMLURL: "https://git.example.com/me/app/actions/runs/" + ref})
	a.jobs[id] = []ActionJob{{ID: 1000 + n, RunID: id, Name: "validate", Status: status}}
	a.logs[1000+n] = log
}

// end finishes run n's re-run with status, as the forge does.
func (a *baseFaultForge) end(n int64, status string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.runs {
		if a.runs[i].Index == n {
			a.runs[i].Status = status
			a.jobs[a.runs[i].ID][0].Status = status
		}
	}
}

func (a *baseFaultForge) rerun() []int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.reruns)
}

const dnsCheckoutLog = "  ☁️  git fetch 'https://github.com/actions/checkout' # ref=v7\n" +
	"⚙️ [runner]: could not fetch remote 'origin': fatal: unable to access 'https://github.com/actions/checkout/': Could not resolve host: github.com (DNS server returned general failure): exit status 128\n🏁  Job failed\n"

// A base branch's push run that failed on its runner is re-run like a
// pull request's head, at most twice, then one task asks an agent (WP-589); a
// run a newer push replaced, a branch that isn't a base and a failure of
// the code are left alone (WP-551).
func TestBaseFaultsRerun(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	af := &baseFaultForge{shipForge: se.sg, jobs: map[int64][]ActionJob{}, logs: map[int64]string{}}
	se.s.Forges = af
	failedAt := se.clockNow().Add(-5 * time.Minute)
	af.add(9, "main", "9999999", "failure", dnsCheckoutLog, failedAt.Add(-time.Hour)) // replaced by #10
	af.add(10, "main", "aaaaaaa", "failure", dnsCheckoutLog, failedAt)
	af.add(11, "release/v_0_9_0", "bbbbbbb", "failure", dnsCheckoutLog, failedAt)
	af.add(12, "wip", "ccccccc", "failure", dnsCheckoutLog, failedAt)
	sweep := func() {
		t.Helper()
		se.s.Wait()                                      // background work (task screening) reads the clock; let it finish first
		se.setClock(se.clockNow().Add(10 * time.Minute)) // the scheduler's pace; the history read is stale by then
		se.s.RecordCIHistory(se.ctx)
	}
	tasks := func() []faultTask {
		t.Helper()
		return runnerFaultTasks(t, se.s, se.ctx)
	}
	state := func(branch string) baseFault {
		t.Helper()
		return se.s.baseFaults(se.ctx, se.f.ID, "me/app")[branch]
	}

	se.s.RecordCIHistory(se.ctx)
	if got := af.rerun(); !slices.Equal(got, []int64{110, 111}) {
		t.Fatalf("first re-runs: %v", got)
	}
	if st := state("main"); st.sha != "aaaaaaa" || st.n != 1 || st.ended != "" {
		t.Fatalf("main state: %+v", st)
	}
	// Running again: nothing to do, and the count stays.
	sweep()
	if got := af.rerun(); len(got) != 2 || state("main").n != 1 {
		t.Fatalf("while running: %v %+v", got, state("main"))
	}
	// Failed on the runner again: the second re-run; the release branch
	// passes this time and is forgotten.
	af.end(10, "failure")
	af.end(11, "success")
	sweep()
	if got := af.rerun(); !slices.Equal(got, []int64{110, 111, 110}) {
		t.Fatalf("second re-runs: %v", got)
	}
	if _, kept := se.s.baseFaults(se.ctx, se.f.ID, "me/app")["release/v_0_9_0"]; kept {
		t.Error("a passed branch's state is kept")
	}
	// The third re-run, then one task that keeps the count going, and no more
	// filings: the fault is tracked and Waypoint goes on re-running it.
	af.end(10, "failure")
	sweep()
	if got := af.rerun(); !slices.Equal(got, []int64{110, 111, 110, 110}) || len(tasks()) != 0 {
		t.Fatalf("third re-run: %v tasks %+v", got, tasks())
	}
	af.end(10, "failure")
	sweep()
	if got := tasks(); len(got) != 1 || got[0].title != "CI runner fault on "+se.f.Name+": the runner couldn't resolve a host name (DNS)" ||
		!strings.Contains(got[0].text, "me/app main run #10") || !strings.Contains(got[0].text, "has re-run them 3 times") {
		t.Fatalf("tasks: %+v", got)
	}
	if st := state("main"); st.n != 3 || st.ended != baseFaultReported {
		t.Fatalf("main state after filing: %+v", st)
	}
	if got := af.rerun(); len(got) != 4 {
		t.Fatalf("re-ran inside the back-off: %v", got)
	}
	sweep()
	sweep()
	if got := af.rerun(); len(got) != 5 || len(tasks()) != 1 || strings.Contains(tasks()[0].text, "Seen again") {
		t.Fatalf("after the back-off: %v tasks %+v", got, tasks())
	}
	// A newer push that fails in its code: not re-run, and the older
	// commit's runner fault is no longer looked at.
	af.add(13, "main", "ddddddd", "failure", "--- FAIL: TestExport (0.01s)\n    export_test.go:12: got 1, want 2\nFAIL\tapp\n[runner]: exitcode '1': failure\n", se.clockNow())
	sweep()
	if got := af.rerun(); len(got) != 5 {
		t.Fatalf("re-ran a code failure: %v", got)
	}
	if st := state("main"); st.sha != "ddddddd" || st.ended != baseFaultOther {
		t.Fatalf("main state after a code failure: %+v", st)
	}
	// A red run the forge lists no jobs or stop time for is waited on for a
	// while, then ended rather than read on every pass.
	af.add(14, "release/v_0_9_0", "eeeeeee", "failure", dnsCheckoutLog, se.clockNow())
	af.mu.Lock()
	af.runs[len(af.runs)-1].Stopped = time.Time{}
	delete(af.jobs, 114)
	af.mu.Unlock()
	sweep()
	if st := state("release/v_0_9_0"); st.sha != "eeeeeee" || st.ended != "" {
		t.Fatalf("release state with no jobs yet: %+v", st)
	}
	sweep()
	if st := state("release/v_0_9_0"); st.ended != baseFaultOther || len(af.rerun()) != 5 {
		t.Fatalf("release state with no jobs after the wait: %+v %v", st, af.rerun())
	}
	// Another workflow of the tip that stopped long ago doesn't cut short the
	// wait for a run with no jobs or stop time yet.
	af.add(15, "release/v_0_8_0", "fffffff", "failure", dnsCheckoutLog, se.clockNow())
	af.add(16, "release/v_0_8_0", "fffffff", "failure", dnsCheckoutLog, se.clockNow().Add(-time.Hour))
	af.mu.Lock()
	af.runs[len(af.runs)-2].Stopped = time.Time{}
	af.runs[len(af.runs)-1].WorkflowID = "other.yml"
	delete(af.jobs, 115)
	af.mu.Unlock()
	reruns := len(af.rerun())
	sweep()
	if st := state("release/v_0_8_0"); st.sha != "fffffff" || st.ended != "" || len(af.rerun()) != reruns {
		t.Fatalf("release state with one workflow's jobs not listed yet: %+v %v", st, af.rerun())
	}
	// The board lists the faults it acted on.
	if fs := se.s.runnerFaults.since(se.clockNow().Add(-time.Hour)); len(fs) == 0 || !strings.HasPrefix(fs[len(fs)-1].check, "main ") {
		t.Errorf("board faults: %+v", fs)
	}
}

// redTip takes the newest commit's failed runs, each workflow's newest.
func TestRedTip(t *testing.T) {
	run := func(n int64, wf, sha, status string) ActionRun {
		return ActionRun{Index: n, WorkflowID: wf, CommitSHA: sha, Status: status}
	}
	red, running := redTip([]ActionRun{run(1, "ci.yml", "a", "failure"), run(3, "ci.yml", "b", "failure"), run(2, "codeql.yml", "b", "success"), run(4, "deploy.yml", "b", "skipped")})
	if running || len(red) != 1 || red[0].Index != 3 {
		t.Fatalf("red %+v running %v", red, running)
	}
	// A workflow's newer run of the tip replaces its older one.
	if red, _ = redTip([]ActionRun{run(3, "ci.yml", "b", "failure"), run(5, "ci.yml", "b", "success")}); len(red) != 0 {
		t.Fatalf("re-run passed: %+v", red)
	}
	if _, running = redTip([]ActionRun{run(3, "ci.yml", "b", "failure"), run(4, "codeql.yml", "b", "running")}); !running {
		t.Fatal("a running run of the tip isn't waited for")
	}
	if b := parseBaseFault(baseFault{faultRerun{sha: "abc", n: 2, first: 5, last: 6}, baseFaultAsked}.String()); b.sha != "abc" || b.n != 2 || b.last != 6 || b.ended != baseFaultAsked {
		t.Fatalf("round trip: %+v", b)
	}
}

// A base branch's re-run whose attempt can't be recorded isn't asked for:
// unrecorded, its back-off would be lost and the jobs re-run on every pass
// (as #3792 for a pull request's head).
func TestBaseFaultUnrecordedIsNotRerun(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	af := &baseFaultForge{shipForge: se.sg, jobs: map[int64][]ActionJob{}, logs: map[int64]string{}}
	se.s.Forges = af
	af.add(10, "main", "aaaaaaa", "failure", dnsCheckoutLog, se.clockNow().Add(-5*time.Minute))
	for _, ev := range []string{"INSERT", "UPDATE"} {
		if _, err := se.s.db.W.ExecContext(se.ctx, `CREATE TRIGGER no_base_fault_`+ev+` BEFORE `+ev+` ON server_settings
			WHEN NEW.key LIKE 'base_fault:%' BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		se.setClock(se.clockNow().Add(10 * time.Minute))
		se.s.RecordCIHistory(se.ctx)
	}
	if got := af.rerun(); len(got) != 0 {
		t.Fatalf("re-ran unrecorded: %v", got)
	}
	for _, ev := range []string{"INSERT", "UPDATE"} {
		if _, err := se.s.db.W.ExecContext(se.ctx, `DROP TRIGGER no_base_fault_`+ev); err != nil {
			t.Fatal(err)
		}
	}
	se.setClock(se.clockNow().Add(10 * time.Minute))
	se.s.RecordCIHistory(se.ctx)
	if got, st := af.rerun(), se.s.baseFaults(se.ctx, se.f.ID, "me/app")["main"]; len(got) != 1 || st.n != 1 {
		t.Fatalf("recorded: %v %+v", got, st)
	}
}
