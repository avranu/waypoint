package core

import (
	"errors"
	"strings"
	"testing"
)

// wp ci log reads the jobs of a main push's run (WP-315): by full or
// abbreviated commit, by run number; the failed jobs' failing part, a named
// job whole; and only for repos in projects the actor can see.
func TestCILogs(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Alpha", "kanban")
	a, _, cl := newAPIFake(t, "forgejo")
	f, err := e.s.CreateForge(e.ctx, e.admin, ForgeInput{Kind: ptr("forgejo"), Name: ptr("Forge"), BaseURL: ptr(a.srv.URL), Token: ptr("tok")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', 0)`, p.ID, f.ID); err != nil {
		t.Fatal(err)
	}
	e.s.Forges = cl
	full := strings.Repeat("a", 40)
	run := map[string]any{"id": 700, "index_in_repo": 42, "workflow_id": "ci.yml", "status": "failure", "commit_sha": full,
		"trigger_event": "push", "prettyref": "main", "title": "Merge pull request", "html_url": "https://forge/acme/app/actions/runs/42"}
	a.reply["GET /repos/acme/app/actions/runs?head_sha="+full+"&page=1&limit=50"] = map[string]any{"workflow_runs": []any{run}}
	a.reply["GET /repos/acme/app/actions/runs?page=1&limit=50"] = map[string]any{"workflow_runs": []any{
		map[string]any{"id": 701, "index_in_repo": 43, "commit_sha": strings.Repeat("b", 40)}, run}}
	a.reply["GET /repos/acme/app/actions/runs?run_number=42&limit=1"] = map[string]any{"workflow_runs": []any{run}}
	a.reply["GET /repos/acme/app/actions/runs?run_number=9&limit=1"] = map[string]any{"workflow_runs": []any{}}
	a.reply["GET /repos/acme/app/actions/runs/700/jobs"] = []map[string]any{
		{"id": 7001, "run_id": 700, "name": "current", "status": "success"},
		{"id": 7002, "run_id": 700, "name": "backend", "status": "failure"},
		{"id": 7003, "run_id": 700, "name": "release", "status": "skipped"}}
	a.reply["GET /repos/acme/app/actions/jobs/7001/logs"] = "2026-10-30T10:00:00.0000000Z ::notice title=Already tested::the merged tree is abc's\n"
	a.reply["GET /repos/acme/app/actions/jobs/7002/logs"] = "ok  \tpkg/a\t1.2s\n--- FAIL: TestX (0.00s)\n    x_test.go:9: boom\nFAIL\n"

	l, err := e.s.CILogs(e.ctx, e.admin, CILogInput{Repo: "ACME/app", SHA: full})
	if err != nil {
		t.Fatal(err)
	}
	if l.Repo != "acme/app" || len(l.Runs) != 1 || l.Runs[0].RunNumber != 42 || l.Runs[0].Event != "push" || len(l.Runs[0].Jobs) != 3 {
		t.Fatalf("logs = %+v", l)
	}
	jobs := l.Runs[0].Jobs
	if jobs[0].Picked || jobs[2].Picked || !jobs[1].Picked || !strings.Contains(jobs[1].Text, "TestX") || jobs[1].From != "forge" {
		t.Errorf("jobs = %+v", jobs)
	}
	if a.call("GET", "/repos/acme/app/actions/jobs/7001/logs") != nil {
		t.Error("read a log nobody asked for")
	}

	l, err = e.s.CILogs(e.ctx, e.admin, CILogInput{Repo: "acme/app", SHA: "AAAAAAA", Job: "Curr", Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if j := l.Runs[0].Jobs[0]; !j.Picked || !strings.Contains(j.Log, "Already tested") || strings.Contains(j.Log, "10:00:00") || l.Runs[0].Jobs[1].Picked {
		t.Errorf("named job = %+v", l.Runs[0].Jobs)
	}

	if l, err = e.s.CILogs(e.ctx, e.admin, CILogInput{Repo: "acme/app", Run: 42}); err != nil || len(l.Runs) != 1 {
		t.Fatalf("run 42: %+v, %v", l, err)
	}

	for name, c := range map[string]struct {
		in   CILogInput
		want string
	}{
		"no repo":      {CILogInput{SHA: full}, "OWNER/NAME"},
		"no target":    {CILogInput{Repo: "acme/app"}, "a commit (SHA) or a run number"},
		"both":         {CILogInput{Repo: "acme/app", SHA: full, Run: 4}, "not both"},
		"not a sha":    {CILogInput{Repo: "acme/app", SHA: "main"}, "isn't a commit SHA"},
		"no such run":  {CILogInput{Repo: "acme/app", Run: 9}, "run 9"},
		"no such job":  {CILogInput{Repo: "acme/app", Run: 42, Job: "e2e"}, "their jobs: current, backend, release"},
		"unknown sha":  {CILogInput{Repo: "acme/app", SHA: "deadbeef"}, "no Actions run for deadbeef"},
		"other repo":   {CILogInput{Repo: "acme/other", SHA: full}, "no Forgejo repo"},
		"unattached":   {CILogInput{Repo: "acme", SHA: full}, "OWNER/NAME"},
		"empty target": {CILogInput{Repo: "acme/app", SHA: " "}, "a commit (SHA) or a run number"},
	} {
		if _, err := e.s.CILogs(e.ctx, e.admin, c.in); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}

	_, err = e.s.CILogs(e.ctx, e.user("zed", false), CILogInput{Repo: "acme/app", SHA: full})
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "no Forgejo repo") {
		t.Errorf("stranger: %v", err)
	}
	if _, err := e.s.CILogs(e.ctx, nil, CILogInput{Repo: "acme/app", SHA: full}); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("no actor: %v", err)
	}
}
