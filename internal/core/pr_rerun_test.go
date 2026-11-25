package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rerunForge answers RerunJobs for wp pr rerun (WP-1661).
type rerunForge struct {
	*shipForge
	mu     sync.Mutex
	failed []string // the failed jobs of the head
	calls  [][2]string
	noRun  bool   // CanRetryChecks says no
	err    error  // RerunJobs answers it, with the jobs it re-ran
	active int    // unfinished runs it reports, with nothing re-run
	cancel func() // called as RerunJobs starts: the caller goes away
}

func (g *rerunForge) CanRetryChecks(ctx context.Context, f *Forge) (bool, error) {
	return !g.noRun, nil
}

func (g *rerunForge) RerunJobs(ctx context.Context, f *Forge, repo, sha, only string) (*RerunResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, [2]string{sha, only})
	if g.cancel != nil {
		g.cancel()
	}
	if err := ctx.Err(); err != nil { // as a real client would
		return nil, err
	}
	if g.active > 0 {
		return &RerunResult{Active: g.active}, nil
	}
	var jobs []string
	for _, n := range g.failed {
		if only == "" || strings.EqualFold(n, only) {
			jobs = append(jobs, n)
		}
	}
	if only != "" && len(jobs) == 0 && len(g.failed) > 0 {
		return &RerunResult{}, &RerunNoMatch{Job: only, Failed: g.failed}
	}
	return &RerunResult{Jobs: jobs, RunURL: "https://git.example.com/me/app/actions/runs/9"}, g.err
}

// manualReruns is what the heads have spent of their re-runs.
func (be *bsEnv) manualReruns() int {
	be.t.Helper()
	var n int
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT COALESCE(SUM(manual_reruns), 0) FROM ci_heads`).Scan(&n); err != nil {
		be.t.Fatal(err)
	}
	return n
}

// seedFailedCheck gives the pull request's head a failed check with a kept log.
func (be *bsEnv) seedFailedCheck(pr *PullRequest, name, log string) {
	be.t.Helper()
	id, err := be.s.trackHead(be.ctx, pr.ForgeID, pr.Repo, pr.HeadSHA)
	if err != nil {
		be.t.Fatal(err)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO ci_checks(head_id, name, state, log, updated_at) VALUES (?, ?, 'failed', ?, ?)`, id, name, log, be.s.nowMS()); err != nil {
		be.t.Fatal(err)
	}
}

// rerunItem is the pull request's history item for a re-run.
func (be *bsEnv) rerunItem(pr *PullRequest) *PRTimelineItem {
	be.t.Helper()
	d, err := be.s.GetPullRequest(be.ctx, be.admin, pr.ID)
	if err != nil {
		be.t.Fatal(err)
	}
	for i := range d.Timeline {
		if d.Timeline[i].Kind == "ci_rerun" {
			return &d.Timeline[i]
		}
	}
	return nil
}

// WP-1661: a re-run is in the pull request's history, with what failed.
func TestRerunPullRequestInPRTimeline(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	be.seedFailedCheck(pr, "ci / backend (pull_request)", "=== RUN   TestExport\n--- FAIL: TestExport (0.02s)\nFAIL\n")
	g := &rerunForge{shipForge: be.sg, failed: []string{"backend"}}
	be.s.Forges = g
	if _, err := be.s.RerunPullRequest(be.ctx, be.agent, pr.ID, ""); err != nil {
		t.Fatal(err)
	}
	it := be.rerunItem(pr)
	if it == nil {
		t.Fatal("no ci_rerun item in the timeline")
	}
	for _, want := range []string{"backend", "1 of 2", "ci / backend (pull_request)", "TestExport"} {
		if !strings.Contains(it.Body, want) {
			t.Errorf("body lacks %q: %q", want, it.Body)
		}
	}
	if it.SHA != pr.HeadSHA || it.Actor == "" {
		t.Errorf("item %+v", it)
	}
	if len(it.Body) > 500 {
		t.Errorf("body is %d bytes", len(it.Body))
	}
}

// A caller that disconnects after the claim doesn't cancel the re-run or
// leave it unrecorded.
func TestRerunPullRequestSurvivesCallerGoingAway(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	ctx, cancel := context.WithCancel(be.ctx)
	defer cancel()
	g := &rerunForge{shipForge: be.sg, failed: []string{"backend"}, cancel: cancel}
	be.s.Forges = g
	if _, err := be.s.RerunPullRequest(ctx, be.agent, pr.ID, ""); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("the fake didn't cancel the request")
	}
	var n int
	if err := be.s.db.R.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'pr.rerun' AND target_id = ?`, pr.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("activity entries %d, %v", n, err)
	}
	if be.rerunItem(pr) == nil {
		t.Error("no history item")
	}
	if got := be.manualReruns(); got != 1 {
		t.Errorf("manual_reruns %d, want 1", got)
	}
}

// Some jobs went again and the forge refused the rest: the answer and the
// activity say so.
func TestRerunPullRequestPartial(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	g := &rerunForge{shipForge: be.sg, failed: []string{"backend", "e2e"}, err: errors.New("POST rerun: 502 bad gateway")}
	be.s.Forges = g
	r, err := be.s.RerunPullRequest(be.ctx, be.agent, pr.ID, "")
	if err != nil || len(r.Jobs) != 2 || r.Count != 1 || !strings.Contains(r.Partial, "502 bad gateway") {
		t.Fatalf("partial: %+v %v", r, err)
	}
	var detail string
	if err := be.s.db.R.QueryRow(`SELECT detail FROM audit_log WHERE action = 'pr.rerun' AND target_id = ?`, pr.ID).Scan(&detail); err != nil || !strings.Contains(detail, "the forge refused the rest: POST rerun: 502") {
		t.Errorf("activity %q, %v", detail, err)
	}
	if got := be.manualReruns(); got != 1 {
		t.Errorf("manual_reruns %d, want 1: jobs went again", got)
	}
}

// A run still going isn't "nothing failed", and costs no re-run.
func TestRerunPullRequestRunsStillGoing(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	g := &rerunForge{shipForge: be.sg, active: 1}
	be.s.Forges = g
	_, err := be.s.RerunPullRequest(be.ctx, be.agent, pr.ID, "")
	if err == nil || !IsInvalid(err) || !strings.Contains(err.Error(), "still going") || !strings.Contains(err.Error(), "wp pr wait 7") || strings.Contains(err.Error(), "nothing failed") {
		t.Fatalf("running: %v", err)
	}
	if got := be.manualReruns(); got != 0 {
		t.Errorf("manual_reruns %d, want 0", got)
	}
}

// A forge error with nothing re-run is an error, and gives the claim back.
func TestRerunPullRequestForgeErrorGivesClaimBack(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	g := &rerunForge{shipForge: be.sg, err: errors.New("forge down")}
	be.s.Forges = g
	_, err := be.s.RerunPullRequest(be.ctx, be.agent, pr.ID, "")
	if err == nil || IsInvalid(err) || !strings.Contains(err.Error(), "forge down") {
		t.Fatalf("forge error: %v", err)
	}
	if got := be.manualReruns(); got != 0 {
		t.Errorf("manual_reruns %d, want 0", got)
	}
}

// Only an open pull request, on a forge with a token, by someone who may merge it.
func TestRerunPullRequestRefusesClosedNoTokenAndStrangers(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	g := &rerunForge{shipForge: be.sg, failed: []string{"backend"}}
	be.s.Forges = g
	ask := func(a *Actor) error { _, err := be.s.RerunPullRequest(be.ctx, a, pr.ID, ""); return err }

	for _, state := range []string{"closed", "merged"} {
		if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET state = ? WHERE id = ?`, state, pr.ID); err != nil {
			t.Fatal(err)
		}
		if err := ask(be.agent); err == nil || !IsInvalid(err) || !strings.Contains(err.Error(), "is "+state) {
			t.Errorf("%s: %v", state, err)
		}
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET state = 'open' WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE forges SET token = '' WHERE id = ?`, pr.ForgeID); err != nil {
		t.Fatal(err)
	}
	if err := ask(be.agent); err == nil || !IsInvalid(err) || !strings.Contains(err.Error(), "can't re-run jobs") {
		t.Errorf("no token: %v", err)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE forges SET token = 'tok' WHERE id = ?`, pr.ForgeID); err != nil {
		t.Fatal(err)
	}
	// Someone with no role on the project can't see the pull request.
	if err := ask(&Actor{User: &User{ID: 99999}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("stranger: %v, want not found", err)
	}
	// An agent of another family may not merge, so may not re-run, another's.
	other := *be.agent
	other.Agent = "openai/gpt-5"
	if err := ask(&other); !errors.Is(err, ErrForbidden) {
		t.Errorf("another agent: %v, want forbidden", err)
	}
	if got := be.manualReruns(); got != 0 || len(g.calls) != 0 {
		t.Errorf("manual_reruns %d, forge calls %v after refusals", got, g.calls)
	}
}

// Five callers at once on a head: exactly two take a re-run.
func TestClaimManualRerunConcurrent(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	headID, err := be.s.trackHead(be.ctx, pr.ForgeID, pr.Repo, pr.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	var claimed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok, err := be.s.claimManualRerun(be.ctx, headID); err != nil {
				t.Error(err)
			} else if ok {
				claimed.Add(1)
			}
		}()
	}
	wg.Wait()
	if claimed.Load() != 2 || be.manualReruns() != 2 {
		t.Errorf("claimed %d, manual_reruns %d, want 2", claimed.Load(), be.manualReruns())
	}
}

func TestRerunPullRequest(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	g := &rerunForge{shipForge: be.sg, failed: []string{"backend", "e2e"}}
	be.s.Forges = g
	ask := func(job string) (*PRRerun, error) { return be.s.RerunPullRequest(be.ctx, be.agent, pr.ID, job) }

	// A name that matches no failed job lists the failed ones, and costs no re-run.
	if _, err := ask("lint"); err == nil || !IsInvalid(err) || !strings.Contains(err.Error(), `"lint"`) || !strings.Contains(err.Error(), "backend, e2e") {
		t.Fatalf("no match: %v", err)
	}
	r, err := ask("e2e")
	if err != nil || len(r.Jobs) != 1 || r.Jobs[0] != "e2e" || r.Count != 1 || r.Max != 2 || r.RunURL == "" {
		t.Fatalf("one job: %+v %v", r, err)
	}
	if r, err = ask(""); err != nil || len(r.Jobs) != 2 || r.Count != 2 {
		t.Fatalf("all jobs: %+v %v", r, err)
	}
	if _, err = ask(""); err == nil || !IsInvalid(err) || !strings.Contains(err.Error(), "already re-run 2 times") || !strings.Contains(err.Error(), "wp pr log 7") {
		t.Fatalf("third: %v", err)
	}
	if len(g.calls) != 3 { // the lint miss, and the two re-runs; the third never asked the forge
		t.Errorf("forge calls %v", g.calls)
	}
	var n int
	if err := be.s.db.R.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'pr.rerun' AND target_id = ?`, pr.ID).Scan(&n); err != nil || n != 2 {
		t.Errorf("activity entries %d, %v", n, err)
	}

	// A new head starts again.
	be.push("8888888bbbb")
	if r, err = ask(""); err != nil || r.Count != 1 || r.Head != "8888888bbbb" {
		t.Fatalf("new head: %+v %v", r, err)
	}
}

func TestRerunPullRequestRefusals(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	g := &rerunForge{shipForge: be.sg}
	be.s.Forges = g
	ask := func() error { _, err := be.s.RerunPullRequest(be.ctx, be.agent, pr.ID, ""); return err }

	// Nothing failed: the attempt isn't spent.
	for i := 0; i < 3; i++ {
		if err := ask(); err == nil || !IsInvalid(err) || !strings.Contains(err.Error(), "nothing failed") || !strings.Contains(err.Error(), "wp pr wait 7") {
			t.Fatalf("nothing failed: %v", err)
		}
	}
	if got := be.manualReruns(); got != 0 {
		t.Errorf("manual_reruns %d after nothing-failed attempts, want 0", got)
	}
	g.failed = []string{"backend"}
	if err := ask(); err != nil {
		t.Fatalf("after nothing-failed attempts: %v", err)
	}
	g.noRun = true
	if err := ask(); err == nil || !IsInvalid(err) || !strings.Contains(err.Error(), "Forgejo 17") {
		t.Fatalf("forge that can't: %v", err)
	}
	if got := be.manualReruns(); got != 1 { // the one that re-ran; the nothing-failed attempts were given back
		t.Errorf("manual_reruns %d, want 1", got)
	}
}

// WP-1661: the Forgejo client re-runs the counting run's failed jobs, or
// the one named, and names a miss's failed jobs.
func TestRerunJobsForgejo(t *testing.T) {
	g := &runForge{repo: "o/r"}
	g.run(10, 110, "failure", job(1, "lint", "success"), job(2, "e2e", "failure"), job(3, "backend", "cancelled"))
	_, cl, f := g.checks(t)
	var rj ForgeJobRerun = cl
	sha := strings.Repeat("a", 40)

	_, err := rj.RerunJobs(context.Background(), f, g.repo, sha, "lint")
	var nm *RerunNoMatch
	if !errors.As(err, &nm) || strings.Join(nm.Failed, ",") != "backend,e2e" || len(g.reruns) != 0 {
		t.Fatalf("passing job named: %v %v reruns %v", err, nm, g.reruns)
	}
	res, err := rj.RerunJobs(context.Background(), f, g.repo, sha, "E2E")
	if err != nil || strings.Join(res.Jobs, ",") != "e2e" || !slices.Equal(g.reruns, []int64{2}) {
		t.Fatalf("one job: %+v %v reruns %v", res, err, g.reruns)
	}
	g.reruns = nil
	res, err = rj.RerunJobs(context.Background(), f, g.repo, sha, "")
	if err != nil || strings.Join(res.Jobs, ",") != "backend" || !slices.Equal(g.reruns, []int64{3}) { // e2e went again above
		t.Fatalf("all: %+v %v reruns %v", res, err, g.reruns)
	}
}

// A running run isn't re-run, and is counted: the answer isn't "nothing failed".
func TestRerunJobsForgejoRunStillGoing(t *testing.T) {
	g := &runForge{repo: "o/r"}
	g.run(10, 110, "running", job(1, "backend", "failure"), job(2, "e2e", "running"))
	_, cl, f := g.checks(t)
	res, err := cl.RerunJobs(context.Background(), f, g.repo, strings.Repeat("a", 40), "")
	if err != nil || len(res.Jobs) != 0 || res.Active != 1 || len(g.reruns) != 0 {
		t.Fatalf("running: %+v %v reruns %v", res, err, g.reruns)
	}
}

// A failed run behind 50 newer skipped ones, on page two, is still re-run:
// by wp pr rerun and by the automatic re-run alike.
func TestRerunJobsForgejoReadsEveryPage(t *testing.T) {
	g := &runForge{repo: "o/r"}
	g.run(1, 101, "failure", job(1, "lint", "success"), job(2, "e2e", "failure"))
	for i := int64(0); i < 50; i++ {
		g.run(100+i, 200+i, "skipped", job(300+i, "lint", "skipped"))
	}
	_, cl, f := g.checks(t)
	sha := strings.Repeat("a", 40)
	res, err := cl.RerunJobs(context.Background(), f, g.repo, sha, "")
	if err != nil || strings.Join(res.Jobs, ",") != "e2e" || !slices.Equal(g.reruns, []int64{2}) {
		t.Fatalf("page two: %+v %v reruns %v", res, err, g.reruns)
	}
	g.jobs[101][1].Status = "failure"
	g.reruns = nil
	if n, err := cl.RetryChecks(context.Background(), f, g.repo, sha); err != nil || n != 1 || !slices.Equal(g.reruns, []int64{2}) {
		t.Fatalf("RetryChecks: %d %v reruns %v", n, err, g.reruns)
	}
}

// The client's github answers: a job by name isn't Unsupported by the forge's
// API but by its shape, and without one it re-runs the head's failed runs.
func TestRerunJobsGitHub(t *testing.T) {
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/actions/runs"):
			_, _ = w.Write([]byte(`{"workflow_runs":[{"id":7,"conclusion":"failure"},{"id":8,"conclusion":"success"}]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rerun-failed-jobs"):
			posted = append(posted, r.URL.Path)
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cl := &httpForgeClient{http: srv.Client()}
	f := &Forge{Kind: "github", BaseURL: srv.URL, token: "t"}
	if _, err := cl.RerunJobs(context.Background(), f, "o/r", "abc", "backend"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("named job: %v, want unsupported", err)
	}
	res, err := cl.RerunJobs(context.Background(), f, "o/r", "abc", "")
	if err != nil || res.Runs != 1 || len(res.Jobs) != 0 || !slices.Equal(posted, []string{"/api/v3/repos/o/r/actions/runs/7/rerun-failed-jobs"}) {
		t.Errorf("github: %+v %v posted %v", res, err, posted)
	}
}

// A failed job that needs another failed job is re-run with it, so naming it
// is refused, and names nothing but the one that is needed.
func TestRerunJobsForgejoNeedsChain(t *testing.T) {
	g := &runForge{repo: "o/r"}
	g.run(10, 110, "failure", job(1, "backend", "failure"), ActionJob{ID: 2, Name: "gate", Status: "failure", Needs: []string{"backend"}})
	_, cl, f := g.checks(t)
	_, err := cl.RerunJobs(context.Background(), f, g.repo, strings.Repeat("a", 40), "gate")
	var ie *InvalidError
	if !errors.As(err, &ie) || !strings.Contains(ie.Msg, "needs another failed job") || len(g.reruns) != 0 {
		t.Fatalf("gate: %v reruns %v", err, g.reruns)
	}
}

// Some jobs went again and the forge then said it can't do the rest: that is
// a partial re-run, recorded and counted, not a failed command.
func TestRerunPullRequestPartialUnsupported(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	g := &rerunForge{shipForge: be.sg, failed: []string{"backend"}, err: fmt.Errorf("%w: no more of these", ErrUnsupported)}
	be.s.Forges = g
	r, err := be.s.RerunPullRequest(be.ctx, be.agent, pr.ID, "")
	if err != nil || r.Count != 1 || !strings.Contains(r.Partial, "no more of these") {
		t.Fatalf("partial: %+v %v", r, err)
	}
	if be.rerunItem(pr) == nil || be.manualReruns() != 1 {
		t.Errorf("not recorded: manual_reruns %d", be.manualReruns())
	}
}

// A count that is claimed again after another gave it back still gets its own
// history item.
func TestRerunPullRequestReusedCountKeepsHistory(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	g := &rerunForge{shipForge: be.sg, failed: []string{"backend"}}
	be.s.Forges = g
	for i := 0; i < 2; i++ {
		if r, err := be.s.RerunPullRequest(be.ctx, be.agent, pr.ID, ""); err != nil || r.Count != 1 || r.Unrecorded {
			t.Fatalf("re-run %d: %+v %v", i, r, err)
		}
		// As an overlapping caller that found nothing to re-run would.
		if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE ci_heads SET manual_reruns = 0`); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT COUNT(*) FROM pr_events WHERE pr_id = ? AND kind = 'ci_rerun'`, pr.ID).Scan(&n); err != nil || n != 2 {
		t.Errorf("history items %d, %v", n, err)
	}
}

// The jobs went again and the record failed: the caller is told.
func TestRerunPullRequestSaysWhenUnrecorded(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	be.s.Forges = &rerunForge{shipForge: be.sg, failed: []string{"backend"}}
	if _, err := be.s.db.W.ExecContext(be.ctx, `CREATE TRIGGER no_rerun_history BEFORE INSERT ON pr_events WHEN NEW.kind = 'ci_rerun' BEGIN SELECT RAISE(ABORT, 'no history'); END`); err != nil {
		t.Fatal(err)
	}
	r, err := be.s.RerunPullRequest(be.ctx, be.agent, pr.ID, "")
	if err != nil || !r.Unrecorded || be.manualReruns() != 1 {
		t.Fatalf("unrecorded: %+v %v, manual_reruns %d", r, err, be.manualReruns())
	}
}

// A commit with exactly the most runs read is read whole; one with more still
// has its newest re-run.
func TestRerunJobsForgejoManyRuns(t *testing.T) {
	for _, extra := range []int64{fullSHARunPages*50 - 1, fullSHARunPages * 50} {
		g := &runForge{repo: "o/r"}
		g.run(100000, 5000, "failure", job(1, "e2e", "failure"))
		for i := int64(0); i < extra; i++ {
			g.run(100+i, 200+i, "skipped")
		}
		_, cl, f := g.checks(t)
		sha := strings.Repeat("a", 40)
		runs, err := cl.ActionRunsAt(context.Background(), f, g.repo, sha)
		if over := extra+1 > fullSHARunPages*50; len(runs) != fullSHARunPages*50 || errors.Is(err, errTooManyRuns) != over || (!over && err != nil) {
			t.Fatalf("%d runs: read %d, %v", extra+1, len(runs), err)
		}
		// The failed run is the newest, so among those read.
		if res, err := cl.RerunJobs(context.Background(), f, g.repo, sha, ""); err != nil || strings.Join(res.Jobs, ",") != "e2e" {
			t.Fatalf("%d runs: %+v %v", extra+1, res, err)
		}
	}
}

// Pruning old heads doesn't give an open pull request's head its re-runs
// back; a closed one's goes.
func TestPruneCIKeepsSpentReruns(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	be.s.Forges = &rerunForge{shipForge: be.sg, failed: []string{"backend"}}
	if _, err := be.s.RerunPullRequest(be.ctx, be.agent, pr.ID, ""); err != nil {
		t.Fatal(err)
	}
	be.setClock(be.clockNow().Add(31 * 24 * time.Hour))
	be.s.pruneCI(be.ctx)
	if got := be.manualReruns(); got != 1 {
		t.Errorf("open: manual_reruns %d, want 1", got)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET state = 'closed' WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	be.s.pruneCI(be.ctx)
	if got := be.manualReruns(); got != 0 {
		t.Errorf("closed: manual_reruns %d, want the head gone", got)
	}
}
