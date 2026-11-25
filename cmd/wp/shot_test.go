package main

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// wp shot attaches each picture as a review screenshot and records the
// page (a local one by its path) as where to try the change; the browser
// is stubbed.
func TestCLIShot(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Chdir(t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	ctx := context.Background()
	p, _ := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Shots")})
	issue, _ := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "Live page"})
	var got shotJob
	shootPages = func(c *cli, job shotJob) ([]shotResult, error) {
		got = job
		var out []shotResult
		for _, s := range job.Sizes {
			for i, tg := range job.Targets {
				if strings.Contains(tg.URL, "broken") {
					out = append(out, shotResult{URL: tg.URL, Size: s.Name, Error: "net::ERR_CONNECTION_REFUSED"})
					continue
				}
				f := filepath.Join(job.Dir, s.Name+string(rune('a'+i))+".png")
				if err := os.WriteFile(f, []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89"), 0o600); err != nil {
					return nil, err
				}
				out = append(out, shotResult{URL: tg.URL, Size: s.Name, File: f, Status: 200})
			}
		}
		return out, nil
	}
	t.Cleanup(func() { shootPages = runShotBrowser })

	out := e.wp("shot", issue.Key, "http://localhost:5173/live?x=1", "https://example.com/a", "http://localhost:9/broken", "--caption", "The new page")
	if len(got.Sizes) != 2 || len(got.Targets) != 3 || got.Targets[0].Auth || !strings.Contains(filepath.ToSlash(got.Dir), "/wp-shots/") {
		t.Errorf("job: %+v", got)
	}
	if !strings.Contains(out, "Try it: /live?x=1") || !strings.Contains(out, "Try it: https://example.com/a") || strings.Contains(out, "Try it: http://localhost:9") || !strings.Contains(out, "ERR_CONNECTION_REFUSED") {
		t.Errorf("out:\n%s", out)
	}
	list, err := e.svc.ListAttachments(ctx, me, core.AttachTarget{Type: core.TargetIssue, ID: issue.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("attachments: %d", len(list))
	}
	for _, a := range list {
		if !a.Review || !strings.HasPrefix(a.Caption, "The new page (") {
			t.Errorf("attachment: review %v caption %q", a.Review, a.Caption)
		}
	}
	c, err := e.svc.CatchUp(ctx, me, p.Key)
	if err != nil || c.Total != 0 {
		t.Errorf("an open task isn't in Catch up: %+v %v", c, err)
	}

	got = shotJob{}
	e.wp("done", issue.Key, "-m", "Added it", "--shot", "https://example.com/b,c", "--size", "phone")
	if len(got.Targets) != 1 || got.Targets[0].URL != "https://example.com/b,c" || len(got.Sizes) != 1 || got.Sizes[0].Name != "phone" {
		t.Errorf("done --shot job: %+v", got)
	}
	if _, code := e.run("done", issue.Key, "https://example.com/x"); code == 0 {
		t.Error("a bare URL after done was taken")
	}
}

func TestShotTryLink(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:5173/live?x=1#top": "/live?x=1#top",
		"http://127.0.0.1:8080":              "/",
		"http://192.168.1.4/a":               "/a",
		"http://dev.localhost/a":             "/a",
		"https://example.com/a?b=1":          "https://example.com/a?b=1",
		"https://waypoint.example/catch-up":  "https://waypoint.example/catch-up",
	} {
		if got := tryLinkFor(in); got != want {
			t.Errorf("tryLinkFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSameOriginDefaultPorts(t *testing.T) {
	cases := []struct {
		page, server string
		want         bool
	}{
		{"https://h/p", "https://h:443", true},
		{"https://h:443/p", "https://h", true},
		{"http://h/p", "http://h:80", true},
		{"https://h:8443/p", "https://h", false},
		{"http://h/p", "https://h", false},
		{"https://other/p", "https://h", false},
	}
	for _, c := range cases {
		u, _ := url.Parse(c.page)
		if got := sameOrigin(u, c.server); got != c.want {
			t.Errorf("sameOrigin(%s, %s) = %v, want %v", c.page, c.server, got, c.want)
		}
	}
}

// A picture that can't be attached is reported and skipped: failing wp done
// once others were on the task had it run again and attach them twice.
func TestShotSkipsAPictureItCannotAttach(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Chdir(t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	ctx := context.Background()
	p, _ := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Shots")})
	issue, _ := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "Half a picture"})
	good := true
	shootPages = func(c *cli, job shotJob) ([]shotResult, error) {
		var out []shotResult
		for _, s := range job.Sizes {
			f := filepath.Join(job.Dir, s.Name+".png")
			if s.Name == "desktop" && good {
				if err := os.WriteFile(f, []byte("\x89PNG\r\n\x1a\n"), 0o600); err != nil {
					return nil, err
				}
			}
			out = append(out, shotResult{URL: job.Targets[0].URL, Size: s.Name, File: f, Status: 200})
		}
		return out, nil
	}
	t.Cleanup(func() { shootPages = runShotBrowser })

	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	shootPages = func(c *cli, job shotJob) ([]shotResult, error) {
		return []shotResult{{URL: job.Targets[0].URL, Size: "desktop", File: secret, Status: 200}}, nil
	}
	if out, code := e.run("shot", issue.Key, "https://example.com/a"); code == 0 || !strings.Contains(out, "outside") {
		t.Errorf("a file outside the pictures was attached (%d): %s", code, out)
	}
	if d, _ := e.svc.GetIssue(ctx, me, issue.Key); len(d.Files) != 0 {
		t.Fatalf("attached %d files from outside", len(d.Files))
	}
	shootPages = func(c *cli, job shotJob) ([]shotResult, error) {
		var out []shotResult
		for _, s := range job.Sizes {
			f := filepath.Join(job.Dir, s.Name+".png")
			if s.Name == "desktop" && good {
				if err := os.WriteFile(f, []byte("\x89PNG\r\n\x1a\n"), 0o600); err != nil {
					return nil, err
				}
			}
			out = append(out, shotResult{URL: job.Targets[0].URL, Size: s.Name, File: f, Status: 200})
		}
		return out, nil
	}

	good = false
	if out, code := e.run("done", issue.Key, "-m", "Done", "--shot", "https://example.com/a"); code == 0 || !strings.Contains(out, "still open") {
		t.Errorf("no picture attached, done went on (%d): %s", code, out)
	}
	if d, _ := e.svc.GetIssue(ctx, me, issue.Key); d.Status.Category == "done" || len(d.Files) != 0 {
		t.Errorf("after a failed shot: %s, %d files", d.Status.Category, len(d.Files))
	}

	good = true
	out := e.wp("done", issue.Key, "-m", "Done", "--shot", "https://example.com/a")
	if !strings.Contains(out, "couldn't attach") {
		t.Errorf("the skipped picture isn't reported:\n%s", out)
	}
	if d, _ := e.svc.GetIssue(ctx, me, issue.Key); d.Status.Category != "done" || len(d.Files) != 1 {
		t.Errorf("after one of two attached: %s, %d files", d.Status.Category, len(d.Files))
	}
}
