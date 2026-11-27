package main

import (
	"context"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// wp new points at an open issue that is already the same work instead of
// filing it again, and lists the ones like it after filing (WP-104).
func TestCLINewSimilarIssues(t *testing.T) {
	e := newEnv(t)
	t.Chdir(t.TempDir())
	ctx := context.Background()
	robin, err := e.svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "j@example.com", Password: "correct horse battery"})
	if err != nil {
		t.Fatal(err)
	}
	me := &core.Actor{User: robin}
	name := "Gateway"
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := e.svc.CreateToken(ctx, me, core.TokenSpec{Name: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAYPOINT_TOKEN", tok)
	for _, title := range []string{"Billing invoice emails", "Dark mode colours", "Avatar upload sizes", "Calendar export feed", "Password reset tokens"} {
		e.wp("new", title, "-p", p.Key)
	}
	theirs := e.wp("new", "Rank models by published benchmarks for each purpose", "-p", p.Key,
		"-d", "Clients name a purpose; the gateway ranks its models for it by published benchmark scores.")
	key := p.Key + "-6"
	if !strings.Contains(theirs, "Created "+key) {
		t.Fatalf("new: %s", theirs)
	}

	out, code := e.run("new", "Rank models by published benchmarks for each purpose", "-p", p.Key,
		"-d", "The gateway should rank models by benchmark scores per purpose.")
	if code == 0 || strings.Contains(out, "Created") || !strings.Contains(out, "not filed: "+key+" [To Do]") ||
		!strings.Contains(out, "Take it with wp next "+key+", or file this anyway with --new") {
		t.Errorf("same work (%d):\n%s", code, out)
	}
	out = e.wp("new", "Rank models by published benchmarks for each purpose", "-p", p.Key, "--new")
	if !strings.Contains(out, "Created "+p.Key+"-7") || !strings.Contains(out, "Open issues like it:\n  "+key+" [To Do]") ||
		!strings.Contains(out, "wp relate "+p.Key+"-7 relates "+key) {
		t.Errorf("--new:\n%s", out)
	}
	if out = e.wp("new", "Retry the forge webhook on a timeout", "-p", p.Key); strings.Contains(out, "Open issues like it") {
		t.Errorf("unrelated:\n%s", out)
	}
	if out = e.wp("new", "Rank models by published benchmarks", "-p", p.Key, "--new", "--json"); strings.Contains(out, "Open issues") || !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("--json:\n%s", out)
	}
}
