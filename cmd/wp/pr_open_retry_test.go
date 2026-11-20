package main

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// quietForge doesn't answer the first quiet attempts to open a pull request.
type quietForge struct {
	openForge
	quiet int
}

func (f *quietForge) CreatePR(ctx context.Context, fg *core.Forge, repo string, in core.NewPR) (*core.ForgePR, error) {
	f.mu.Lock()
	if f.quiet > 0 {
		f.quiet--
		f.mu.Unlock()
		return nil, &url.Error{Op: "Post", URL: fg.BaseURL + "/api/v1/repos/" + repo + "/pulls", Err: context.DeadlineExceeded}
	}
	f.mu.Unlock()
	return f.openForge.CreatePR(ctx, fg, repo, in)
}

func (f *quietForge) ListPRs(context.Context, *core.Forge, string) ([]*core.ForgePR, error) {
	return nil, nil
}

// wp pr open asks again when the forge didn't answer, and says so; when it
// never answers, the error names the forge (WP-265).
func TestCLIPROpenForgeDown(t *testing.T) {
	e := newEnv(t)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	var slept []time.Duration
	prOpenSleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { prOpenSleep = time.Sleep })
	me := e.login()
	ctx := context.Background()
	fg := &quietForge{quiet: 1}
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
	bare := filepath.Join(t.TempDir(), "origin.git")
	sh(t, "", "init", "-q", "--bare", bare)
	dir := gitRepo(t, "git@git.example.com:robin/waypoint.git")
	sh(t, dir, "config", "url."+bare+".pushInsteadOf", "git@git.example.com:robin/waypoint.git")
	sh(t, dir, "checkout", "-q", "-b", "main")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "first")
	sh(t, dir, "push", "-q", "origin", "main")
	sh(t, dir, "checkout", "-q", "-b", "fix-redirect")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "Fix the redirect loop")

	out, code := e.run("pr", "open")
	if code != 0 || !strings.Contains(out, "git.example.com didn't answer") || !strings.Contains(out, "trying again in 5s") ||
		!strings.Contains(out, "Opened robin/waypoint#41") || len(slept) != 1 {
		t.Fatalf("one quiet attempt: %d slept %v\n%s", code, slept, out)
	}

	sh(t, dir, "checkout", "-q", "-b", "fix-other", "main")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "Fix the other thing")
	fg.mu.Lock()
	fg.quiet, slept = 5, nil
	fg.mu.Unlock()
	out, code = e.run("pr", "open")
	if code == 0 || len(slept) != 2 || !strings.Contains(out, "wp: open the pull request: git.example.com didn't answer within") ||
		strings.Contains(out, "internal error") {
		t.Fatalf("never answers: %d slept %v\n%s", code, slept, out)
	}
}

// A bare 500 names the request, not just "internal error".
func TestAPIErrorBare500(t *testing.T) {
	e := &apiError{status: http.StatusInternalServerError, msg: "internal error", method: "POST", path: "/api/v1/projects/INF/pulls"}
	if got := e.Error(); got != "the server failed with HTTP 500 on POST /api/v1/projects/INF/pulls; its log has the cause" {
		t.Errorf("got %q", got)
	}
	e = &apiError{status: http.StatusBadGateway, msg: "git.example.com didn't answer within 30 s; try again"}
	if got := e.Error(); got != e.msg || !e.transient() {
		t.Errorf("502: %q transient=%v", got, e.transient())
	}
}
