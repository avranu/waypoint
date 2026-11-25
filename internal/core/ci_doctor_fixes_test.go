package core

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// runsForge gives the doctor's forge a list of runs (WP-1851).
type runsForge struct {
	*actionsForge
	runs []ActionRun
}

func (g *runsForge) ActionRuns(context.Context, *Forge, string, []string, int, int) ([]ActionRun, error) {
	return slices.Clone(g.runs), nil // the reader deletes from what it gets, zeroing the array
}

// WP-1851: the mains step reads the merge lines' bases, so it runs after the
// lines are read: a line into release/1.2 shows that branch's runs, not main.
func TestCIDoctorMainsFollowLineBases(t *testing.T) {
	be := newBabysitEnvOn(t, "forgejo")
	be.runnerOnline()
	pr := be.open()
	if _, err := be.s.UpdateForge(be.ctx, be.admin, be.f.ID, ForgeInput{Token: ptr("tok")}); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT OR IGNORE INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, ?, 0)`, be.p.ID, be.f.ID, pr.Repo); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET base_ref = 'release/1.2', armed_at = ?, armed_by = ?, armed_style = 'merge' WHERE id = ?`, be.s.nowMS(), be.admin.User.ID, pr.ID); err != nil {
		t.Fatal(err)
	}
	// The board caches its reads for a fixed-clock test's whole run: step past them.
	be.setClock(be.clockNow().Add(time.Hour))
	be.s.Forges = &runsForge{actionsForge: &actionsForge{shipForge: be.sg}, runs: []ActionRun{{
		ID: 1, Index: 1, WorkflowID: "ci.yml", PrettyRef: "release/1.2", Event: "push", Status: "success",
		CommitSHA: "abc1234", Created: be.clockNow().Add(-time.Hour),
	}}}
	d, err := be.s.CIDoctor(be.ctx, be.admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Lines) != 1 || d.Lines[0].Base != "release/1.2" {
		t.Fatalf("lines: %+v", d.Lines)
	}
	if len(d.Mains) != 1 || d.Mains[0].Branch != "release/1.2" || d.Mains[0].Run != 1 || d.Mains[0].Runs != 1 {
		t.Fatalf("mains %+v, want the line base release/1.2 with its run", d.Mains)
	}
}

// WP-1851: when the forge can't be read and the last read stands in, the
// database steps still read as of now, not as of that read.
func TestCIDoctorStaleSnapshotKeepsNow(t *testing.T) {
	restoreDoctorBudgets(t)
	be := newBabysitEnvOn(t, "forgejo")
	be.runnerOnline()
	pr := be.open()
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET armed_at = ?, armed_by = ?, armed_style = 'merge' WHERE id = ?`, be.s.nowMS(), be.admin.User.ID, pr.ID); err != nil {
		t.Fatal(err)
	}
	be.s.Forges = &actionsForge{shipForge: be.sg}
	first, err := be.s.CIDoctor(be.ctx, be.admin)
	if err != nil || len(first.Missing) != 0 {
		t.Fatalf("first read: %v missing %q", err, first.Missing)
	}
	be.setClock(be.clockNow().Add(5 * time.Hour))
	ciDoctorBudgets.Snapshot = 300 * time.Millisecond
	be.s.Forges = &slowRuns{actionsForge: &actionsForge{shipForge: be.sg}, delay: 5 * time.Second}
	d, err := be.s.CIDoctor(be.ctx, be.admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Missing) != 1 || !strings.Contains(d.Missing[0], "from the last read") {
		t.Fatalf("missing %q, want the last read named", d.Missing)
	}
	if !d.GeneratedAt.Equal(be.clockNow()) {
		t.Errorf("generated at %s, want now %s (the stale read was %s)", d.GeneratedAt, be.clockNow(), first.GeneratedAt)
	}
}

// WP-1851: a partial doctor still tells the admins of a silent host, tells
// none of its own findings, and forgets nothing it told before.
func TestSweepCIHealthPartialTellsHostsKeepsKeys(t *testing.T) {
	restoreDoctorBudgets(t)
	be, _ := armedBabysit(t)
	be.report("runner2", 5<<30)
	be.setClock(be.clockNow().Add(ciHostSilent + time.Hour))
	told := func() int {
		t.Helper()
		var n int
		if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT count(*) FROM notifications WHERE kind = ? AND user_id = ?`, NotifyCI, be.admin.User.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	has := func(key string) bool {
		t.Helper()
		var n int
		if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT count(*) FROM server_settings WHERE key = ?`, key).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	old := ciNotifiedKey + "line_stalled:me/app"
	be.s.putSetting(be.ctx, old, itoa64(be.clockNow().Add(-ciNotifyEvery-time.Hour).UnixMilli()))

	lines := ciDoctorBudgets.Lines
	ciDoctorBudgets.Lines = time.Nanosecond
	be.s.SweepCIHealth(be.ctx)
	if n := told(); n != 1 {
		t.Fatalf("told %d times after a partial sweep, want the silent host once", n)
	}
	if !has(ciNotifiedKey + "host_silent:runner2") {
		t.Errorf("the silent host's key wasn't kept")
	}
	if !has(old) {
		t.Errorf("a partial sweep forgot a finding it told before")
	}
	// The same sweep with the whole doctor does forget what no longer shows.
	ciDoctorBudgets.Lines = lines
	be.s.SweepCIHealth(be.ctx)
	if has(old) {
		t.Errorf("a whole sweep kept a cleared finding's key")
	}
}

// realRerunForge re-runs through the Forgejo client against a fake Forgejo
// (runForge), so RerunPullRequest sees the client's own errors.
type realRerunForge struct {
	*shipForge
	cl *httpForgeClient
	f  *Forge
}

func (g *realRerunForge) CanRetryChecks(context.Context, *Forge) (bool, error) { return true, nil }

func (g *realRerunForge) RerunJobs(ctx context.Context, _ *Forge, repo, sha, only string) (*RerunResult, error) {
	return g.cl.RerunJobs(ctx, g.f, repo, sha, only)
}

// WP-1851: naming a failed job that Forgejo re-runs only with another failed
// job is a 400-class refusal, as the API maps it through RerunPullRequest's
// wrapping, and it costs the head no re-run.
func TestRerunPullRequestJobNeedingAnotherIsInvalid(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	g := &runForge{repo: pr.Repo}
	g.run(10, 110, "failure", job(1, "backend", "failure"), ActionJob{ID: 2, Name: "e2e", Status: "failure", Needs: []string{"backend"}})
	_, cl, f := g.checks(t)
	be.s.Forges = &realRerunForge{shipForge: be.sg, cl: cl, f: f}

	_, err := be.s.RerunPullRequest(be.ctx, be.agent, pr.ID, "e2e")
	if err == nil || !strings.Contains(err.Error(), "needs another failed job") {
		t.Fatalf("error %v, want the job needing another failed job", err)
	}
	if !IsInvalid(err) {
		t.Errorf("not invalid: %T %v", err, err)
	}
	var ie *InvalidError // how the API's writeError picks a 400
	if !errors.As(err, &ie) || !strings.Contains(ie.Msg, "run it without --job") {
		t.Errorf("not an *InvalidError through the wrap: %v", err)
	}
	if len(g.reruns) != 0 {
		t.Errorf("re-ran %v", g.reruns)
	}
	var n int
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT COALESCE(SUM(manual_reruns), 0) FROM ci_heads`).Scan(&n); err != nil || n != 0 {
		t.Errorf("manual_reruns %d, %v, want the attempt given back", n, err)
	}
}
