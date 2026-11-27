package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func commitFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "Add merge"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

const briefMergeGo = "package core\n\n// MergeReady says whether a pull request can merge.\nfunc MergeReady(id int) bool {\n\treturn id > 0\n}\n"

// A question needs no server: the code half comes from the local index.
func TestBriefQuestionOffline(t *testing.T) {
	t.Chdir(codeRepo(t))
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", "http://127.0.0.1:1")
	var o, e strings.Builder
	if c := run([]string{"brief", "where is MergeReady decided?"}, &o, &e); c != 0 {
		t.Fatalf("exit %d\n%s%s", c, o.String(), e.String())
	}
	for _, want := range []string{"Code: files this points at", "core/merge.go", "symbol MergeReady"} {
		if !strings.Contains(o.String(), want) {
			t.Errorf("brief lacks %q:\n%s", want, o.String())
		}
	}
	if strings.Contains(o.String(), "Task ") || strings.Contains(o.String(), "Governs") {
		t.Errorf("a question offline has no task or records:\n%s", o.String())
	}
}

// wp next prints a short brief after the task; wp brief is the long one.
func TestNextPrintsCompactBriefAndBriefIsFull(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	t.Setenv("WP_TASK", "")
	t.Setenv("CLAUDECODE", "")
	commitFiles(t, s.repo, map[string]string{"core/merge.go": briefMergeGo, "README.md": "# App\n"})
	i, err := s.svc.CreateIssue(context.Background(), s.me, core.IssueInput{Project: s.project.Key, Title: "Make MergeReady wait for checks", Description: "Change `core/merge.go`: MergeReady returns true too early. Link [[ADR9]] and [[Claim]].", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}

	out := s.wp("next", i.Key)
	for _, want := range []string{"Where to start", "core/merge.go", "Named symbols:"} {
		if !strings.Contains(out, want) {
			t.Errorf("next lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "return id > 0") {
		t.Errorf("the short brief carries no bodies:\n%s", out)
	}

	full := s.wp("brief")
	for _, want := range []string{"Task " + i.Key, "MergeReady returns true too early", "Symbols the text names:", "5  \treturn id > 0"} {
		if !strings.Contains(full, want) {
			t.Errorf("brief lacks %q:\n%s", want, full)
		}
	}

	if out := s.wp("next", i.Key, "--no-brief"); strings.Contains(out, "Where to start") {
		t.Errorf("--no-brief:\n%s", out)
	}
	if out := s.wp("brief", i.Key, "--json"); !strings.Contains(out, `"files"`) {
		t.Errorf("brief --json:\n%s", out)
	}
}

// A decision the task links and also names by its long code is listed once
// under "Governs this" (WP-244).
func TestBriefListsALinkedAndNamedNoteOnce(t *testing.T) {
	s := newSessionEnv(t)
	s.asAgent()
	t.Setenv("WP_TASK", "")
	t.Setenv("CLAUDECODE", "")
	ctx := context.Background()
	commitFiles(t, s.repo, map[string]string{"core/merge.go": briefMergeGo})
	i, err := s.svc.CreateIssue(ctx, s.me, core.IssueInput{Project: s.project.Key, Title: "Make MergeReady wait for checks", Description: "As " + s.project.Key + "-ADR9 and ADR-0009 say, MergeReady waits.", Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.svc.CreateDesign(ctx, s.me, core.DesignInput{Title: ptr("Merges wait for checks"), Kind: ptr("decision"), Body: ptr("A merge waits for its checks."), Project: &s.project.Key, Code: ptr("ADR9"), Links: []string{i.Key}}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"next", i.Key}, {"brief", i.Key}} {
		out := s.wp(args...)
		if n := strings.Count(out, "ADR9 decision (approved): Merges wait for checks"); n != 1 {
			t.Errorf("wp %s lists the note %d times under Governs this, want 1:\n%s", args[0], n, out)
		}
	}
}
