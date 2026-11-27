package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// wp files fetches what is attached to an issue (embedded or not, in the
// description or in a comment) with the CLI's own login, and wp show points
// at it (WP-92).
func TestCLIFiles(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Chdir(t.TempDir())
	ctx := context.Background()
	p, _ := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Shots")})
	issue, _ := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "Broken layout", Description: "It overflows."})
	other, _ := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "Elsewhere"})
	pg, err := e.svc.CreatePage(ctx, me, core.PageInput{Title: ptr("Layout plan"), Category: ptr("plan"), Project: core.Some(p.Key), Body: ptr("Plan.")})
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89"
	write := func(name, body string) string {
		path := filepath.Join(src, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	shot := write("image.png", png)
	logf := write("run.log", "started\nfinished\n")
	zip := write("out.zip", "PK\x03\x04data")

	if out := e.wp("files", issue.Key); !strings.Contains(out, "No attachments on "+issue.Key) {
		t.Errorf("empty: %s", out)
	}
	if out := e.wp("show", issue.Key); strings.Contains(out, "Attachments:") {
		t.Errorf("a hint with nothing attached: %s", out)
	}

	e.wp("attach", issue.Key, shot, "--caption", "Before the fix", "--embed")
	e.wp("attach", issue.Key, zip)
	e.wp("attach", issue.Key, shot, "--embed", "--comment")
	e.wp("attach", other.Key, logf)
	of, _ := e.svc.ListAttachments(ctx, me, core.AttachTarget{Type: core.TargetIssue, ID: other.ID})
	if _, err := e.svc.AddComment(ctx, me, issue.Key, "See ["+of[0].Name+"]("+of[0].URL+")"); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "out")
	out := e.wp("files", issue.Key, "--dir", dir)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("files output:\n%s", out)
	}
	wantBody := map[string]string{"image.png": png, "out.zip": "PK\x03\x04data", "run.log": "started\nfinished\n"}
	for _, l := range lines {
		path, _, _ := strings.Cut(l, "  ")
		if !filepath.IsAbs(path) || filepath.Dir(path) != dir {
			t.Errorf("path %q not in %s", path, dir)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if want := wantBody[strings.SplitN(filepath.Base(path), "-", 2)[1]]; string(got) != want {
			t.Errorf("%s holds %q, want %q", path, got, want)
		}
	}
	if !strings.Contains(lines[0], "Before the fix (image.png) - description") {
		t.Errorf("first line: %q", lines[0])
	}
	if !strings.Contains(out, "out.zip") || !strings.Contains(out, "run.log") || !strings.Contains(out, "comment ") {
		t.Errorf("missing a file or its source:\n%s", out)
	}
	if tok := os.Getenv("WAYPOINT_TOKEN"); tok != "" && strings.Contains(out, tok) {
		t.Error("the token was printed")
	}

	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	out = e.wp("files", strings.ToLower(issue.Key))
	if !strings.HasPrefix(out, filepath.Join(tmp, "wp-files", issue.Key)) {
		t.Errorf("default dir: %s", out)
	}

	var js []fileInfo
	if err := json.Unmarshal([]byte(e.wp("files", issue.Key, "--dir", dir, "--json")), &js); err != nil || len(js) != 4 {
		t.Fatalf("--json: %v %v", err, js)
	}
	for _, f := range js {
		if !strings.HasPrefix(f.URL, e.url+"/api/v1/attachments/") || f.Path == "" {
			t.Errorf("json entry %+v", f)
		}
	}

	show := e.wp("show", issue.Key)
	if !strings.Contains(show, "Attachments: 3 — wp files "+issue.Key+" saves them to view") {
		t.Errorf("show hint:\n%s", show)
	}
	if strings.Contains(show, "](/api/v1/attachments/") || !strings.Contains(show, "]("+e.url+"/api/v1/attachments/") {
		t.Errorf("show links:\n%s", show)
	}

	e.wp("attach", "page:"+pg.Code, logf)
	if out := e.wp("files", "page:"+pg.Code, "--dir", dir); !strings.Contains(out, "run.log") {
		t.Errorf("page files: %s", out)
	}

	for _, args := range [][]string{
		{"files", "NOPE-1"},
		{"files", issue.Key, "extra"},
		{"files"},
	} {
		if out, code := e.run(args...); code == 0 {
			t.Errorf("wp %s succeeded: %s", strings.Join(args, " "), out)
		}
	}
}
