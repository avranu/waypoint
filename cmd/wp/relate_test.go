package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// WP-1003: `wp relate B relates A`, with A done before B was filed, says
// it recorded a follow-up and why.
func TestRelateSaysFollowUp(t *testing.T) {
	e := newEnv(t)
	t.Chdir(t.TempDir())
	var mu sync.Mutex
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	e.svc.SetClock(func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	tick := func() { mu.Lock(); now = now.Add(time.Minute); mu.Unlock() }
	ctx := context.Background()
	robin, err := e.svc.CreateUser(ctx, core.SystemActor(), core.CreateUserInput{Username: "robin", Email: "j@example.com", Password: "correct horse battery"})
	if err != nil {
		t.Fatal(err)
	}
	me := &core.Actor{User: robin}
	name := "Export"
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := e.svc.CreateToken(ctx, me, core.TokenSpec{Name: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAYPOINT_PROJECT", p.Key)
	t.Setenv("WAYPOINT_TOKEN", tok)
	t.Setenv("WAYPOINT_AGENT", "none")
	first, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "Export to CSV"})
	if err != nil {
		t.Fatal(err)
	}
	tick()
	done := "Done"
	if _, err := e.svc.UpdateIssue(ctx, me, first.Key, core.IssuePatch{Status: &done}); err != nil {
		t.Fatal(err)
	}
	tick()
	later, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "CSV export drops quotes"})
	if err != nil {
		t.Fatal(err)
	}
	out := e.wp("relate", later.Key, "relates", first.Key)
	if !strings.Contains(out, later.Key+" follows "+first.Key) || !strings.Contains(out, first.Key+" was done before "+later.Key+" was filed") {
		t.Errorf("relate from the later task: %s", out)
	}
	other, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "CSV export: header row"})
	if err != nil {
		t.Fatal(err)
	}
	out = e.wp("relate", first.Key, "relates", other.Key)
	if !strings.Contains(out, first.Key+" followed by "+other.Key) || !strings.Contains(out, "wp relate "+other.Key+" follows "+first.Key) {
		t.Errorf("relate from the done task: %s", out)
	}
	if out = e.wp("relate", later.Key, "relates", other.Key); strings.TrimSpace(out) != later.Key+" relates "+other.Key {
		t.Errorf("neither done: %s", out)
	}
}
