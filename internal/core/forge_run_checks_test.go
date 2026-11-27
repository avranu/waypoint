package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// runForge is a Forgejo head with Actions runs (WP-593): its statuses (the
// whole history), runs and their jobs.
type runForge struct {
	repo     string
	statuses []fakeStatus
	runs     []ActionRun
	jobs     map[int64][]ActionJob
	runsErr  int

	mu       sync.Mutex
	paths    []string
	reruns   []int64
	historyN int
}

type fakeStatus struct {
	id      int64
	context string
	status  string
	desc    string
	run     int64
	job     int
}

func (s fakeStatus) json(repo string) map[string]any {
	return map[string]any{"id": s.id, "context": s.context, "status": s.status, "description": s.desc,
		"target_url": fmt.Sprintf("https://forge.test/%s/actions/runs/%d/jobs/%d", repo, s.run, s.job)}
}

var runJobsPath = regexp.MustCompile(`/actions/runs/(\d+)/jobs$`)
var jobRerunPath = regexp.MustCompile(`/actions/jobs/(\d+)/rerun$`)

func (g *runForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.paths = append(g.paths, r.URL.Path)
	enc := json.NewEncoder(w)
	switch p := r.URL.Path; {
	case strings.HasSuffix(p, "/version"):
		_ = enc.Encode(map[string]string{"version": "17.0.1"})
	case strings.HasSuffix(p, "/status"):
		newest := map[string]fakeStatus{}
		for _, s := range g.statuses {
			if o, ok := newest[s.context]; !ok || s.id > o.id {
				newest[s.context] = s
			}
		}
		names := make([]string, 0, len(newest))
		for n := range newest {
			names = append(names, n)
		}
		slices.Sort(names)
		var list []map[string]any
		for _, n := range names {
			list = append(list, newest[n].json(g.repo))
		}
		_ = enc.Encode(map[string]any{"statuses": list})
	case strings.HasSuffix(p, "/statuses"):
		var list []map[string]any
		if r.URL.Query().Get("page") == "1" {
			g.historyN++
			for i := len(g.statuses) - 1; i >= 0; i-- {
				list = append(list, g.statuses[i].json(g.repo))
			}
		}
		_ = enc.Encode(list)
	case strings.HasSuffix(p, "/actions/runs"):
		if g.runsErr != 0 {
			http.Error(w, "no", g.runsErr)
			return
		}
		runs := slices.Clone(g.runs)
		slices.SortFunc(runs, func(a, b ActionRun) int { return cmpInt64(b.Index, a.Index) }) // newest first
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		runs = runs[min((page-1)*50, len(runs)):min(page*50, len(runs))]
		_ = enc.Encode(map[string]any{"workflow_runs": runs})
	case runJobsPath.MatchString(p):
		id, _ := strconv.ParseInt(runJobsPath.FindStringSubmatch(p)[1], 10, 64)
		_ = enc.Encode(g.jobs[id])
	case jobRerunPath.MatchString(p) && r.Method == http.MethodPost:
		id, _ := strconv.ParseInt(jobRerunPath.FindStringSubmatch(p)[1], 10, 64)
		g.reruns = append(g.reruns, id)
		for run, js := range g.jobs {
			for i := range js {
				if js[i].ID == id {
					g.jobs[run][i].Status = "waiting"
				}
			}
		}
	default:
		http.NotFound(w, r)
	}
}

func (g *runForge) run(number, id int64, status string, jobs ...ActionJob) {
	g.runs = append(g.runs, ActionRun{ID: id, Index: number, WorkflowID: "ci.yml", Event: "pull_request", Status: status,
		HTMLURL: fmt.Sprintf("https://forge.test/%s/actions/runs/%d", g.repo, number)})
	if g.jobs == nil {
		g.jobs = map[int64][]ActionJob{}
	}
	for i := range jobs {
		jobs[i].RunID = id
	}
	g.jobs[id] = jobs
}

func (g *runForge) checks(t *testing.T) ([]CommitCheck, *httpForgeClient, *Forge) {
	t.Helper()
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	cl := &httpForgeClient{http: srv.Client()}
	f := &Forge{Kind: "forgejo", BaseURL: srv.URL, token: "t"}
	got, err := cl.CommitChecks(context.Background(), f, g.repo, strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	return got, cl, f
}

func checkNamed(t *testing.T, got []CommitCheck, name string) CommitCheck {
	t.Helper()
	for _, c := range got {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %q in %+v", name, got)
	return CommitCheck{}
}

func job(id int64, name, status string) ActionJob {
	return ActionJob{ID: id, Name: name, Status: status}
}

// WP-591 (#272): run 1705 was cancelled when 1713 started on the same head,
// and its cancellations were posted after some of 1713's statuses. 1713
// passed and skipped release and deploy: the head passed, and nothing is
// re-run.
func TestRunThatCountsCancelledOlderRun(t *testing.T) {
	const backend, release, deploy, gone = "ci / backend (pull_request)", "ci / release (pull_request)", "ci / deploy (pull_request)", "ci / old (pull_request)"
	g := &runForge{repo: "Robin/waypoint"}
	g.run(1705, 3005, "cancelled", job(51, "backend", "cancelled"), job(52, "release", "cancelled"), job(53, "deploy", "cancelled"), job(54, "old", "cancelled"))
	g.run(1713, 3013, "success", job(61, "backend", "success"), job(62, "release", "skipped"), job(63, "deploy", "skipped"))
	g.statuses = []fakeStatus{
		{1, backend, "pending", "Has started running", 1705, 0},
		{2, gone, "pending", "Has started running", 1705, 3},
		{10, backend, "pending", "Waiting to run", 1713, 0},
		{12, release, "skipped", "Has been skipped", 1713, 1},
		{13, backend, "failure", "Has been cancelled", 1705, 0},
		{14, deploy, "failure", "Has been cancelled", 1705, 2},
		{15, release, "failure", "Has been cancelled", 1705, 1},
		{16, gone, "failure", "Has been cancelled", 1705, 3},
		{20, backend, "success", "Successful in 9m", 1713, 0},
	}
	got, cl, f := g.checks(t)
	if st := ciState(got, 0, 1<<40); st != CIPassed {
		t.Fatalf("ci state %q, want passed: %+v", st, got)
	}
	if len(got) != 3 {
		t.Errorf("got %+v, want backend, deploy and release (old has no job in 1713)", got)
	}
	if c := checkNamed(t, got, backend); c.ID != 20 || c.Description != "Successful in 9m" {
		t.Errorf("backend: %+v", c)
	}
	if c := checkNamed(t, got, release); !skippedCheck(c) || c.ID != 12 || !strings.HasSuffix(c.URL, "/runs/1713/jobs/1") {
		t.Errorf("release: %+v, want 1713's skip", c)
	}
	if c := checkNamed(t, got, deploy); !skippedCheck(c) || c.ID != 14 || !strings.HasSuffix(c.URL, "/runs/1713/jobs/2") {
		t.Errorf("deploy: %+v, want 1713's skip, made from its job", c)
	}
	if n, err := cl.RetryChecks(context.Background(), f, g.repo, strings.Repeat("a", 40)); err != nil || n != 0 || len(g.reruns) != 0 {
		t.Errorf("re-run %d jobs %v (err %v), want none: 1705 was replaced", n, g.reruns, err)
	}
}

// WP-158/WP-226: an edit's run whose jobs all skip doesn't replace the
// failed run before it, and the failed run is the one re-run.
func TestRunThatCountsSkippedEditRun(t *testing.T) {
	const e2e, lint = "ci / e2e (pull_request)", "ci / lint (pull_request)"
	g := &runForge{repo: "o/r"}
	g.run(10, 110, "failure", job(1, "lint", "success"), job(2, "e2e", "failure"))
	g.run(11, 111, "skipped", job(3, "lint", "skipped"), job(4, "e2e", "skipped"))
	g.statuses = []fakeStatus{
		{1, lint, "success", "Successful in 1m", 10, 0},
		{5, e2e, "failure", "Failing after 3m", 10, 1},
		{6, lint, "skipped", "Has been skipped", 11, 0},
		{7, e2e, "skipped", "Has been skipped", 11, 1},
	}
	got, cl, f := g.checks(t)
	if st := ciState(got, 0, 1<<40); st != CIFailed {
		t.Fatalf("ci state %q, want failed: %+v", st, got)
	}
	if c := checkNamed(t, got, e2e); c.State != CheckFailed || c.Description != "Failing after 3m" || c.ID != 5 {
		t.Errorf("e2e: %+v, want run 10's failure", c)
	}
	if c := checkNamed(t, got, lint); skippedCheck(c) || c.ID != 1 {
		t.Errorf("lint: %+v, want run 10's pass", c)
	}
	if n, err := cl.RetryChecks(context.Background(), f, g.repo, strings.Repeat("a", 40)); err != nil || n != 1 || !slices.Equal(g.reruns, []int64{2}) {
		t.Errorf("re-ran %d jobs %v (err %v), want run 10's e2e", n, g.reruns, err)
	}
}

// WP-116: a job re-run in the run that counts passes; its earlier failure
// doesn't stand.
func TestRunThatCountsRerunJob(t *testing.T) {
	const e2e = "ci / e2e (pull_request)"
	g := &runForge{repo: "o/r"}
	g.run(10, 110, "success", job(2, "e2e", "success"))
	g.statuses = []fakeStatus{
		{5, e2e, "failure", "Failing after 3m", 10, 0},
		{7, e2e, "pending", "Has started running", 10, 0},
		{8, e2e, "success", "Successful in 4m", 10, 0},
	}
	got, _, _ := g.checks(t)
	if len(got) != 1 || ciState(got, 0, 1<<40) != CIPassed || got[0].ID != 8 {
		t.Fatalf("got %+v, want the re-run's pass", got)
	}
	if g.historyN != 0 {
		t.Errorf("read the history %d times; the combined status was the run that counts", g.historyN)
	}
}

// A newer run that has started counts: its checks are pending, even where
// the combined status still shows the older run's result.
func TestRunThatCountsRunInProgress(t *testing.T) {
	const e2e = "ci / e2e (pull_request)"
	g := &runForge{repo: "o/r"}
	g.run(10, 110, "success", job(2, "e2e", "success"))
	g.run(12, 112, "running", job(3, "e2e", "waiting"))
	g.statuses = []fakeStatus{{8, e2e, "success", "Successful in 4m", 10, 0}}
	got, _, _ := g.checks(t)
	if len(got) != 1 || got[0].State != CheckPending || !strings.HasSuffix(got[0].URL, "/runs/12/jobs/0") {
		t.Fatalf("got %+v, want run 12's waiting job", got)
	}
}

// WP-424: on Juno #77's head the full run failed juno-browser (fail-fast,
// so Forgejo calls it cancelled) and the edit run after it skipped every
// job; the combined status showed only the skip, and the pull request
// merged.
func TestRunThatCountsTrove77(t *testing.T) {
	const browser, deploy = "CI / juno-browser (pull_request)", "CI / deploy (pull_request)"
	g := &runForge{repo: "Robin/Juno"}
	g.run(489, 1489, "skipped", job(1, "lint", "skipped"), job(2, "juno-browser", "skipped"), job(3, "deploy", "skipped"))
	g.run(492, 1492, "cancelled", job(11, "lint", "success"), job(12, "juno-browser", "cancelled"), job(13, "deploy", "cancelled"))
	g.run(494, 1494, "skipped", job(21, "lint", "skipped"), job(22, "juno-browser", "skipped"), job(23, "deploy", "skipped"))
	g.statuses = []fakeStatus{
		{3, browser, "skipped", "Has been skipped", 489, 1},
		{52, browser, "pending", "Blocked by required conditions", 494, 1},
		{60, browser, "pending", "Has started running", 492, 1},
		{69, browser, "failure", "Has been cancelled", 492, 1},
		{70, deploy, "failure", "Has been cancelled", 492, 2},
		{73, browser, "skipped", "Has been skipped", 494, 1},
		{80, deploy, "pending", "Blocked by required conditions", 494, 2},
		{81, deploy, "skipped", "Has been skipped", 494, 2},
	}
	got, _, _ := g.checks(t)
	if len(got) != 2 || ciState(got, 0, 1<<40) != CIFailed {
		t.Fatalf("got %+v, want two failed checks", got)
	}
	for _, c := range got {
		if c.State != CheckFailed || !cancelledCheck(c.Description) || !strings.Contains(c.URL, "/runs/492/") {
			t.Errorf("%s: %+v, want run 492's cancellation", c.Name, c)
		}
	}
	if g.historyN != 1 {
		t.Errorf("read the history %d times, want once", g.historyN)
	}
}

// WP-552: a pull request whose go-unit runs only on drafts: the draft run 247
// ran it, and the full run 248 after ready skipped it without a status. It
// reads as skipped, and the full run has finished.
func TestRunThatCountsDraftOnlyJob(t *testing.T) {
	const unit, full = "ci / go-unit (pull_request)", "ci / full-run (pull_request)"
	g := &runForge{repo: "Assistant/Mail"}
	g.run(247, 1247, "success", job(1, "go-unit", "success"), job(2, "full-run", "skipped"))
	g.run(248, 1248, "success", job(11, "go-unit", "skipped"), job(12, "full-run", "success"))
	g.statuses = []fakeStatus{
		{3, unit, "success", "Successful in 2m", 247, 0},
		{4, full, "skipped", "Has been skipped", 247, 1},
		{9, full, "success", "Successful in 20m", 248, 1},
	}
	got, _, _ := g.checks(t)
	u := checkNamed(t, got, unit)
	if !skippedCheck(u) || !strings.HasSuffix(u.URL, "/runs/248/jobs/0") {
		t.Fatalf("go-unit: %+v, want the full run's skip", u)
	}
	now := time.Now()
	pr := &PullRequest{State: "open", WipAuto: true, ReadyAt: &now, HeadSHA: "abc", readySHA: "abc", readyStatusID: 5, readyChecks: []string{unit, full}}
	var pcs []PRCheck
	for _, c := range got {
		pcs = append(pcs, PRCheck{Name: c.Name, State: c.State, Description: c.Description, StatusID: c.ID})
	}
	if r := fullRunReasons(pr, pcs); len(r) != 0 {
		t.Errorf("full run reasons %v, want none", r)
	}
}

// A forge without the Actions runs API: the newest status per name. Any
// other error fails the read.
func TestRunThatCountsNoRunsAPI(t *testing.T) {
	const e2e = "ci / e2e (pull_request)"
	g := &runForge{repo: "o/r", runsErr: http.StatusNotFound}
	g.statuses = []fakeStatus{{5, e2e, "failure", "Failing", 10, 0}, {6, e2e, "skipped", "Has been skipped", 11, 0}}
	got, _, _ := g.checks(t)
	if len(got) != 1 || got[0].ID != 6 {
		t.Fatalf("got %+v, want the newest status", got)
	}
	g = &runForge{repo: "o/r", runsErr: http.StatusInternalServerError, statuses: g.statuses}
	srv := httptest.NewServer(g)
	defer srv.Close()
	cl := &httpForgeClient{http: srv.Client()}
	if got, err := cl.CommitChecks(context.Background(), &Forge{Kind: "forgejo", BaseURL: srv.URL, token: "t"}, "o/r", "abc"); err == nil {
		t.Fatalf("got %+v, want the runs list's error", got)
	}
}

// Without an Actions status the combined status is the whole answer.
func TestRunThatCountsNotActions(t *testing.T) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		_ = json.NewEncoder(w).Encode(map[string]any{"statuses": []map[string]any{{"id": 4, "context": "forge-checks/codeql", "status": "success", "target_url": "https://forge.test/checks/4"}}})
	}))
	defer srv.Close()
	cl := &httpForgeClient{http: srv.Client()}
	if got, err := cl.CommitChecks(context.Background(), &Forge{Kind: "forgejo", BaseURL: srv.URL, token: "t"}, "o/r", "abc"); err != nil || len(got) != 1 || n != 1 {
		t.Fatalf("got %+v err %v requests %d", got, err, n)
	}
}

func TestCountingRuns(t *testing.T) {
	r := func(n int64, ev, status string) ActionRun {
		return ActionRun{ID: n + 1000, Index: n, WorkflowID: "ci.yml", Event: ev, Status: status}
	}
	got := countingRuns([]ActionRun{r(5, "pull_request", "failure"), r(7, "pull_request", "skipped"), r(6, "push", "skipped"), r(4, "push", "skipped"), r(3, "pull_request", "success")})
	if pr := got[runGroup(r(0, "pull_request", ""))]; pr.Index != 5 {
		t.Errorf("pull_request: run %d, want 5 (7 skipped everything)", pr.Index)
	}
	if push := got[runGroup(r(0, "push", ""))]; push.Index != 6 {
		t.Errorf("push: run %d, want 6 (every run skipped: the newest)", push.Index)
	}
}
