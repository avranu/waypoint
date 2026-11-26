package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Audit 2103: $$ before a brace, bash's ${ cmd; } substitution, a script with
// no #! line run by its path, and the option and printf branches no test held.
func TestGuardAudit2103(t *testing.T) {
	g0, repo := guardTestSite(t)
	writeFile(t, filepath.Join(repo, "tools", "bad.sh"), "#!/bin/sh\necho x > .claude/settings.json\n")
	writeFile(t, filepath.Join(repo, "deploy"), "sudo ls\n")
	writeFile(t, filepath.Join(repo, "notes"), "#!/usr/bin/awk -f\nsudo ls\n")
	pad := strings.Repeat("# "+strings.Repeat("x", 61)+"\n", guardMaxScript/64+1)
	const mine, project = false, true
	for _, c := range []struct {
		cmd     string
		project bool
		want    string
	}{
		// .1: $$ is the shell's own number, and the { after it opens nothing.
		{`echo $${; sudo ls`, mine, ruleRoot},
		{`echo $${; echo x > .claude/settings.json`, mine, ruleGuard},
		{`echo "$${"; sudo ls`, mine, ruleRoot},
		// .1: ${ cmd; } runs its commands.
		{`: ${ echo x > .claude/settings.json; }`, mine, ruleGuard},
		{`v=${ true; sudo ls; }`, mine, ruleRoot},
		{`echo "${ sudo ls; }"`, mine, ruleRoot},
		{`echo ${| sudo ls; }`, mine, ruleRoot},
		{`echo ${ echo a; }; sudo ls`, mine, ruleRoot},
		{`echo ${x:-a b}; echo ok > docs/a.md`, mine, ""},
		{`echo ${x:-a b}; echo ok > .claude/settings.json`, mine, ruleGuard},
		// .2: bash runs a text file with no #! line as a shell script.
		{"cat > run <<'EOF'\nsudo ls\nEOF\nchmod +x run && ./run", mine, ruleRoot},
		{`./deploy`, mine, ruleRoot},
		{`./deploy`, project, ""},
		{`./notes`, mine, ""}, // a #! line naming a program that is no shell
		// Mark check 2108.1: a remote-tracking ref written by hand would pass
		// the session's own commit off as origin's.
		{`git update-ref refs/remotes/origin/main HEAD`, mine, ruleGuard},
		{`git -C . update-ref -m x refs/remotes/origin/HEAD HEAD`, mine, ruleGuard},
		{`git update-ref --stdin < refs.txt`, mine, ruleGuard},
		{`git symbolic-ref refs/remotes/origin/HEAD refs/heads/mine`, mine, ruleGuard},
		{`git remote set-head origin mine`, mine, ruleGuard},
		{`git fetch . HEAD:refs/remotes/origin/main`, mine, ruleGuard},
		{`git push . +HEAD:refs/remotes/origin/main`, mine, ruleGuard},
		{`git fetch origin mine:refs/remotes/origin/main`, mine, ruleGuard},
		{`git fetch origin main:refs/remotes/origin/main`, mine, ""},
		{`git push origin HEAD:refs/remotes/origin/main`, mine, ""},
		{`git symbolic-ref refs/remotes/origin/HEAD`, mine, ""},
		{`git symbolic-ref --short refs/remotes/origin/HEAD`, mine, ""},
		{`git remote set-head origin -a`, mine, ""},
		{`git remote set-head origin --auto`, mine, ""},
		{`git update-ref refs/heads/tmp HEAD`, mine, ""},
		{`git update-ref -d refs/remotes/origin/gone`, mine, ""},
		{`git fetch origin`, mine, ""},
		{`git fetch origin +refs/heads/*:refs/remotes/origin/*`, mine, ""},
		{`git push origin HEAD:refs/heads/mine`, mine, ""},
		// .12: each shell option that takes a value.
		{`bash --rcfile x tools/bad.sh`, project, ruleGuard},
		{`bash --init-file x tools/bad.sh`, project, ruleGuard},
		{`bash +o posix tools/bad.sh`, project, ruleGuard},
		{`bash -O extglob tools/bad.sh`, project, ruleGuard},
		{`bash +O extglob tools/bad.sh`, project, ruleGuard},
		// .13: a %s with no argument left prints nothing; text past the guard's
		// limit is not kept.
		{`printf 'echo x > .claude/settings.json %s\n' > /tmp/s.sh && sh /tmp/s.sh`, mine, ruleGuard},
		{`printf 'echo ok > docs/a.md %s\n' > /tmp/s.sh && sh /tmp/s.sh`, mine, ""},
		{`printf '%s' '` + pad + `' > /tmp/s.sh && sh /tmp/s.sh`, mine, ruleGuard},
	} {
		g := &guard{site: g0.site, server: g0.server, origin: g0.origin, dir: g0.site.cwd, project: func(string) bool { return c.project }}
		got := ""
		if h := g.bashHit(c.cmd); h != nil {
			got = h.rule
		}
		if got != c.want {
			cmd := c.cmd
			if len(cmd) > 200 {
				cmd = cmd[:60] + " … " + cmd[len(cmd)-60:]
			}
			t.Errorf("%q (project %v): rule %q, want %q", cmd, c.project, got, c.want)
		}
	}
}

// A script the checkout holds unchanged from a commit of origin's default
// branch is the project's though that branch has moved on (audit 2103.9), and
// origin/master is read where there is no origin/HEAD or origin/main (2103.11).
func TestTrackedCleanBehindOrigin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.test"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	s := filepath.Join(dir, "tools", "s.sh")
	git("init", "-q")
	writeFile(t, s, "echo one\n")
	git("add", ".")
	git("commit", "-q", "-m", "one")
	writeFile(t, s, "echo two\n")
	git("commit", "-q", "-am", "two")
	git("update-ref", "refs/remotes/origin/master", "HEAD")
	if !trackedClean(s) {
		t.Error("origin/master is the default branch where no other is known")
	}
	git("reset", "-q", "--hard", "HEAD~1")
	if !trackedClean(s) {
		t.Error("a script unchanged from the commit the checkout left origin at is the project's")
	}
	writeFile(t, s, "sudo ls\n")
	if trackedClean(s) {
		t.Error("an edited script behind origin is not the project's")
	}
	git("commit", "-q", "-am", "mine")
	if trackedClean(s) {
		t.Error("a script the session committed on a branch behind origin is not the project's")
	}
}

// Outside any home the trace sets no HOME, as the server's reading sets none:
// both leave $HOME/x a path they cannot place (audit 2103.8).
func TestTraceVarsSetNoHomeOutsideAHome(t *testing.T) {
	t.Setenv("HOME", "/home/claude")
	if h, ok := traceVars("/srv/app")["HOME"]; ok {
		t.Errorf("HOME = %q in /srv/app, want none", h)
	}
	var cmds []traceCommand
	var mem traceScripts
	traceCommands("echo x > $HOME/app/a.go", "/srv/app", traceVars("/srv/app"), &mem, &cmds)
	if len(cmds) != 1 || len(cmds[0].Wrote) != 1 || cmds[0].Wrote[0].Path != "$HOME/app/a.go" {
		t.Errorf("commands %+v, want one write to $HOME/app/a.go", cmds)
	}
}
