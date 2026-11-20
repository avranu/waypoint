package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestParseTTL(t *testing.T) {
	for in, want := range map[string]int{"": 0, "90": 90, "90m": 5400, "12h": 43200, "3d": 259200, "2w": 1209600, "1.5h": 5400} {
		if got, err := parseTTL(in); err != nil || got != want {
			t.Errorf("parseTTL(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"soon", "-3d", "3x", "d"} {
		if _, err := parseTTL(in); err == nil {
			t.Errorf("parseTTL(%q) accepted", in)
		}
	}
}

// gitRepoWithCommit makes a checkout with one commit and goes into it.
func gitRepoWithCommit(t *testing.T) {
	t.Helper()
	gitRepo(t, "git@example.com:nobody/nothing.git")
	cmd := exec.Command("git", "-c", "user.name=T", "-c", "user.email=t@example.com", "commit", "--allow-empty", "-q", "-m", "first")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v %s", err, out)
	}
}

func TestCLIEnvironments(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	ctx := context.Background()
	name := "Harbor"
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: &name, Settings: &core.ProjectSettings{DevEnvURL: "https://{name}.dev.harbor.org"}})
	if err != nil {
		t.Fatal(err)
	}
	if out, code := e.run("env", "ls"); code == 0 {
		t.Errorf("env ls without a project should say which: %s", out)
	}
	out := e.wp("env", "ls", "-p", p.Key)
	if !strings.Contains(out, "No environments") {
		t.Errorf("empty ls: %s", out)
	}
	if out := strings.TrimSpace(e.wp("env", "ls", "-p", p.Key, "--json")); out != "[]" {
		t.Errorf("empty ls --json: %s", out)
	}

	// CI: started, then succeeded for the same commit.
	out = e.wp("env", "deploy", "staging", "-p", p.Key, "--commit", "3fa9c1e5b7d2", "--branch", "main", "--status", "started", "--url", "https://ci.example.com/runs/7")
	if !strings.Contains(out, "started: 3fa9c1e to "+p.Key+"/staging") {
		t.Errorf("deploy started: %s", out)
	}
	out = e.wp("env", "deploy", "staging", "-p", p.Key, "--commit", "3fa9c1e5b7d2", "--version", "1.4.0")
	if !strings.Contains(out, "succeeded: 3fa9c1e") {
		t.Errorf("deploy: %s", out)
	}
	if out, code := e.run("env", "deploy", "staging", "-p", p.Key, "--commit", "zzz"); code == 0 {
		t.Errorf("bad commit accepted: %s", out)
	}

	// A dev environment from the project's address pattern.
	out = e.wp("env", "up", "a23x42s", "-p", p.Key, "--branch", "fix/login", "--commit", "abc1234", "--ttl", "3d")
	if !strings.Contains(out, "a23x42s is up at https://a23x42s.dev.harbor.org") || !strings.Contains(out, "until") {
		t.Errorf("up: %s", out)
	}
	if out, code := e.run("env", "up", "x", "-p", p.Key, "--ttl", "soon"); code == 0 {
		t.Errorf("bad ttl accepted: %s", out)
	}

	out = e.wp("env", "ls", "-p", p.Key)
	for _, want := range []string{"staging", "main@3fa9c1e v1.4.0", "a23x42s", "fix/login@abc1234", "https://a23x42s.dev.harbor.org", "STATUS"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls missing %q:\n%s", want, out)
		}
	}
	out = e.wp("env", "show", "staging", "-p", p.Key)
	for _, want := range []string{p.Key + "/staging", "Runs:    main@3fa9c1e v1.4.0", "Deployments:", "succeeded", "anthropic/claude-opus-5-5"} {
		if !strings.Contains(out, want) {
			t.Errorf("show missing %q:\n%s", want, out)
		}
	}
	var js []map[string]any
	if err := json.Unmarshal([]byte(e.wp("env", "ls", "-p", p.Key, "--json")), &js); err != nil || len(js) != 2 {
		t.Errorf("json: %v %d", err, len(js))
	}

	out = e.wp("env", "down", "a23x42s", "-p", p.Key)
	if !strings.Contains(out, "stopped") {
		t.Errorf("down: %s", out)
	}
	if out := e.wp("env", "ls", "-p", p.Key); strings.Contains(out, "a23x42s") {
		t.Errorf("a stopped environment is listed:\n%s", out)
	}
	if out := e.wp("env", "ls", "-p", p.Key, "--stopped"); !strings.Contains(out, "a23x42s") || !strings.Contains(out, "stopped") {
		t.Errorf("--stopped:\n%s", out)
	}
	// Every project.
	if out := e.wp("env", "ls", "-A"); !strings.Contains(out, "PROJECT") || !strings.Contains(out, p.Key) {
		t.Errorf("ls -A:\n%s", out)
	}

	// In a checkout, deploy defaults to the commit checked out here.
	gitRepoWithCommit(t)
	out = e.wp("env", "deploy", "preview-1", "-p", p.Key)
	if !strings.Contains(out, "succeeded:") {
		t.Errorf("deploy from a checkout: %s", out)
	}
}
