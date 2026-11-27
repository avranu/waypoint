package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// wp goals, wp goal and wp trace, down to a line of code (WP-382).
func TestCLIGoals(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	agentTok := os.Getenv("WAYPOINT_TOKEN")
	ctx := context.Background()
	humanTok, _, err := e.svc.CreateAPIToken(ctx, me, "robin", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	asHuman := func() { t.Setenv("WAYPOINT_TOKEN", humanTok) }
	asAgent := func() { t.Setenv("WAYPOINT_TOKEN", agentTok) }
	p, _ := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Atlas")})
	f, _ := e.svc.CreateForge(ctx, me, core.ForgeInput{Kind: ptr("github")})
	if _, err := e.svc.AddProjectRepo(ctx, me, p.Key, f.ID, "robin/atlas", false); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	gitRepo(t, "git@github.com:robin/atlas.git")

	asHuman()
	if out := e.wp("goals"); !strings.Contains(out, "No goals yet") {
		t.Errorf("empty goals:\n%s", out)
	}
	if out := e.wp("goal", "add", "Travellers find a route in one tap"); !strings.Contains(out, "Created design #1") {
		t.Errorf("goal add:\n%s", out)
	}
	asAgent()
	var task struct{ Key string }
	if err := json.Unmarshal([]byte(e.wp("new", "Draw the route list", "--json")), &task); err != nil || task.Key == "" {
		t.Fatalf("new: %v %+v", err, task)
	}
	key := task.Key
	if out := e.wp("goal", "link", key, "1"); !strings.Contains(out, "Linked (serves)") {
		t.Errorf("goal link:\n%s", out)
	}
	out := e.wp("goals", "gaps")
	if !strings.Contains(out, "by agent anthropic/claude-opus-5-5") {
		t.Fatalf("gaps:\n%s", out)
	}
	if out, code := e.run("goal", "confirm", "1"); code == 0 {
		t.Errorf("an agent confirmed a link:\n%s", out)
	}
	asHuman()
	if out := e.wp("goal", "confirm", "1"); !strings.Contains(out, "counts as yours") {
		t.Errorf("confirm:\n%s", out)
	}
	if out := e.wp("goals"); !strings.Contains(out, "1 by people") {
		t.Errorf("goals after confirm:\n%s", out)
	}

	if err := os.WriteFile("routes.go", []byte("package atlas\n\nfunc Routes() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "routes.go"}, {"-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "-q", "-m", "Draw the route list (" + key + ")"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	out = e.wp("trace", "routes.go:3")
	if testing.Verbose() {
		t.Log("\n" + out + "\n" + e.wp("goals") + "\n" + e.wp("goals", "gaps"))
	}
	for _, want := range []string{"routes.go:3  func Routes() {}", "last changed in", key + " Draw the route list", "Travellers find a route in one tap", "by robin"} {
		if !strings.Contains(out, want) {
			t.Errorf("trace FILE:LINE lacks %q:\n%s", want, out)
		}
	}
	if out := e.wp("trace", key); !strings.Contains(out, "route to goal") {
		t.Errorf("trace key:\n%s", out)
	}
	if out, code := e.run("trace", "routes.go:99"); code == 0 {
		t.Errorf("a line past the end traced:\n%s", out)
	}
}
