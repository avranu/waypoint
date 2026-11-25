package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// wp ship A B opens one pull request linked to both, both In Review; a key
// that can't go along is refused before anything is committed; a later
// wp ship C onto the open pull request leaves C In Review, not Done (WP-1426).
func TestShipSeveralKeysShipsOnePullRequest(t *testing.T) {
	e := newEnv(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "T")
		t.Setenv(k+"_EMAIL", "t@example.com")
	}
	me := e.login()
	ctx := context.Background()
	fg := &openForge{}
	e.svc.Forges = fg
	kind, base, token := "forgejo", "https://git.example.com", "tok"
	f, err := e.svc.CreateForge(ctx, me, core.ForgeInput{Kind: &kind, BaseURL: &base, Token: &token})
	if err != nil {
		t.Fatal(err)
	}
	forgePeople(t, e, me, f)
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Waypoint")})
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Elsewhere")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AddProjectRepo(ctx, me, p.Key, f.ID, "robin/waypoint", false); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	issue := func(project, title string) *core.IssueDetail {
		t.Helper()
		i, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: project, Title: title})
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	main, also, closed, away := issue(p.Key, "Main"), issue(p.Key, "Also"), issue(p.Key, "Closed"), issue(other.Key, "Away")
	if _, err := e.svc.UpdateIssue(ctx, me, closed.Key, core.IssuePatch{Status: ptr("Done")}); err != nil {
		t.Fatal(err)
	}

	bare := filepath.Join(t.TempDir(), "origin.git")
	sh(t, "", "init", "-q", "--bare", bare)
	dir := gitRepo(t, "git@git.example.com:robin/waypoint.git")
	sh(t, dir, "config", "url."+bare+".pushInsteadOf", "git@git.example.com:robin/waypoint.git")
	sh(t, dir, "checkout", "-q", "-b", "main")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	sh(t, dir, "push", "-q", "origin", "main")
	sh(t, dir, "checkout", "-q", "-b", strings.ToLower(main.Key)+"-work")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, dir, "add", "a.txt")
	t.Setenv("WAYPOINT_AGENT", "none")

	var seven []string
	for i := 0; i < 6; i++ {
		seven = append(seven, issue(p.Key, "More").Key)
	}
	for _, tc := range []struct {
		with []string
		want string
	}{
		{[]string{closed.Key}, "is closed"},
		{[]string{away.Key}, "isn't in " + p.Key},
		{[]string{"P-999"}, "P-999"},
		{seven, "at most 6 tasks"},
	} {
		args := append([]string{"ship", main.Key}, tc.with...)
		out, code := e.run(append(args, "-p", p.Key, "-m", "Did it")...)
		if code != 2 || !strings.Contains(out, tc.want) || !strings.Contains(out, "wp ") {
			t.Errorf("ship with %v: %d\n%s", tc.with, code, out)
		}
	}
	if len(fg.created) != 0 {
		t.Fatalf("a refused ship opened %d pull requests", len(fg.created))
	}

	out := e.wp("ship", main.Key, also.Key, "-p", p.Key, "-m", "Did it")
	if len(fg.created) != 1 || !strings.Contains(out, "also "+also.Key+" In Review") {
		t.Fatalf("ship A B (%d created):\n%s", len(fg.created), out)
	}
	for _, k := range []string{main.Key, also.Key} {
		d, err := e.svc.GetIssue(ctx, me, k)
		if err != nil || d.Status.Name != "In Review" {
			t.Errorf("%s not In Review: %v %+v", k, err, d)
		}
		if shown := e.wp("show", k); !strings.Contains(shown, "pulls/41") {
			t.Errorf("%s isn't linked to the pull request:\n%s", k, shown)
		}
	}
	if shown := e.wp("show", also.Key); !strings.Contains(shown, "Did it") {
		t.Errorf("the ship message isn't on %s:\n%s", also.Key, shown)
	}

	// The same command again (more work on the branch) is a no-op for tasks
	// already linked and moved.
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, dir, "add", "b.txt")
	if out, code := e.run("ship", main.Key, also.Key, "-p", p.Key, "-m", "More"); code != 0 {
		t.Errorf("second ship A B: %d\n%s", code, out)
	}

	// A task shipped later onto the open pull request is In Review until the
	// merge, not Done at once.
	late := issue(p.Key, "Late")
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, dir, "add", "c.txt")
	if out, code := e.run("ship", late.Key, "-p", p.Key, "-m", "Late work"); code != 0 {
		t.Fatalf("ship %s onto the open pull request: %d\n%s", late.Key, code, out)
	}
	if len(fg.created) != 1 {
		t.Errorf("opened %d pull requests", len(fg.created))
	}
	d, err := e.svc.GetIssue(ctx, me, late.Key)
	if err != nil || d.Status.Name != "In Review" || len(d.Links) != 1 {
		t.Errorf("%s after shipping onto the open pull request: %v %+v links %+v", late.Key, err, d.Status, d.Links)
	}
}

// wp ship A B whose arm fails after the pull request opened: the error says
// so, and B is still linked and In Review, with nothing named as dropped.
func TestShipSeveralKeysArmFailureKeepsTheOthers(t *testing.T) {
	e := newEnvServing(t, nil, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/merge") {
				http.Error(w, `{"error":"forge is down"}`, http.StatusBadGateway)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "T")
		t.Setenv(k+"_EMAIL", "t@example.com")
	}
	me := e.login()
	ctx := context.Background()
	fg := &openForge{}
	e.svc.Forges = fg
	kind, base, token := "forgejo", "https://git.example.com", "tok"
	f, err := e.svc.CreateForge(ctx, me, core.ForgeInput{Kind: &kind, BaseURL: &base, Token: &token})
	if err != nil {
		t.Fatal(err)
	}
	forgePeople(t, e, me, f)
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Waypoint")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AddProjectRepo(ctx, me, p.Key, f.ID, "robin/waypoint", false); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	main, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "Main"})
	if err != nil {
		t.Fatal(err)
	}
	also, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "Also"})
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(t.TempDir(), "origin.git")
	sh(t, "", "init", "-q", "--bare", bare)
	dir := gitRepo(t, "git@git.example.com:robin/waypoint.git")
	sh(t, dir, "config", "url."+bare+".pushInsteadOf", "git@git.example.com:robin/waypoint.git")
	sh(t, dir, "checkout", "-q", "-b", "main")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	sh(t, dir, "push", "-q", "origin", "main")
	sh(t, dir, "checkout", "-q", "-b", strings.ToLower(main.Key)+"-work")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, dir, "add", "a.txt")
	t.Setenv("WAYPOINT_AGENT", "none")

	out, code := e.run("ship", main.Key, also.Key, "-p", p.Key, "-m", "Did it")
	if code == 0 || !strings.Contains(out, "couldn't arm") {
		t.Fatalf("an arm failure: %d\n%s", code, out)
	}
	if len(fg.created) != 1 {
		t.Fatalf("opened %d pull requests", len(fg.created))
	}
	for _, k := range []string{main.Key, also.Key} {
		d, err := e.svc.GetIssue(ctx, me, k)
		if err != nil {
			t.Fatalf("%s after the arm failed: %v\n%s", k, err, out)
		}
		if d.Status.Name != "In Review" || len(d.Links) != 1 {
			t.Errorf("%s after the arm failed: %+v links %+v\n%s", k, d.Status, d.Links, out)
		}
	}
	if strings.Contains(out, "isn't linked") {
		t.Errorf("the output names a task as dropped:\n%s", out)
	}
}
