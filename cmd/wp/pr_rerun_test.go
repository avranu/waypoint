package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// rerunForge re-runs two failed jobs (WP-1661).
type rerunForge struct {
	core.ForgeClient
	calls []string
	err   error // RerunJobs answers it with the jobs it re-ran
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
	return &core.RerunResult{Jobs: jobs, RunURL: "https://git.example.com/robin/waypoint/actions/runs/9"}, f.err
}

// rerunEnv is a pull request (robin/waypoint#3) on a forge with a token, for
// a CLI signed in as a person: an agent merges only the pull requests it wrote.
func rerunEnv(t *testing.T) (*env, *rerunForge) {
	t.Helper()
	e := newEnv(t)
	me := e.login()
	fg := &rerunForge{}
	e.svc.Forges = fg
	prRepo(t, e, me)
	token := "forge-token"
	if _, err := e.svc.UpdateForge(context.Background(), me, 1, core.ForgeInput{Token: &token}); err != nil {
		t.Fatal(err)
	}
	person, _, err := e.svc.CreateToken(context.Background(), me, core.TokenSpec{Name: "person"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAYPOINT_TOKEN", person)
	gitRepo(t, "git@git.example.com:robin/waypoint.git")
	return e, fg
}

// The refused rest of a partial re-run is said, with where to read what is
// still failed.
func TestCLIPRRerunPartial(t *testing.T) {
	e, fg := rerunEnv(t)
	fg.err = errors.New("502 bad gateway")
	out := e.wp("pr", "rerun", "3")
	for _, want := range []string{"re-ran 2 jobs (backend, e2e)", "not all went again: 502 bad gateway; `wp pr log 3` shows what is still failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q:\n%s", want, out)
		}
	}
}

func TestCLIPRRerun(t *testing.T) {
	e, _ := rerunEnv(t)
	out, code := e.run("pr", "rerun", "3")
	if code != 0 {
		t.Fatalf("rerun: %d %s", code, out)
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

// A blank --job (an unset shell variable) re-runs nothing, from the CLI and
// from the API.
func TestCLIPRRerunBlankJob(t *testing.T) {
	e, fg := rerunEnv(t)
	for _, job := range []string{"", " "} {
		if out, code := e.run("pr", "rerun", "3", "--job", job); code == 0 || !strings.Contains(out, "--job needs a job's name") {
			t.Errorf("--job %q: %d %s", job, code, out)
		}
	}
	req, err := http.NewRequest(http.MethodPost, e.url+"/api/v1/pulls/1/rerun?job=+", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("WAYPOINT_TOKEN"))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "job is blank") {
		t.Errorf("?job=+: %d %s %v", res.StatusCode, body, err)
	}
	if len(fg.calls) != 0 {
		t.Errorf("forge calls %q", fg.calls)
	}
}
