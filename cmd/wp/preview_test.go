package main

import (
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// A /PATH is a page of this checkout's preview (WP-1226): taken there,
// signed in with the preview's own token, captioned as the branch's build
// and recorded by its path.
func TestCLIShotPreviewPath(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Chdir(t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	ctx := context.Background()
	p, _ := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Preview")})
	issue, _ := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "New settings"})
	builds := 0
	startPreview = func(*cli) (*previewState, error) {
		builds++
		return &previewState{URL: "http://127.0.0.1:41234", Port: 41234, Token: "wpt_preview"}, nil
	}
	t.Cleanup(func() { startPreview = (*cli).ensurePreview })
	var got shotJob
	shootPages = func(c *cli, job shotJob) ([]shotResult, error) {
		got = job
		var out []shotResult
		for i, tg := range job.Targets {
			f := filepath.Join(job.Dir, string(rune('a'+i))+".png")
			if err := os.WriteFile(f, []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89"), 0o600); err != nil {
				return nil, err
			}
			out = append(out, shotResult{URL: tg.URL, Size: "desktop", File: f, Status: 200})
		}
		return out, nil
	}
	t.Cleanup(func() { shootPages = runShotBrowser })

	out := e.wp("shot", issue.Key, "/admin?tab=ai", "/settings", "https://example.com/x", "--size", "desktop")
	if builds != 1 {
		t.Errorf("the preview was started %d times for two paths", builds)
	}
	if len(got.Targets) != 3 || got.Targets[0].URL != "http://127.0.0.1:41234/admin?tab=ai" || !got.Targets[0].Auth ||
		got.Targets[1].URL != "http://127.0.0.1:41234/settings" || got.Targets[2].Auth {
		t.Errorf("targets: %+v", got.Targets)
	}
	if got.Tokens["http://127.0.0.1:41234"] != "wpt_preview" || len(got.Tokens) != 2 {
		t.Errorf("tokens: %v", got.Tokens)
	}
	if !strings.Contains(out, "Try it: /admin?tab=ai") {
		t.Errorf("out:\n%s", out)
	}
	list, _ := e.svc.ListAttachments(ctx, me, core.AttachTarget{Type: core.TargetIssue, ID: issue.ID})
	n := 0
	for _, a := range list {
		if strings.HasSuffix(a.Caption, " · this branch's build") {
			n++
		}
	}
	if n != 2 || len(list) != 3 {
		t.Errorf("%d of %d captions say they show the branch's build", n, len(list))
	}

	// A preview that can't start says why, and nothing is taken.
	startPreview = func(*cli) (*previewState, error) { return nil, errors.New("not a Waypoint checkout") }
	got = shotJob{}
	if out, code := e.run("shot", issue.Key, "/admin"); code == 0 || !strings.Contains(out, "not a Waypoint checkout") || got.Targets != nil {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

func TestWebOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:41234/x":     "http://127.0.0.1:41234",
		"https://WP.Example.com:443/a": "https://wp.example.com",
		"http://h:80":                  "http://h",
		"https://h:8443":               "https://h:8443",
		"http://[::1]:9/":              "http://[::1]:9",
	} {
		u, _ := url.Parse(in)
		if got := webOrigin(u); got != want {
			t.Errorf("webOrigin(%s) = %s, want %s", in, got, want)
		}
	}
}

// The preview signs up through the server's first-run invite and gets an
// admin's token and the task's project.
func TestPreviewSignUp(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	link, err := e.svc.BootstrapInvite(ctx)
	if err != nil || link == nil {
		t.Fatal(link, err)
	}
	log := filepath.Join(t.TempDir(), "server.log")
	if err := os.WriteFile(log, []byte("starting\n\n  Create your admin account: "+link.URL+"\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	invite, err := previewInvite(log)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := previewSignUp(e.url, invite, "PV")
	if err != nil || tok == "" {
		t.Fatal(tok, err)
	}
	actor, err := e.svc.TokenActor(ctx, tok)
	if err != nil || actor.User == nil || !actor.User.IsAdmin {
		t.Fatalf("token: %+v %v", actor, err)
	}
	if p, err := e.svc.GetProject(ctx, actor, "PV"); err != nil || p.Name != "PV (preview)" {
		t.Errorf("project: %+v %v", p, err)
	}
	if _, err := previewInvite(filepath.Join(t.TempDir(), "none.log")); err == nil {
		t.Error("a missing log gave an invite")
	}
}

// Outside a Waypoint checkout there is no preview, and it says what to do.
func TestPreviewNeedsWaypointCheckout(t *testing.T) {
	newEnv(t)
	t.Chdir(t.TempDir())
	if _, err := waypointCheckout(); err == nil || !strings.Contains(err.Error(), "give wp shot its URL") {
		t.Errorf("err: %v", err)
	}
}

// A pid is the preview's only when its command line is the keeper's or the
// server's for that directory; any other process is left alone.
func TestPreviewProcOnlyOwn(t *testing.T) {
	if previewProc(os.Getpid(), t.TempDir(), nil) {
		t.Error("this test process taken for a preview's")
	}
	if previewProc(os.Getpid(), filepath.Dir(os.Args[0]), nil) {
		t.Error("a process that merely has the directory in its command line taken for a preview's")
	}
}

// The keeper deletes only a preview's own directory.
func TestPreviewServeRefusesOtherDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"top":"/nonexistent"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdPreviewServe(&cli{out: io.Discard, err: io.Discard}, []string{dir}); err == nil {
		t.Error("served a directory that is not the preview's")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Error("deleted it:", err)
	}
}
