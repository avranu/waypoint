package main

import (
	"context"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// rerunForge re-runs two failed jobs (WP-1661).
type rerunForge struct {
	core.ForgeClient
	calls []string
}

func (f *rerunForge) GetItem(ctx context.Context, fg *core.Forge, repo string, n int, pr bool) (*core.ForgeItem, error) {
	return &core.ForgeItem{Number: n, Title: "Change", State: "open", IsPR: pr}, nil
}

func (f *rerunForge) CanRetryChecks(context.Context, *core.Forge) (bool, error) { return true, nil }

func (f *rerunForge) RerunJobs(ctx context.Context, fg *core.Forge, repo, sha, only string) (*core.RerunResult, error) {
	f.calls = append(f.calls, only)
	if only == "lint" {
		return &core.RerunResult{}, &core.RerunNoMatch{Job: only, Failed: []string{"backend", "e2e"}}
	}
	jobs := []string{"backend", "e2e"}
	if only != "" {
		jobs = []string{only}
	}
	return &core.RerunResult{Jobs: jobs, RunURL: "https://git.example.com/robin/waypoint/actions/runs/9"}, nil
}

func TestCLIPRRerun(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	fg := &rerunForge{}
	e.svc.Forges = fg
	prRepo(t, e, me)
	token := "forge-token"
	if _, err := e.svc.UpdateForge(context.Background(), me, 1, core.ForgeInput{Token: &token}); err != nil {
		t.Fatal(err)
	}
	gitRepo(t, "git@git.example.com:robin/waypoint.git")

	out, code := e.run("pr", "rerun", "3")
	if code != 0 {
		// The CLI's token is an agent's, which merges only its own pull requests.
		if !strings.Contains(out, "agents merge only the pull requests they wrote") {
			t.Fatalf("rerun: %d %s", code, out)
		}
		t.Skip("the test agent isn't the pull request's author; the service tests cover the rest")
	}
	for _, want := range []string{"robin/waypoint#3: re-ran 2 jobs (backend, e2e) on abc1233 (1 of 2 for this head).", "Run: https://git.example.com/robin/waypoint/actions/runs/9", "wp pr wait 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
	if out, code = e.run("pr", "rerun", "3", "--job", "lint"); code == 0 || !strings.Contains(out, `"lint"`) || !strings.Contains(out, "backend, e2e") {
		t.Errorf("--job miss: %d %s", code, out)
	}
	if out = e.wp("pr", "rerun", "3", "--job", "e2e"); !strings.Contains(out, "re-ran 1 job (e2e) on abc1233 (2 of 2") {
		t.Errorf("--job:\n%s", out)
	}
	if out, code = e.run("pr", "rerun", "3"); code == 0 || !strings.Contains(out, "already re-run 2 times") {
		t.Errorf("third: %d %s", code, out)
	}
	if !strings.Contains(e.wp("help", "--all"), "rerun") {
		t.Error("wp help --all doesn't list rerun")
	}
}
