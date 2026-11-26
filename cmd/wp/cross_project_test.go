package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// lostForge finds nothing: these tests' repositories exist only here, and
// refreshing a link doesn't ask the real GitHub API (WP-1065).
type lostForge struct{}

func (lostForge) GetItem(context.Context, *core.Forge, string, int, bool) (*core.ForgeItem, error) {
	return nil, errors.New("HTTP 404")
}

func (lostForge) ListIssues(context.Context, *core.Forge, string, time.Time) ([]core.ForgeItem, error) {
	return nil, nil
}

func (lostForge) SetIssueState(context.Context, *core.Forge, string, int, bool) error {
	return errors.New("HTTP 404")
}

// crossEnv is a repo attached to one project, with a task there and a task
// in another project.
func crossEnv(t *testing.T) (e *env, me *core.Actor, home, other *core.Project, homeTask, otherTask *core.IssueDetail) {
	t.Helper()
	e = newEnv(t)
	e.svc.Forges = lostForge{}
	me = e.login()
	ctx := context.Background()
	mk := func(name string) *core.Project {
		p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: &name})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	task := func(p *core.Project, title string) *core.IssueDetail {
		i, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: title, Agent: "any"})
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	home, other = mk("Harbor"), mk("Tools")
	f, err := e.svc.CreateForge(ctx, me, core.ForgeInput{Kind: ptr("github")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AddProjectRepo(ctx, me, home.Key, f.ID, "robin/harbor", false); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	homeTask, otherTask = task(home, "Fix the map"), task(other, "Sharpen the saw")
	return e, me, home, other, homeTask, otherTask
}

func historyHas(t *testing.T, e *env, me *core.Actor, key, reason string) bool {
	t.Helper()
	events, err := e.svc.History(context.Background(), me, key, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Field == "cross-project reason" && ev.New == reason {
			return true
		}
	}
	return false
}

func TestNoScopeStopsWorkCommands(t *testing.T) {
	e, _, _, other, _, otherTask := crossEnv(t)
	t.Chdir(t.TempDir())
	for _, args := range [][]string{{"ls"}, {"next"}, {"batch"}, {"search", "saw"}, {"next", "--peek"}} {
		if out, code := e.run(args...); code == 0 || !strings.Contains(out, "no project here: pass -p KEY, or -A for every project") {
			t.Errorf("wp %v with no scope: %d %s", args, code, out)
		}
	}
	if out := e.wp("ls", "-A"); !strings.Contains(out, "Sharpen") || !strings.Contains(out, "Fix the map") {
		t.Errorf("ls -A:\n%s", out)
	}
	if out := e.wp("search", "saw", "-A"); !strings.Contains(out, "Sharpen") {
		t.Errorf("search -A:\n%s", out)
	}
	if out := e.wp("next", "--peek", "-A"); !strings.Contains(out, "Fix the map") && !strings.Contains(out, "Sharpen") {
		t.Errorf("next --peek -A looks at every project:\n%s", out)
	}
	if out, code := e.run("next", otherTask.Key); code == 0 || !strings.Contains(out, "-p "+other.Key) {
		t.Errorf("next KEY with no scope: %d %s", code, out)
	}
	if out := e.wp("next", otherTask.Key, "-p", other.Key, "--no-brief"); !strings.Contains(out, otherTask.Key) {
		t.Errorf("-p names its project:\n%s", out)
	}
}

func TestCrossProjectTakeNeedsReason(t *testing.T) {
	e, me, _, other, homeTask, otherTask := crossEnv(t)
	gitRepo(t, "git@github.com:robin/harbor.git")

	for _, args := range [][]string{{"next", otherTask.Key}, {"take", otherTask.Key}, {"batch", otherTask.Key},
		{"next", otherTask.Key, "-p", other.Key}, {"next", otherTask.Key, "-A"}, {"next", otherTask.Key, "--cross-project", "  "}} {
		out, code := e.run(args...)
		if code == 0 {
			t.Fatalf("wp %v took it:\n%s", args, out)
		}
		for _, want := range []string{"outside the scope here", "wp new -p " + other.Key, "wp start " + otherTask.Key, "--cross-project"} {
			if !strings.Contains(out, want) {
				t.Errorf("wp %v refusal lacks %q:\n%s", args, want, out)
			}
		}
	}

	if out := e.wp("next", homeTask.Key, "--cross-project", "not needed", "--no-brief"); !strings.Contains(out, homeTask.Key) {
		t.Errorf("own task:\n%s", out)
	}
	if historyHas(t, e, me, homeTask.Key, "not needed") {
		t.Error("a task in the project recorded a cross-project reason")
	}

	const why = "the fix has to land with APP's map change"
	if out := e.wp("next", otherTask.Key, "--cross-project", why, "--no-brief"); !strings.Contains(out, otherTask.Key) {
		t.Errorf("with a reason:\n%s", out)
	}
	if !historyHas(t, e, me, otherTask.Key, why) {
		t.Error("the reason isn't on the task's history")
	}
}

func TestKeylessClaimBeyondHomeNeedsReason(t *testing.T) {
	e, me, _, other, _, otherTask := crossEnv(t)
	gitRepo(t, "git@github.com:robin/harbor.git")
	for _, args := range [][]string{{"next", "-A"}, {"next", "-p", other.Key}, {"batch", "-p", other.Key}} {
		if out, code := e.run(args...); code == 0 || !strings.Contains(out, "--cross-project") {
			t.Errorf("wp %v: %d %s", args, code, out)
		}
	}
	if out := e.wp("next", "--peek", "-p", other.Key); !strings.Contains(out, "Sharpen") {
		t.Errorf("peek at another project:\n%s", out)
	}
	if out := e.wp("next", "-p", other.Key, "--cross-project", "draining Tools", "--no-brief"); !strings.Contains(out, otherTask.Key) {
		t.Errorf("-p with a reason:\n%s", out)
	}
	if !historyHas(t, e, me, otherTask.Key, "draining Tools") {
		t.Error("the reason isn't on the task's history")
	}
}

func TestSessionStartedForTaskNeedsNoReason(t *testing.T) {
	e, _, _, _, _, otherTask := crossEnv(t)
	gitRepo(t, "git@github.com:robin/harbor.git")
	t.Setenv("WP_TASK", otherTask.Key)
	if out := e.wp("next", otherTask.Key, "--no-brief"); !strings.Contains(out, otherTask.Key) {
		t.Errorf("the session's own task:\n%s", out)
	}
}

func TestUnattachedRepoCountsAsNoHome(t *testing.T) {
	e, _, _, other, _, otherTask := crossEnv(t)
	gitRepo(t, "git@github.com:robin/elsewhere.git")
	if out, code := e.run("ls"); code == 0 || !strings.Contains(out, "isn't attached") {
		t.Errorf("ls in an unattached repo: %d %s", code, out)
	}
	if out := e.wp("next", otherTask.Key, "-p", other.Key, "--no-brief"); !strings.Contains(out, otherTask.Key) {
		t.Errorf("-p in an unattached repo:\n%s", out)
	}
}

func claimedBy(t *testing.T, e *env, me *core.Actor, key string) string {
	t.Helper()
	i, err := e.svc.GetIssue(context.Background(), me, key)
	if err != nil {
		t.Fatal(err)
	}
	return i.ClaimedBy
}

// A scope that only partly lies inside the session's project is a crossing.
func TestPartlyOutsideScopeNeedsReason(t *testing.T) {
	e, me, home, other, _, _ := crossEnv(t)
	ctx := context.Background()
	gitRepo(t, "git@github.com:robin/harbor.git")
	name := "Group"
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	parent := p.Key
	for _, child := range []string{home.Key, other.Key} {
		if _, err := e.svc.UpdateProject(ctx, me, child, core.ProjectInput{Parent: core.Some(parent)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"next", "-p", parent}, {"batch", "-p", parent}} {
		if out, code := e.run(args...); code == 0 || !strings.Contains(out, "--cross-project") {
			t.Errorf("wp %v: %d %s", args, code, out)
		}
	}
	if out := e.wp("next", "-p", home.Key, "--no-brief"); !strings.Contains(out, "Fix the map") {
		t.Errorf("-p of its own project:\n%s", out)
	}
}

// Batch leaves a related task in another project alone unless told why.
func TestBatchStaysInsideWithoutReason(t *testing.T) {
	e, me, home, _, homeTask, otherTask := crossEnv(t)
	ctx := context.Background()
	gitRepo(t, "git@github.com:robin/harbor.git")
	near, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: home.Key, Title: "Next door", Agent: "any"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{near.Key, otherTask.Key} {
		if err := e.svc.AddRelation(ctx, me, homeTask.Key, "relates", key); err != nil {
			t.Fatal(err)
		}
	}
	e.wp("batch", homeTask.Key, "--no-brief")
	if claimedBy(t, e, me, near.Key) == "" {
		t.Error("the related task in the project wasn't claimed")
	}
	if got := claimedBy(t, e, me, otherTask.Key); got != "" {
		t.Errorf("the related task in another project was claimed by %s", got)
	}
}

func TestBatchWithReasonTakesRelatedAcross(t *testing.T) {
	e, me, _, _, homeTask, otherTask := crossEnv(t)
	ctx := context.Background()
	gitRepo(t, "git@github.com:robin/harbor.git")
	if err := e.svc.AddRelation(ctx, me, otherTask.Key, "relates", homeTask.Key); err != nil {
		t.Fatal(err)
	}
	const why = "one fix spans both"
	e.wp("batch", otherTask.Key, "--cross-project", why, "--no-brief")
	if claimedBy(t, e, me, homeTask.Key) == "" {
		t.Error("the related task wasn't claimed")
	}
	if !historyHas(t, e, me, otherTask.Key, why) {
		t.Error("the reason isn't on the task's history")
	}
}

// --repo also finds tasks of other projects that link to the repo; from
// inside the project those aren't what it hands out.
func TestRepoFlagStaysInsideTheProject(t *testing.T) {
	e, me, _, _, homeTask, otherTask := crossEnv(t)
	gitRepo(t, "git@github.com:robin/harbor.git")
	e.wp("edit", homeTask.Key, "--status", "Done")
	if _, err := e.svc.AddLink(context.Background(), me, otherTask.Key, "https://github.com/robin/harbor/pull/9", ""); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if out := e.wp("next", "--repo", "robin/harbor", "--no-brief"); strings.Contains(out, otherTask.Key) {
		t.Errorf("--repo reached another project's task:\n%s", out)
	}
	t.Chdir(t.TempDir())
	if out := e.wp("next", "--repo", "robin/harbor", "--no-brief"); !strings.Contains(out, otherTask.Key) {
		t.Errorf("the link should match with no project to stay in:\n%s", out)
	}
}
