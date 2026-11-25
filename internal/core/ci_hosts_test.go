package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func (be *bsEnv) report(host string, diskFree int64, containers ...CIContainerUsage) {
	be.t.Helper()
	if _, err := be.s.ReportCIHost(be.ctx, be.admin, CIHostReport{Forge: "git.example.com", Host: host, Labels: []string{"forgejo-ci", " forgejo-ci ", "forgejo-ci-heavy"},
		Capacity: 8, CPUs: 24, DiskTotal: 256 << 30, DiskFree: diskFree, Containers: containers}); err != nil {
		be.t.Fatal(err)
	}
}

// runnerFaultTexts are the open runner-fault tasks' descriptions and
// comments, one string a task (WP-589).
func (be *bsEnv) runnerFaultTexts() []string {
	be.t.Helper()
	var out []string
	for _, ft := range runnerFaultTasks(be.t, be.s, be.ctx) {
		out = append(out, ft.text)
	}
	return out
}

func (be *bsEnv) retries() []string {
	_, _, _, r := be.sg.snapshot()
	return r
}

// A runner fault is re-run by Waypoint, backing off, for as long as the head
// fails that way: one that keeps coming back is tracked in a task, once, and
// the re-runs go on (WP-231, WP-589, WP-838).
func TestBabysitRerunsRunnerFault(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.sg.sm.Lock()
	be.sg.logs["ci / test"] = enospcLog
	be.sg.sm.Unlock()
	be.ci("7777777aaaa", CheckFailed)
	be.step()
	if b := be.babysit(); len(be.retries()) != 0 || !strings.Contains(b.Wait, "re-runs it shortly") {
		t.Fatalf("first: retries %v wait %q", be.retries(), b.Wait)
	}
	be.setClock(be.clockNow().Add(faultRerunAfter))
	be.step()
	if b := be.babysit(); !slices.Equal(be.retries(), []string{"7777777aaaa"}) || !strings.Contains(b.Wait, "re-ran 1 job (attempt 1)") || len(be.jobs()) != 0 {
		t.Fatalf("re-run: retries %v wait %q", be.retries(), b.Wait)
	}
	be.step() // the old failure, before the forge says it runs again
	if len(be.retries()) != 1 {
		t.Fatalf("re-ran while the first re-run settles: %v", be.retries())
	}
	// Still inside the back-off: the second re-run waits five minutes from the first.
	be.setClock(be.clockNow().Add(faultRerunSettle))
	be.step()
	if b := be.babysit(); len(be.retries()) != 1 || !strings.Contains(b.Wait, "failed again after attempt 1; Waypoint re-runs it at") {
		t.Fatalf("backing off: retries %v wait %q", be.retries(), b.Wait)
	}
	be.setClock(be.clockNow().Add(faultBackoffBase - faultRerunSettle))
	be.step()
	if len(be.retries()) != 2 {
		t.Fatalf("after the first back-off: retries %v", be.retries())
	}
	be.setClock(be.clockNow().Add(faultBackoff(2)))
	be.step()
	if got := be.runnerFaultTexts(); len(be.retries()) != 3 || len(got) != 0 {
		t.Fatalf("third re-run: retries %v tasks %q", be.retries(), got)
	}
	// Three re-runs and still failing: tracked once, and it carries on.
	be.setClock(be.clockNow().Add(faultRerunSettle))
	be.step()
	be.step()
	got := be.runnerFaultTexts()
	if len(got) != 1 || !strings.Contains(got[0], "has re-run them 3 times") || strings.Count(got[0], "Seen again") != 0 {
		t.Fatalf("after three re-runs: %q", got)
	}
	if b := be.babysit(); b.State != BabysitWatching || len(be.jobs()) != 0 || !strings.Contains(b.Wait, runnerFaultTasks(t, be.s, be.ctx)[0].key+" tracks it") {
		t.Fatalf("tracked: %+v jobs %v", b, be.jobs())
	}
	be.setClock(be.clockNow().Add(faultBackoff(3)))
	be.step()
	if got := be.runnerFaultTexts(); len(be.retries()) != 4 || len(got) != 1 || strings.Count(got[0], "Seen again") != 0 {
		t.Fatalf("a fourth re-run reports nothing new: retries %v tasks %q", be.retries(), got)
	}
	// A new head starts over.
	be.push("8888888bbbb")
	be.ci("8888888bbbb", CheckFailed)
	be.setClock(be.clockNow().Add(faultRerunAfter))
	be.step()
	if r := be.retries(); len(r) != 5 || r[4] != "8888888bbbb" {
		t.Fatalf("new head: %v", r)
	}
}

// A re-run that passes is seen even when its webhooks never come: the
// failed head is read again while it waits, so the front moves on instead of
// filing a task to fix a runner that already passed (Juno#135).
func TestBabysitFaultRerunPassSeenWithoutWebhook(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.sg.sm.Lock()
	be.sg.logs["ci / test"] = enospcLog
	be.sg.sm.Unlock()
	be.ci("7777777aaaa", CheckFailed)
	be.step()
	be.setClock(be.clockNow().Add(faultRerunAfter))
	be.step()
	if len(be.retries()) != 1 {
		t.Fatalf("retries %v", be.retries())
	}
	// The re-run passes on the forge; Waypoint hears nothing of it.
	be.sg.setChecks("7777777aaaa", CommitCheck{Name: "ci / test", State: CheckSuccess, URL: "https://ci.example.com/run/1"})
	for range 3 {
		be.setClock(be.clockNow().Add(faultRerunSettle))
		be.step()
	}
	if got := be.runnerFaultTexts(); len(be.retries()) != 1 || len(got) != 0 {
		t.Fatalf("missed the passing re-run: retries %v tasks %q", be.retries(), got)
	}
	if st := be.pr(7).CIState; st == CIFailed {
		t.Fatalf("ci state %q", st)
	}
}

// A forge that finds nothing to re-run gets the settle time for its new
// results and backs off like any other, so a forge that never posts them is
// tracked in a task instead of a silent wait (Juno#117 waited on a fail-fast
// run nothing re-ran).
func TestBabysitFaultNothingToRerun(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.sg.retryNone = true
	be.sg.sm.Lock()
	be.sg.logs["ci / test"] = enospcLog
	be.sg.sm.Unlock()
	be.ci("7777777aaaa", CheckFailed)
	be.step()
	be.setClock(be.clockNow().Add(faultRerunAfter))
	be.step()
	if b := be.babysit(); !strings.Contains(b.Wait, "waiting for the forge's new results (attempt 1)") {
		t.Fatalf("first: wait %q", b.Wait)
	}
	be.step()
	if len(be.retries()) != 1 {
		t.Fatalf("asked again while settling: %v", be.retries())
	}
	be.setClock(be.clockNow().Add(faultBackoff(1)))
	be.step()
	be.step()
	if got := be.runnerFaultTexts(); len(be.retries()) != 2 || len(got) != 0 {
		t.Fatalf("the second re-run's results weren't awaited: retries %v tasks %q", be.retries(), got)
	}
	be.setClock(be.clockNow().Add(faultBackoff(2)))
	be.step()
	be.setClock(be.clockNow().Add(faultRerunSettle))
	be.step()
	if got := be.runnerFaultTexts(); len(be.retries()) != 3 || len(got) != 1 || !strings.Contains(got[0], "has re-run them 3 times") {
		t.Fatalf("retries %v tasks %q", be.retries(), got)
	}
	if len(be.jobs()) != 0 {
		t.Fatalf("a round started: %v", be.jobs())
	}
}

// A host that reports low disk is waited for, and its jobs re-run when it
// has room again; still full half an hour on, it is tracked in a task.
func TestBabysitRerunWaitsForHost(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.report("runner2", 2<<30)
	be.sg.sm.Lock()
	be.sg.logs["ci / test"] = enospcLog
	be.sg.sm.Unlock()
	be.ci("7777777aaaa", CheckFailed)
	be.setClock(be.clockNow().Add(faultRerunAfter))
	be.report("runner2", 2<<30)
	be.step()
	if b := be.babysit(); len(be.retries()) != 0 || !strings.Contains(b.Wait, "when the host recovers (runner2: 2.0 GiB of 256.0 GiB free") {
		t.Fatalf("sick host: retries %v wait %q", be.retries(), b.Wait)
	}
	be.report("runner2", 100<<30)
	be.step()
	if len(be.retries()) != 1 {
		t.Fatalf("recovered host: retries %v", be.retries())
	}
	be.setClock(be.clockNow().Add(faultRerunSettle))
	be.report("runner2", 1<<30)
	be.step()
	if len(be.retries()) != 1 || len(be.runnerFaultTexts()) != 0 {
		t.Fatalf("sick again: %v", be.retries())
	}
	be.setClock(be.clockNow().Add(faultSickFor))
	be.report("runner2", 1<<30)
	be.step()
	if got := be.runnerFaultTexts(); len(got) != 1 || !strings.Contains(got[0], "still not fit") {
		t.Fatalf("sick for long: %q", got)
	}
}

// Reports make the pool, the hosts' health and each job's usage.
func TestCIHostReports(t *testing.T) {
	be := newBabysitEnv(t)
	start := be.clockNow().Add(-10 * time.Minute)
	end := be.clockNow().Add(-time.Minute)
	be.report("runner3", 200<<30,
		CIContainerUsage{TaskID: 11, Job: "test", Name: "FORGEJO-ACTIONS-TASK-11_WORKFLOW-x_JOB-test", Role: "job", Started: start, CPUSeconds: 100, MemPeak: 1 << 30},
		CIContainerUsage{TaskID: 11, Job: "test", Name: "FORGEJO-ACTIONS-TASK-11_WORKFLOW-x_JOB-test-db", Role: "service", Started: start, CPUSeconds: 20, MemPeak: 512 << 20})
	// The next report: it finished; a late lower sample never lowers what was counted.
	be.report("runner3", 200<<30,
		CIContainerUsage{TaskID: 11, Job: "test", Name: "FORGEJO-ACTIONS-TASK-11_WORKFLOW-x_JOB-test", Role: "job", Started: start, Ended: &end, CPUSeconds: 1000, MemPeak: 3 << 29, MemLimit: 7 << 30, CPULimit: 3},
		CIContainerUsage{TaskID: 11, Job: "test", Name: "FORGEJO-ACTIONS-TASK-11_WORKFLOW-x_JOB-test-db", Role: "service", Started: start, Ended: &end, CPUSeconds: 10, MemPeak: 1 << 20})
	be.report("runner2", 5<<30)

	hosts, err := be.s.ciHosts(be.ctx)
	if err != nil {
		t.Fatal(err)
	}
	hs := hosts[be.f.ID]
	if len(hs) != 2 || hs[0].Name != "runner2" || hs[0].Health != "low_disk" || hs[1].Health != "ok" || !slices.Equal(hs[1].Labels, []string{"forgejo-ci", "forgejo-ci-heavy"}) {
		t.Fatalf("hosts: %+v", hs)
	}
	pool := reportedPool(hs, []CIPoolRunner{{Name: "runner1", Capacity: 2}, {Name: "runner3", Capacity: 1}}, 30)
	if len(pool) != 3 || pool[1].Name != "runner3" || pool[1].Capacity != 8 || pool[2].Name != "runner1" {
		t.Fatalf("pool: %+v", pool)
	}
	if a := ciHostAlerts(hs); len(a) != 1 || a[0].Level != "error" || !strings.Contains(a[0].Title, "runner2 is nearly out of disk") {
		t.Fatalf("alerts: %+v", a)
	}

	// Unnamed until the board reads the task.
	if u, err := be.s.CIUsage(be.ctx, be.admin, "", 0); err != nil || len(u) != 0 {
		t.Fatalf("usage before the task is known: %v %v", u, err)
	}
	snap := &ciSnapshot{repos: []*ciRepoData{{ref: ciRepoRef{forge: be.f, repo: "me/app"},
		hist: &ciHistory{tasks: []ActionTask{{ID: 11, Name: "test", WorkflowID: "ci.yml", Status: "success"}}}}}}
	be.s.noteCITasks(be.ctx, snap)
	u, err := be.s.CIUsage(be.ctx, be.admin, "ME/app", 0)
	if err != nil || len(u) != 1 {
		t.Fatalf("usage: %v %v", u, err)
	}
	if g := u[0]; g.Runs != 1 || g.MemMax != 3<<29+512<<20 || g.CPUP50 != 1020 || g.CoresP50 != 1.89 || g.MemLimit != 7<<30 || !slices.Equal(g.Hosts, []string{"runner3"}) {
		t.Fatalf("usage: %+v", g)
	}

	// Stale after a quiet spell.
	be.setClock(be.clockNow().Add(ciHostFresh + time.Second))
	hosts, _ = be.s.ciHosts(be.ctx)
	if hosts[be.f.ID][1].Health != "stale" || reportedPool(hosts[be.f.ID], nil, 30) != nil {
		t.Fatalf("stale: %+v", hosts[be.f.ID])
	}

	// Only admins report, and a token with permissions needs ci.report.
	if _, err := be.s.ReportCIHost(be.ctx, be.agent, CIHostReport{Forge: "nowhere.example", Host: "x"}); err == nil || !strings.Contains(err.Error(), "no forge") {
		t.Fatalf("unknown forge: %v", err)
	}
	limited := *be.admin
	limited.Permissions = PermissionSet{PermPullsRead: true}
	if _, err := be.s.ReportCIHost(be.ctx, &limited, CIHostReport{Forge: "git.example.com", Host: "x"}); err == nil {
		t.Fatal("a token without ci.report reported")
	}
}

// fakeJob is a job of a fake Forgejo run; key is its workflow key, which
// Forgejo's API doesn't give.
type fakeJob struct {
	run, id           int64
	key, name, status string
	needs             []string
}

func TestForgejoRerun(t *testing.T) {
	sha := strings.Repeat("a", 40)
	version := "17.0.1+gitea-1.22.0"
	var posted []string
	jobs := []*fakeJob{
		{run: 3, id: 30, key: "build", name: "build", status: "failure"},
		{run: 3, id: 31, key: "test", name: "test", status: "failure", needs: []string{"build"}},
		{run: 3, id: 32, key: "lint", name: "lint", status: "success"},
		{run: 3, id: 33, key: "e2e", name: "e2e", status: "failure"},
		{run: 5, id: 50, key: "unit", name: "unit", status: "failure"},
		{run: 5, id: 51, key: "browser", name: "browser", status: "cancelled"}, // cancelled by unit's failure
		{run: 5, id: 52, key: "deploy", name: "deploy", status: "cancelled", needs: []string{"browser"}},
		{run: 5, id: 53, key: "lint", name: "lint", status: "success"},
		// A matrix job's name isn't its key: the job that needs it is
		// reopened by its re-run all the same, and not asked for again.
		{run: 5, id: 54, key: "shard", name: "shard 1", status: "failure"},
		{run: 5, id: 55, key: "gather", name: "gather", status: "cancelled", needs: []string{"shard"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/version":
			_ = json.NewEncoder(w).Encode(map[string]string{"version": version})
		case r.URL.Path == "/api/v1/repos/me/app/actions/runs":
			if r.URL.Query().Get("head_sha") != sha {
				t.Errorf("query %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"workflow_runs": []map[string]any{
				{"id": 4, "workflow_id": "ci.yml", "trigger_event": "pull_request", "status": "skipped"}, // an event the workflow ignores
				{"id": 3, "workflow_id": "ci.yml", "trigger_event": "pull_request", "status": "failure"},
				{"id": 2, "workflow_id": "ci.yml", "trigger_event": "pull_request", "status": "failure"}, // older attempt
				{"id": 1, "workflow_id": "lint.yml", "trigger_event": "pull_request", "status": "success"},
				{"id": 5, "workflow_id": "full.yml", "trigger_event": "pull_request", "status": "cancelled"}, // fail-fast
			}})
		case strings.HasPrefix(r.URL.Path, "/api/v1/repos/me/app/actions/runs/") && strings.HasSuffix(r.URL.Path, "/jobs"):
			id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/me/app/actions/runs/"), "/jobs"), 10, 64)
			var out []map[string]any
			for _, j := range jobs {
				if j.run == id {
					out = append(out, map[string]any{"id": j.id, "name": j.name, "status": j.status, "needs": j.needs})
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rerun"):
			id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/repos/me/app/actions/jobs/"), "/rerun"), 10, 64)
			i := slices.IndexFunc(jobs, func(j *fakeJob) bool { return j.id == id })
			if i < 0 || jobs[i].status == "waiting" {
				http.Error(w, "job not done", http.StatusBadRequest) // as Forgejo refuses a reopened job
				return
			}
			posted = append(posted, r.URL.Path)
			// Forgejo reopens the job and, by workflow key, every job that needs it.
			reopen := []*fakeJob{jobs[i]}
			for len(reopen) > 0 {
				j := reopen[0]
				reopen = reopen[1:]
				j.status = "waiting"
				for _, d := range jobs {
					if d.run == j.run && d.status != "waiting" && slices.Contains(d.needs, j.key) {
						reopen = append(reopen, d)
					}
				}
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &httpForgeClient{http: srv.Client()}
	f := &Forge{Kind: "forgejo", BaseURL: srv.URL, token: "t"}
	n, err := c.RetryChecks(context.Background(), f, "me/app", sha)
	if err != nil || n != 5 || !slices.Equal(posted, []string{"/api/v1/repos/me/app/actions/jobs/30/rerun", "/api/v1/repos/me/app/actions/jobs/33/rerun",
		"/api/v1/repos/me/app/actions/jobs/50/rerun", "/api/v1/repos/me/app/actions/jobs/51/rerun", "/api/v1/repos/me/app/actions/jobs/54/rerun"}) {
		t.Fatalf("n %d err %v posted %v", n, err, posted)
	}
	// One run by id: only its jobs (WP-551).
	jobs[0].status, jobs[1].status, jobs[3].status = "failure", "failure", "failure"
	posted = nil
	if n, err := c.RetryRun(context.Background(), f, "me/app", 3); err != nil || n != 2 ||
		!slices.Equal(posted, []string{"/api/v1/repos/me/app/actions/jobs/30/rerun", "/api/v1/repos/me/app/actions/jobs/33/rerun"}) {
		t.Fatalf("one run: n %d err %v posted %v", n, err, posted)
	}
	version = "16.0.5+gitea-1.22.0"
	if ok, err := c.CanRetryChecks(context.Background(), f); ok || err != nil {
		t.Fatalf("16: %v %v", ok, err)
	}
	if _, err := c.RetryChecks(context.Background(), f, "me/app", sha); err == nil || !strings.Contains(err.Error(), "Forgejo 17") {
		t.Fatalf("16: %v", err)
	}
	if forgejoMajor("v17.2.0") != 17 || forgejoMajor("dev") != 0 {
		t.Fatal("forgejoMajor")
	}
}

// One host reporting doesn't shrink the pool to that host: with no pool
// configured, what else is seen running stays as slots that take any label
// (WP-519).
func TestReportedPoolKeepsUnreportedSlots(t *testing.T) {
	hs := []CIHost{{Name: "runner1", Health: "ok", Capacity: 4, Labels: []string{"forgejo-ci", "forgejo-ci-priority"}}}
	pool := reportedPool(hs, nil, 13)
	if len(pool) != 2 || pool[0].Name != "runner1" || pool[1].Name != unreportedRunner || pool[1].Capacity != 9 || len(pool[1].Labels) != 0 {
		t.Fatalf("pool: %+v", pool)
	}
	if p := reportedPool(hs, nil, 3); len(p) != 1 {
		t.Fatalf("the report explains everything seen: %+v", p)
	}
	if p := reportedPool(hs, []CIPoolRunner{{Name: "runner2", Capacity: 3, Labels: []string{"forgejo-ci-heavy"}}}, 13); len(p) != 2 || p[1].Name != "runner2" {
		t.Fatalf("a configured pool is the admin's word: %+v", p)
	}

	now := time.Date(2026, 11, 3, 5, 0, 0, 0, time.UTC)
	jobs := []simJob{
		{id: 1, runsOn: []string{"forgejo-ci"}, running: true, end: now.Add(5 * time.Minute)},
		{id: 2, runsOn: []string{"forgejo-ci-heavy"}, dur: 10 * time.Minute},
	}
	simulate(now, pool, jobs)
	if jobs[1].noRunner || !jobs[1].placed || !jobs[1].start.Equal(now) {
		t.Fatalf("a heavy job waits for nothing on the unreported slots: %+v", jobs[1])
	}
}
