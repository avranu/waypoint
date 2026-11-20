package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestCLIAttach(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	t.Chdir(t.TempDir())
	ctx := context.Background()
	p, _ := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Shots")})
	issue, _ := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "Broken layout", Description: "It overflows."})
	pg, err := e.svc.CreatePage(ctx, me, core.PageInput{Title: ptr("Layout plan"), Category: ptr("plan"), Project: core.Some(p.Key), Body: ptr("Plan text.")})
	if err != nil {
		t.Fatal(err)
	}
	note, err := e.svc.CreateDesign(ctx, me, core.DesignInput{Title: ptr("Board"), Kind: ptr("screen"), Body: ptr("The board."), Project: &p.Key})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	shot := write("shot.png", "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")
	logf := write("run.log", "started\nfinished\n")
	zip := write("out.zip", "PK\x03\x04data")

	// Plain: prints the Markdown for each file, in order.
	out := e.wp("attach", issue.Key, shot, zip)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "![shot.png](/api/v1/attachments/") || !strings.HasPrefix(lines[1], "[out.zip](/api/v1/attachments/") {
		t.Errorf("attach output:\n%s", out)
	}
	if d, _ := e.svc.GetIssue(ctx, me, issue.Key); strings.Contains(d.Description, "attachments") || len(d.Files) != 2 {
		t.Errorf("without --embed the description changed or files are missing: %q %d", d.Description, len(d.Files))
	}
	// --embed on an issue appends to the description, caption as the label; agent-written.
	out = e.wp("attach", issue.Key, shot, "--caption", "Before the fix", "--embed")
	if !strings.Contains(out, "![Before the fix](/api/v1/attachments/") {
		t.Errorf("caption: %s", out)
	}
	d, _ := e.svc.GetIssue(ctx, me, issue.Key)
	if !strings.HasPrefix(d.Description, "It overflows.\n\n![Before the fix](/api/v1/attachments/") {
		t.Errorf("description = %q", d.Description)
	}
	// --embed --comment posts a comment instead and leaves the description.
	e.wp("attach", issue.Key, logf, "--embed", "--comment")
	cs, _ := e.svc.ListComments(ctx, me, issue.Key)
	if len(cs) != 1 || !strings.HasPrefix(cs[0].Body, "[run.log](/api/v1/attachments/") {
		t.Errorf("comments = %+v", cs)
	}
	if d2, _ := e.svc.GetIssue(ctx, me, issue.Key); d2.Description != d.Description {
		t.Error("--comment changed the description")
	}
	// A page by code or title; the body grows.
	e.wp("attach", "page:"+pg.Code, shot, "--embed")
	e.wp("attach", "page:Layout plan", logf)
	got, _ := e.svc.GetPage(ctx, me, pg.ID)
	if !strings.HasPrefix(got.Body, "Plan text.\n\n![shot.png](/api/v1/attachments/") {
		t.Errorf("page body = %q", got.Body)
	}
	if files, _ := e.svc.ListAttachments(ctx, me, core.AttachTarget{Type: core.TargetPage, ID: pg.ID}); len(files) != 2 {
		t.Errorf("page files = %d", len(files))
	}
	// A design note: an agent's embed is an addition waiting for a person.
	out = e.wp("attach", "design:"+strconv.FormatInt(note.ID, 10), shot, "--embed")
	if !strings.Contains(out, "![shot.png]") {
		t.Errorf("design: %s", out)
	}
	n, _ := e.svc.GetDesign(ctx, me, note.ID)
	if n.Body != "The board." || len(n.Additions) == 0 {
		t.Errorf("design note body %q additions %d", n.Body, len(n.Additions))
	}
	// design:#12 works for the embed as it does for the upload.
	out = e.wp("attach", "design:#"+strconv.FormatInt(note.ID, 10), shot, "--embed")
	if !strings.Contains(out, "Added to design #"+strconv.FormatInt(note.ID, 10)) {
		t.Errorf("design:#N: %s", out)
	}
	// stdin needs a name; JSON carries the markdown.
	if out, code := e.run("attach", issue.Key, "-"); code == 0 || !strings.Contains(out, "--name") {
		t.Errorf("stdin without a name: %d %s", code, out)
	}
	out = e.wp("attach", issue.Key, shot, "--json")
	var js struct {
		Target      string
		Attachments []attached
	}
	if err := json.Unmarshal([]byte(out), &js); err != nil || js.Target != "issue:"+issue.Key || len(js.Attachments) != 1 || js.Attachments[0].Kind != "image" {
		t.Errorf("--json: %v\n%s", err, out)
	}
	// Mistakes fail before anything is uploaded.
	before, _ := e.svc.ListAttachments(ctx, me, core.AttachTarget{Type: core.TargetIssue, ID: issue.ID})
	for _, args := range [][]string{
		{"attach", issue.Key, shot, filepath.Join(dir, "missing.png")},
		{"attach", issue.Key},
		{"attach", "comment:1", shot, "--embed"},
		{"attach", issue.Key, shot, "--comment"},
		{"attach", "page:Nope", shot},
		{"attach", "session", shot},
		{"attach", "request:R-1", shot},
	} {
		if out, code := e.run(args...); code == 0 {
			t.Errorf("wp %s succeeded: %s", strings.Join(args, " "), out)
		}
	}
	if after, _ := e.svc.ListAttachments(ctx, me, core.AttachTarget{Type: core.TargetIssue, ID: issue.ID}); len(after) != len(before) {
		t.Errorf("a failed attach uploaded %d files", len(after)-len(before))
	}
	// session reads WAYPOINT_SESSION, the CLI's own session id. Until a hook
	// has registered it the server doesn't know the session.
	t.Setenv("WAYPOINT_SESSION", "abc")
	t.Setenv("WAYPOINT_MACHINE", "box")
	if out, code := e.run("attach", "session", shot); code == 0 || !strings.Contains(out, "session") {
		t.Errorf("unregistered session: %d %s", code, out)
	}
	agent := &core.Actor{User: me.User, Agent: "anthropic/claude-opus-5-5"}
	res, err := e.svc.SessionHook(ctx, agent, []core.HookEvent{{Event: "SessionStart", SessionID: "abc", Machine: "box", Cwd: "/work/app"}})
	if err != nil {
		t.Fatal(err)
	}
	out = e.wp("attach", "session", shot, "--caption", "What it looks like")
	if !strings.HasPrefix(out, "![What it looks like](/api/v1/attachments/") {
		t.Errorf("attach session: %s", out)
	}
	if got, _ := e.svc.ListAttachments(ctx, me, core.AttachTarget{Type: core.TargetSession, ID: res.Session}); len(got) != 1 || got[0].Caption != "What it looks like" || got[0].Agent == "" {
		t.Errorf("session files: %+v", got)
	}
	// The session by its number works from anywhere, too.
	t.Setenv("WAYPOINT_SESSION", "")
	e.wp("attach", "session:"+strconv.FormatInt(res.Session, 10), logf)
	if got, _ := e.svc.ListAttachments(ctx, me, core.AttachTarget{Type: core.TargetSession, ID: res.Session}); len(got) != 2 {
		t.Errorf("session files by number: %+v", got)
	}
	// A request, by its code.
	sess := *agent
	sess.Session = res.Session
	req, _, err := e.svc.CreateRequest(ctx, &sess, core.RequestInput{Kind: core.RequestQuestion, Title: "Which?", Questions: []core.Question{{Question: "A or B?"}}})
	if err != nil {
		t.Fatal(err)
	}
	e.wp("attach", "request:"+req.Ref, logf)
	if got, _ := e.svc.ListAttachments(ctx, me, core.AttachTarget{Type: core.TargetRequest, ID: req.ID}); len(got) != 1 || got[0].Name != "run.log" {
		t.Errorf("request files: %+v", got)
	}
}
