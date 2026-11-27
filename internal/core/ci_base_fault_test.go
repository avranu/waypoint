package core

import (
	"context"
	"fmt"
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
	mu       sync.Mutex
	runs     []ActionRun
	jobs     map[int64][]ActionJob
	logs     map[int64]string
	reruns   []int64
	jobReads int
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
	a.jobReads++
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
	af.add(9, "main", "9999999", "failure", dnsCheckoutLog, failedAt.Add(-time.Hour))
	af.add(10, "main", "aaaaaaa", "failure", dnsCheckoutLog, failedAt)
	af.add(11, "release/v_0_9_0", "bbbbbbb", "failure", dnsCheckoutLog, failedAt)
	af.add(12, "wip", "ccccccc", "failure", dnsCheckoutLog, failedAt)
	sweep := func() {
		t.Helper()
		se.s.Wait()
		se.setClock(se.clockNow().Add(10 * time.Minute))
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
	sweep()
	if got := af.rerun(); len(got) != 2 || state("main").n != 1 {
		t.Fatalf("while running: %v %+v", got, state("main"))
	}
	af.end(10, "failure")
	af.end(11, "success")
	sweep()
	if got := af.rerun(); !slices.Equal(got, []int64{110, 111, 110}) {
		t.Fatalf("second re-runs: %v", got)
	}
	if _, kept := se.s.baseFaults(se.ctx, se.f.ID, "me/app")["release/v_0_9_0"]; kept {
		t.Error("a passed branch's state is kept")
	}
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
	af.add(13, "main", "ddddddd", "failure", "--- FAIL: TestExport (0.01s)\n    export_test.go:12: got 1, want 2\nFAIL\tapp\n[runner]: exitcode '1': failure\n", se.clockNow())
	sweep()
	if got := af.rerun(); len(got) != 5 {
		t.Fatalf("re-ran a code failure: %v", got)
	}
	if st := state("main"); st.sha != "ddddddd" || st.ended != baseFaultOther {
		t.Fatalf("main state after a code failure: %+v", st)
	}
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

// A base branch whose advisories job failed gets one urgent task, and no
// other while that one is open (priority 4 is urgent); the job is never a runner fault and doesn't
// hide one in the same run (WP-1660).
func TestBaseAdvisoriesFileOneTask(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	af := &baseFaultForge{shipForge: se.sg, jobs: map[int64][]ActionJob{}, logs: map[int64]string{}}
	se.s.Forges = af
	advisories := func(n int64, sha string, also ...ActionJob) {
		af.add(n, "main", sha, "failure", "", se.clockNow().Add(-10*time.Minute))
		af.mu.Lock()
		af.jobs[100+n] = append([]ActionJob{{ID: 1000 + n, RunID: 100 + n, Name: advisoriesJob, Status: "failure"}}, also...)
		af.logs[1000+n] = "GO-2026-0001 called\n🏁  Job failed\n"
		af.mu.Unlock()
		se.s.noteCIActive(se.f.ID, "me/app") // the push's webhook
	}
	tasks := func() []string {
		t.Helper()
		rows, err := se.s.db.R.QueryContext(se.ctx, `SELECT i.title || ' ' || i.priority || ' ' || coalesce(i.agent, '') || ' ' || i.description FROM issues i JOIN statuses st ON st.id = i.status_id
			WHERE i.title LIKE 'Fix the advisories on %' AND st.category NOT IN ('done','canceled') ORDER BY i.id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	sweep := func() {
		t.Helper()
		se.s.Wait()
		se.setClock(se.clockNow().Add(10 * time.Minute))
		se.s.RecordCIHistory(se.ctx)
	}

	af.add(10, "main", "aaaaaaa", "failure", "", se.clockNow().Add(-10*time.Minute))
	af.mu.Lock()
	af.jobs[110] = []ActionJob{{ID: 2010, RunID: 110, Name: "backend shard 1", Status: "failure"}, {ID: 1010, RunID: 110, Name: advisoriesJob, Status: "failure"}}
	af.logs[2010] = "--- FAIL: TestX\n🏁  Job failed\n"
	af.logs[1010] = "GO-2026-0001 called\n🏁  Job failed\n"
	af.mu.Unlock()
	sweep()
	got := tasks()
	if len(got) != 1 || !strings.HasPrefix(got[0], "Fix the advisories on me/app main 4 claude ") || !strings.Contains(got[0], "wp ci log --repo me/app --run 10 --job advisories") {
		t.Fatalf("tasks after an advisories failure: %q", got)
	}
	if r := af.rerun(); len(r) != 0 {
		t.Fatalf("an advisories failure was re-run as a runner fault: %v", r)
	}
	advisories(11, "bbbbbbb")
	sweep()
	if got := tasks(); len(got) != 1 {
		t.Fatalf("tasks after a second failure: %q", got)
	}
	advisories(12, "ccccccc", ActionJob{ID: 2012, RunID: 112, Name: "validate", Status: "failure"})
	af.mu.Lock()
	af.logs[2012] = dnsCheckoutLog
	af.mu.Unlock()
	sweep()
	if r := af.rerun(); !slices.Equal(r, []int64{112}) {
		t.Fatalf("a runner fault beside advisories: re-runs %v", r)
	}
}

// The advisories job also runs daily from its own workflow (event schedule)
// and by hand: a failure there files the task whether or not a push run is
// red, one run is looked at once whatever became of its task, a filing that
// failed or had nobody to ask is told apart, and each newer failed run files
// again once the earlier task is closed (WP-1660).
func TestBaseAdvisoriesScheduledRuns(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	af := &baseFaultForge{shipForge: se.sg, jobs: map[int64][]ActionJob{}, logs: map[int64]string{}}
	se.s.Forges = af
	run := func(n int64, event, status, advisories string, others ...ActionJob) {
		t.Helper()
		af.add(n, "main", fmt.Sprintf("%07d", n), status, "", se.clockNow().Add(-10*time.Minute))
		af.mu.Lock()
		defer af.mu.Unlock()
		for i := range af.runs {
			if af.runs[i].Index == n {
				af.runs[i].Event = event
			}
		}
		af.jobs[100+n] = append([]ActionJob{{ID: 1000 + n, RunID: 100 + n, Name: advisoriesJob, Status: advisories}}, others...)
		af.logs[1000+n] = "GO-2026-0001 called\n"
	}
	open := func() int {
		t.Helper()
		var n int
		if err := se.s.db.R.QueryRowContext(se.ctx, `SELECT count(*) FROM issues i JOIN statuses st ON st.id = i.status_id
			WHERE i.title LIKE 'Fix the advisories on %' AND st.category NOT IN ('done','canceled')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	closeTasks := func() {
		t.Helper()
		if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE issues SET status_id = (SELECT st.id FROM statuses st WHERE st.project_id = issues.project_id AND st.category = 'done' LIMIT 1)
			WHERE title LIKE 'Fix the advisories on %'`); err != nil {
			t.Fatal(err)
		}
	}
	// a scheduled run sends no webhook: a quiet repo's next full read sees it
	sweep := func() {
		t.Helper()
		se.s.Wait()
		se.setClock(se.clockNow().Add(ciIdleReread))
		se.s.RecordCIHistory(se.ctx)
	}
	total := func() int {
		t.Helper()
		var n int
		if err := se.s.db.R.QueryRowContext(se.ctx, `SELECT count(*) FROM issues WHERE title LIKE 'Fix the advisories on %'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	run(5, "push", "success", "success")
	run(6, "schedule", "failure", "failure")
	sweep()
	if open() != 1 {
		t.Fatalf("a failed scheduled run filed %d tasks", open())
	}
	sweep()
	closeTasks()
	sweep()
	if total() != 1 {
		t.Fatalf("the same run filed again: %d tasks", total())
	}
	run(7, "schedule", "failure", "failure")
	sweep()
	if total() != 2 || open() != 1 {
		t.Fatalf("a newer failed run: %d tasks, %d open", total(), open())
	}
	closeTasks()
	run(8, "push", "failure", "failure", ActionJob{ID: 2008, RunID: 108, Name: "backend shard 1", Status: "failure"})
	af.mu.Lock()
	af.logs[2008] = "--- FAIL: TestX\n"
	af.mu.Unlock()
	sweep()
	closeTasks()
	run(9, "push", "failure", "failure")
	af.mu.Lock()
	af.jobs[109] = []ActionJob{{ID: 2009, RunID: 109, Name: "backend shard 1", Status: "failure"}, {ID: 1009, RunID: 109, Name: advisoriesJob, Status: "failure"}}
	af.logs[2009] = "--- FAIL: TestX\n"
	af.mu.Unlock()
	if open() != 0 || total() != 3 {
		t.Fatalf("after run 8: %d open, %d total", open(), total())
	}
	sweep()
	if open() != 1 || total() != 4 {
		t.Fatalf("after run 9: %d open, %d total", open(), total())
	}
	closeTasks()
	run(10, "schedule", "failure", "cancelled")
	sweep()
	reads := af.jobReads
	run(11, "schedule", "success", "success")
	sweep()
	if total() != 4 || af.jobReads != reads {
		t.Fatalf("cancelled or passing run: %d tasks, %d job reads (was %d)", total(), af.jobReads, reads)
	}

	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE users SET disabled = 1`); err != nil {
		t.Fatal(err)
	}
	run(12, "schedule", "failure", "failure")
	sweep()
	if _, err := se.s.db.W.ExecContext(se.ctx, `UPDATE users SET disabled = 0`); err != nil {
		t.Fatal(err)
	}
	sweep()
	if total() != 4 {
		t.Fatalf("filed with no owner, or retried the run: %d tasks", total())
	}
	run(13, "workflow_dispatch", "failure", "failure")
	sweep()
	if total() != 5 || open() != 1 {
		t.Fatalf("the next run after a no-owner one: %d tasks, %d open", total(), open())
	}

	closeTasks()
	if _, err := se.s.db.W.ExecContext(se.ctx, `CREATE TRIGGER no_advisories_task BEFORE INSERT ON issues
		WHEN NEW.title LIKE 'Fix the advisories on %' BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	run(14, "schedule", "failure", "failure")
	sweep()
	if total() != 5 {
		t.Fatalf("filed through a failing write: %d tasks", total())
	}
	if _, err := se.s.db.W.ExecContext(se.ctx, `DROP TRIGGER no_advisories_task`); err != nil {
		t.Fatal(err)
	}
	sweep()
	if total() != 6 || open() != 1 {
		t.Fatalf("after the write works: %d tasks, %d open", total(), open())
	}
}

// The advisories job runs only on the base, so its failure there isn't one a
// pull request can fix first (WP-1660).
func TestBaseFailingSkipsAdvisories(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	pr := &PullRequest{ForgeID: se.f.ID, Repo: "me/app", BaseRef: "main"}
	const tip = "b0b0b0b0b0b"
	if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO branch_activity(forge_id, repo, branch, head_sha, pushed_at) VALUES (?,?,?,?,?)`, pr.ForgeID, pr.Repo, "main", tip, se.s.nowMS()); err != nil {
		t.Fatal(err)
	}
	headID, err := se.s.trackHead(se.ctx, pr.ForgeID, pr.Repo, tip)
	if err != nil {
		t.Fatal(err)
	}
	add := func(name string) {
		t.Helper()
		if _, err := se.s.db.W.ExecContext(se.ctx, `INSERT INTO ci_checks(head_id, name, state, description, updated_at) VALUES (?,?,?,?,?)`, headID, name, CheckFailed, "Failing after 3m", se.s.nowMS()); err != nil {
			t.Fatal(err)
		}
	}
	add("ci / advisories (push)")
	if _, failing, err := se.s.baseFailing(se.ctx, pr); err != nil || len(failing) != 0 {
		t.Fatalf("advisories alone: failing = %v (%v)", failing, err)
	}
	add("ci / backend (push)")
	if _, failing, err := se.s.baseFailing(se.ctx, pr); err != nil || len(failing) != 1 {
		t.Fatalf("advisories and backend: failing = %v (%v)", failing, err)
	}
}

// A read that fails while looking for whom to ask isn't "nobody to ask":
// the run stays unseen and the next pass files the task (audit #1485).
func TestBaseAdvisoriesOwnerReadFails(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	af := &baseFaultForge{shipForge: se.sg, jobs: map[int64][]ActionJob{}, logs: map[int64]string{}}
	se.s.Forges = af
	af.add(6, "main", "0000006", "failure", "", se.clockNow().Add(-10*time.Minute))
	af.mu.Lock()
	for i := range af.runs {
		af.runs[i].Event = "schedule"
	}
	af.jobs[106] = []ActionJob{{ID: 1006, RunID: 106, Name: advisoriesJob, Status: "failure"}}
	af.logs[1006] = "GO-2026-0001 called\n"
	af.mu.Unlock()
	tasks := func() int {
		t.Helper()
		var n int
		if err := se.s.db.R.QueryRowContext(se.ctx, `SELECT count(*) FROM issues WHERE title LIKE 'Fix the advisories on %'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	sweep := func() {
		t.Helper()
		se.s.Wait()
		se.setClock(se.clockNow().Add(10 * time.Minute))
		se.s.RecordCIHistory(se.ctx)
		se.s.Wait()
	}
	rename := func(from, to string) {
		t.Helper()
		if _, err := se.s.db.W.ExecContext(se.ctx, `ALTER TABLE `+from+` RENAME TO `+to); err != nil {
			t.Fatal(err)
		}
	}
	rename("project_members", "project_members_gone")
	sweep()
	rename("project_members_gone", "project_members")
	if tasks() != 0 {
		t.Fatalf("filed %d tasks with the owner unreadable", tasks())
	}
	sweep()
	if tasks() != 1 {
		t.Fatalf("after the read works again: %d tasks, want 1", tasks())
	}
}

// What a base branch is still owed is done by the next pass, however quiet
// its repo is: a filing that failed doesn't wait for the idle repo's next
// full read (WP-1980).
func TestBaseAdvisoriesRetriedInQuietRepo(t *testing.T) {
	se := newShipEnv(t, "forgejo")
	af := &baseFaultForge{shipForge: se.sg, jobs: map[int64][]ActionJob{}, logs: map[int64]string{}}
	se.s.Forges = af
	af.add(6, "main", "0000006", "failure", "", se.clockNow().Add(-time.Hour))
	af.mu.Lock()
	af.runs[0].Event = "schedule"
	af.jobs[106] = []ActionJob{{ID: 1006, RunID: 106, Name: advisoriesJob, Status: "failure"}}
	af.logs[1006] = "GO-2026-0001 called\n"
	af.mu.Unlock()
	tasks := func() (n int) {
		t.Helper()
		if err := se.s.db.R.QueryRowContext(se.ctx, `SELECT count(*) FROM issues WHERE title LIKE 'Fix the advisories on %'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	exec := func(q string) {
		t.Helper()
		if _, err := se.s.db.W.ExecContext(se.ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	sweep := func() {
		t.Helper()
		se.s.Wait()
		se.setClock(se.clockNow().Add(time.Minute))
		se.s.RecordCIHistory(se.ctx)
	}
	idle := func() bool {
		se.s.ciBoard.mu.Lock()
		defer se.s.ciBoard.mu.Unlock()
		return se.s.ciBoard.idle(ciGateKey(se.f.ID, "me/app"), se.clockNow(), false)
	}
	exec(`CREATE TRIGGER no_advisories_task BEFORE INSERT ON issues
		WHEN NEW.title LIKE 'Fix the advisories on %' BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
	sweep()
	if tasks() != 0 || idle() {
		t.Fatalf("a failed filing: %d tasks, idle %v", tasks(), idle())
	}
	exec(`DROP TRIGGER no_advisories_task`)
	sweep()
	if tasks() != 1 || !idle() {
		t.Fatalf("the next pass: %d tasks, idle %v", tasks(), idle())
	}
}
