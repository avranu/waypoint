package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

// What a server, a forge or a flag supplies never reaches a command line as
// an option (WP-1370), outside git too. Each refusal has its accept control.

// cursor-agent takes the prompt as an argument, so it comes after "--"; the
// other drivers hand theirs over on standard input and never in argv.
func TestEveryDriverKeepsAPromptThatIsAnOptionOutOfArgv(t *testing.T) {
	work := filepath.Join(t.TempDir(), "job1-prompt")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"--force", "--version", "-x", "fix the bug"} {
		for _, ro := range []bool{false, true} {
			in := agentJob{ReadOnly: ro}
			d, err := cursorDriver(in, prompt, work)
			if err != nil {
				t.Fatalf("cursor %q ro=%v: %v", prompt, ro, err)
			}
			n := len(d.args)
			if n < 2 || d.args[n-2] != "--" || d.args[n-1] != prompt {
				t.Errorf("cursor argv ends %q, want -- then the prompt %q", d.args[max(0, n-2):], prompt)
			}
			for _, name := range []string{"claude", "codex"} {
				var dd *cliDriver
				if name == "claude" {
					dd, err = claudeDriver(in, prompt, work)
				} else {
					dd, err = codexDriver(in, prompt, work)
				}
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if slices.Contains(dd.args, prompt) || !strings.Contains(dd.stdin, prompt) {
					t.Errorf("%s: prompt %q must be on stdin only; args %v", name, prompt, dd.args)
				}
			}
			p := newTestProc(t)
			od := p.opencodeDriver(t.Context(), in, prompt, "", work)
			if slices.Contains(od.args, prompt) || !strings.Contains(od.stdin, prompt) {
				t.Errorf("opencode: prompt %q must be on stdin only; args %v", prompt, od.args)
			}
		}
	}
}

// claude stop is given only an id of the shape claude --bg prints.
func TestFanoutStopOnlyGivesClaudeAnIdItCouldHavePrinted(t *testing.T) {
	rec := fakeCLI(t, "claude", "#!/bin/sh\necho \"$@\" >> \"$FAKE_OUT/stops\"\n")
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	items := []*fanoutItemView{
		{State: "running", AttachID: "--bogus-x"},
		{State: "running", AttachID: "-h"},
		{State: "starting", AttachID: "b50893f0"},
		{State: "running", AttachID: "ab cdef12"},
		{State: "done", AttachID: "deadbeef"},
	}
	if got := stopAttached(bin, items); got != 1 {
		t.Errorf("stopped %d, want 1 (the one legal id of a live item)", got)
	}
	b, _ := os.ReadFile(filepath.Join(rec, "stops"))
	if strings.TrimSpace(string(b)) != "stop b50893f0" {
		t.Errorf("claude was run with %q, want only `stop b50893f0`", b)
	}
}

func TestAttachIDShape(t *testing.T) {
	for _, ok := range []string{"b50893f0", "abcdef", strings.Repeat("a", 36)} {
		if !core.ValidAttachID(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "--bogus-x", "-abcdef", "abcde", strings.Repeat("a", 37), "abcdef\n--x", "ABCDEF12", "abc def12"} {
		if core.ValidAttachID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

// A docker image named like an option is refused before docker is run.
func TestCodeqlImageNameIsNotAnOption(t *testing.T) {
	for _, bad := range []string{"--privileged", "-v", "img --privileged", "img\n", ""} {
		if bad == "" {
			continue // no image is its own, separate refusal
		}
		if checkCodeqlImage(bad) == nil {
			t.Errorf("image %q accepted", bad)
		}
	}
	for _, ok := range []string{"ghcr.io/github/codeql-action/codeql:latest", "codeql", "reg:5000/team/codeql@sha256:abcdef"} {
		if err := checkCodeqlImage(ok); err != nil {
			t.Errorf("image %q refused: %v", ok, err)
		}
	}
	fd := &fakeDocker{}
	if _, err := runCodeqlScan(t.Context(), fd, codeqlOpts{Dir: t.TempDir(), Image: "--privileged"}); err == nil {
		t.Error("the scan accepted --privileged as its image")
	}
}

// The remote the hook reads the repository from is never an option.
func TestHookRemoteNamedLikeAnOptionIsNotGivenToGit(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ran")
	shim := "#!/bin/sh\necho \"$@\" >> " + log + "\ncase \"$*\" in\n*is-inside-work-tree*) echo true;;\n*\"get-url origin\"*) exit 1;;\n*\" remote\") echo \"$REMOTE\";;\n*get-url*) echo https://git.example.com/o/r.git;;\nesac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(shim), 0o755); err != nil { // #nosec G306 -- a test shim must be executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("REMOTE", "--all")
	if g := gitFactsAt(t.TempDir()); g.Repo != "" {
		t.Errorf("an option-named remote gave the repo %q", g.Repo)
	}
	if b, _ := os.ReadFile(log); strings.Contains(string(b), "get-url --all") {
		t.Errorf("git was given the remote as an option: %s", b)
	}
	// Control: a legal name is read.
	t.Setenv("REMOTE", "upstream")
	if g := gitFactsAt(t.TempDir()); g.Repo != "o/r" {
		t.Errorf("a legal remote gave the repo %q, want o/r", g.Repo)
	}
}
