package main

import (
	"context"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// Drafts (WP-42) stay out of an agent's shell even when the agent forgets
// --agent or the CLI is told it's a person, and only a person publishes.
func TestDraftsStayOutOfAgentShells(t *testing.T) {
	e := newEnv(t)
	t.Chdir(t.TempDir())
	ctx := context.Background()
	robin, err := e.svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "j@example.com", Password: "correct horse battery"})
	if err != nil {
		t.Fatal(err)
	}
	me := &core.Actor{User: robin}
	name := "Ideas"
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAYPOINT_PROJECT", p.Key)
	tok, _, err := e.svc.CreateToken(ctx, me, core.TokenSpec{Name: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAYPOINT_TOKEN", tok)
	t.Setenv("WAYPOINT_AGENT", "none")
	shell := func(agent bool) {
		for _, k := range []string{"CLAUDECODE", "ANTIGRAVITY_AGENT", "GEMINI_CLI", "OPENCODE", "CODEX_SANDBOX", "CODEX_SANDBOX_NETWORK_DISABLED"} {
			t.Setenv(k, "")
		}
		if agent {
			t.Setenv("CLAUDECODE", "1")
		}
	}

	shell(false)
	key := p.Key + "-1"
	if out := e.wp("new", "Half a thought", "-p", p.Key, "--draft"); !strings.Contains(out, "Created "+key) || !strings.Contains(out, "draft") {
		t.Errorf("new --draft: %s", out)
	}
	if out := e.wp("page", "new", "idea", "Loose idea", "-p", p.Key, "--draft"); !strings.Contains(out, "draft") {
		t.Errorf("page new --draft: %s", out)
	}
	if out := e.wp("ls", "is:draft"); !strings.Contains(out, key) || !strings.Contains(out, " draft") {
		t.Errorf("ls is:draft: %s", out)
	}

	shell(true)
	if out := e.wp("ls"); strings.Contains(out, "Half a thought") {
		t.Errorf("agent shell ls shows the draft: %s", out)
	}
	if out := e.wp("search", "thought loose"); strings.Contains(out, "Half a thought") || strings.Contains(out, "Loose idea") {
		t.Errorf("agent shell search shows drafts: %s", out)
	}
	if out, code := e.run("show", key); code == 0 || strings.Contains(out, "Half a thought") {
		t.Errorf("agent shell show: %d %s", code, out)
	}
	if out, code := e.run("publish", key); code == 0 || !strings.Contains(out, "for people") {
		t.Errorf("agent shell publish: %d %s", code, out)
	}

	shell(false)
	if out := e.wp("publish", key); !strings.Contains(out, "Published "+key) {
		t.Errorf("publish: %s", out)
	}
	shell(true)
	if out := e.wp("ls"); !strings.Contains(out, "Half a thought") || strings.Contains(out, " draft") {
		t.Errorf("published, agent shell ls: %s", out)
	}
}
