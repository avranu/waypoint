package core

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
)

// rerunForge answers RerunJobs for wp pr rerun (WP-1661).
type rerunForge struct {
	*shipForge
	mu     sync.Mutex
	failed []string // the failed jobs of the head
	calls  [][2]string
	noRun  bool // CanRetryChecks says no
}

func (g *rerunForge) CanRetryChecks(ctx context.Context, f *Forge) (bool, error) {
	return !g.noRun, nil
}

func (g *rerunForge) RerunJobs(ctx context.Context, f *Forge, repo, sha, only string) (*RerunResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, [2]string{sha, only})
	var jobs []string
	for _, n := range g.failed {
		if only == "" || strings.EqualFold(n, only) {
			jobs = append(jobs, n)
		}
	}
	if only != "" && len(jobs) == 0 && len(g.failed) > 0 {
		return &RerunResult{}, &RerunNoMatch{Job: only, Failed: g.failed}
	}
	return &RerunResult{Jobs: jobs, RunURL: "https://git.example.com/me/app/actions/runs/9"}, nil
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
	g.failed = []string{"backend"}
	if err := ask(); err != nil {
		t.Fatalf("after nothing-failed attempts: %v", err)
	}
	g.noRun = true
	if err := ask(); err == nil || !IsInvalid(err) || !strings.Contains(err.Error(), "Forgejo 17") {
		t.Fatalf("forge that can't: %v", err)
	}
	// Someone with no role on the project can't.
	if _, err := be.s.RerunPullRequest(be.ctx, &Actor{User: &User{ID: 99999}}, pr.ID, ""); err == nil {
		t.Fatal("a stranger re-ran jobs")
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
