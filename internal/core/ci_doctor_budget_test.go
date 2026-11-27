package core

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// slowRuns is a forge whose list of runs takes longer than the doctor will
// wait (WP-1851).
type slowRuns struct {
	*actionsForge
	delay time.Duration
}

func (g *slowRuns) ActionRuns(ctx context.Context, f *Forge, repo string, st []string, page, limit int) ([]ActionRun, error) {
	select {
	case <-time.After(g.delay):
	case <-ctx.Done():
	}
	return g.actionsForge.ActionRuns(ctx, f, repo, st, page, limit)
}

func restoreDoctorBudgets(t *testing.T) {
	t.Helper()
	was := ciDoctorBudgets
	t.Cleanup(func() { ciDoctorBudgets = was })
}

// WP-1851: a forge that is slow to list runs doesn't fail the doctor: it
// answers within its budget with what the database says and names what it
// couldn't read, and the sweep that notifies admins doesn't take that for
// "everything cleared".
func TestCIDoctorSlowForgePartial(t *testing.T) {
	restoreDoctorBudgets(t)
	ciDoctorBudgets.Snapshot = 300 * time.Millisecond
	be := newBabysitEnvOn(t, "forgejo")
	be.s.Forges = &slowRuns{actionsForge: &actionsForge{shipForge: be.sg}, delay: 5 * time.Second}

	start := time.Now()
	d, err := be.s.CIDoctor(be.ctx, be.admin)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("a slow forge failed the doctor: %v", err)
	}
	if took > 3*time.Second {
		t.Errorf("answered after %s, want within the snapshot's 300ms and the rest", took)
	}
	if len(d.Missing) != 1 || !strings.Contains(d.Missing[0], "the forge's runs and jobs") || !strings.Contains(d.Missing[0], "not read in time") {
		t.Fatalf("missing %q, want the forge's runs and jobs", d.Missing)
	}
	var partial *CIFinding
	for i, f := range d.Findings {
		if f.Kind == "doctor_partial" {
			partial = &d.Findings[i]
		}
	}
	if partial == nil || partial.Level != "warn" || !strings.Contains(partial.Title, "the forge's runs and jobs") {
		t.Errorf("no finding names the missing part: %+v", d.Findings)
	}
	if d.Verdict == "ok" {
		t.Errorf("a partial answer says ok")
	}
}

// WP-1851: a step that runs out of its own time leaves its part out and says
// which; the others still answer: the main branches' runs, which the lines
// step doesn't feed when it left nothing, are still read.
func TestCIDoctorStepOutOfTime(t *testing.T) {
	restoreDoctorBudgets(t)
	be := newBabysitEnvOn(t, "forgejo")
	be.runnerOnline()
	pr := be.open()
	if _, err := be.s.UpdateForge(be.ctx, be.admin, be.f.ID, ForgeInput{Token: ptr("tok")}); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT OR IGNORE INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, ?, 0)`, be.p.ID, be.f.ID, pr.Repo); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET armed_at = ?, armed_by = ?, armed_style = 'merge' WHERE id = ?`, be.s.nowMS(), be.admin.User.ID, pr.ID); err != nil {
		t.Fatal(err)
	}
	be.setClock(be.clockNow().Add(time.Hour)) // past the board's cached reads
	be.s.Forges = &runsForge{actionsForge: &actionsForge{shipForge: be.sg}, runs: []ActionRun{{
		ID: 1, Index: 1, WorkflowID: "ci.yml", PrettyRef: "main", Event: "push", Status: "success",
		CommitSHA: "abc1234", Created: be.clockNow().Add(-time.Hour),
	}}}
	ciDoctorBudgets.Lines = time.Nanosecond
	d, err := be.s.CIDoctor(be.ctx, be.admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Missing) != 1 || !strings.Contains(d.Missing[0], "the merge lines") {
		t.Fatalf("missing %q, want only the merge lines", d.Missing)
	}
	if len(d.Mains) != 1 || d.Mains[0].Branch != "main" || d.Mains[0].Run != 1 {
		t.Errorf("the other steps stopped answering: mains %+v", d.Mains)
	}
}

// c7127: the jobs that ran out of time are their own step, so when only
// their read is cut short the hosts' runs aren't said to be missing.
func TestCIDoctorTimeoutsStepOwnsItsMiss(t *testing.T) {
	restoreDoctorBudgets(t)
	be := newBabysitEnvOn(t, "forgejo")
	ciDoctorBudgets.Timeouts = time.Nanosecond
	d, err := be.s.CIDoctor(be.ctx, be.admin)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Missing) != 1 || !strings.Contains(d.Missing[0], "the jobs that ran out of time") {
		t.Fatalf("missing %q, want only the jobs that ran out of time", d.Missing)
	}
}

// 2093.3: a role read that fails isn't kept as "not visible": the answer
// says what it couldn't read rather than leaving the repo out unsaid.
func TestCIDoctorRoleReadFailureIsMissing(t *testing.T) {
	restoreDoctorBudgets(t)
	be := newBabysitEnvOn(t, "forgejo")
	pr := be.open()
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT OR IGNORE INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, ?, 0)`, be.p.ID, be.f.ID, pr.Repo); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET armed_at = ?, armed_by = ?, armed_style = 'merge' WHERE id = ?`, be.s.nowMS(), be.admin.User.ID, pr.ID); err != nil {
		t.Fatal(err)
	}
	viewer := be.user("vic", false)
	if err := be.s.SetMember(be.ctx, be.admin, be.p.Key, viewer.User.Username, "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `ALTER TABLE project_members RENAME TO project_members_away`); err != nil {
		t.Fatal(err)
	}
	d, err := be.s.CIDoctor(be.ctx, viewer)
	if _, rerr := be.s.db.W.ExecContext(context.Background(), `ALTER TABLE project_members_away RENAME TO project_members`); rerr != nil {
		t.Fatal(rerr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(d.Missing, func(m string) bool { return strings.Contains(m, "what you may see of some repos (failed:") }) || d.Verdict == "ok" {
		t.Fatalf("missing %q, verdict %q", d.Missing, d.Verdict)
	}
}
