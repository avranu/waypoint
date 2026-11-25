package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/codeindex"
	"git.jmann.me/claude/waypoint/internal/core"
)

// prepRepo is a throwaway checkout with a little Go in it, indexed.
func prepRepo(t *testing.T) (string, *codeindex.Index) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
	}
	run("init", "-q", "-b", "main")
	src := "package merge\n\n// Service merges.\ntype Service struct{}\n\n// MergeReady says whether a pull request may merge.\nfunc (s *Service) MergeReady(id int) bool {\n\treturn checksPassed(id)\n}\n\nfunc checksPassed(id int) bool { return id > 0 }\n"
	if err := os.WriteFile(filepath.Join(dir, "merge.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "merge_test.go"), []byte("package merge\n\nimport \"testing\"\n\nfunc TestMergeReady(t *testing.T) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "merge")
	ix, err := codeindex.Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	if _, err := ix.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	return dir, ix
}

// The bundle carries the ranked files with outlines and the source of the
// symbols the task names, all from the index (WP-212).
func TestBuildPrepBundle(t *testing.T) {
	_, ix := prepRepo(t)
	b, err := buildPrepBundle(context.Background(), ix, "MergeReady refuses drafts", "`MergeReady` should say no for a draft.", "", "WP-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Files) == 0 || b.Files[0].Path != "merge.go" || b.Commit == "" {
		t.Fatalf("bundle files = %+v, commit %q", b.Files, b.Commit)
	}
	var names []string
	for _, y := range b.Files[0].Symbols {
		names = append(names, y.Name)
	}
	if !strings.Contains(strings.Join(names, " "), "Service.MergeReady") {
		t.Errorf("outline = %v", names)
	}
	if len(b.Bodies) == 0 || !strings.Contains(b.Bodies[0].Text, "return checksPassed(id)") {
		t.Errorf("bodies = %+v", b.Bodies)
	}
}

// Printing a prep resolves each symbol in the live index; a pick it no
// longer has is left out, and the full form adds the lines. It only points:
// the facts come from the records.
func TestRenderPrepNotes(t *testing.T) {
	_, ix := prepRepo(t)
	p := &prepView{State: "ready", Model: "LLM gateway · muse-spark", Commit: "abc123def4567890", UpdatedAt: time.Now().Add(-2 * time.Hour)}
	var n prepNotes
	if err := json.Unmarshal([]byte(`{"files": [{"path": "merge.go", "why": "the gate"}],
		"symbols": [{"path": "merge.go", "name": "Service.MergeReady", "why": "decides"}, {"path": "merge.go", "name": "Gone", "why": "renamed since"}],
		"tests": ["merge_test.go"],
		"related": [{"key": "WP-7", "title": "Gate merges on checks", "state": "Done", "prs": [12], "files": ["merge.go"], "why": "changed this gate"}],
		"terms": [{"term": "Merge gate", "definition": "What decides that a pull request may merge.", "why": "the task describes it"}],
		"questions": ["Is a WIP: title a draft?"]}`), &n); err != nil {
		t.Fatal(err)
	}
	p.Notes = &n
	compact := renderPrepNotes(p, ix, 600, true)
	for _, want := range []string{"A weaker model, `muse-spark`, suggested these places to start.\n  merge.go:6-9 Service.MergeReady — decides\n  merge.go — the gate\n",
		"merge.go:6-9 Service.MergeReady — decides", "Tests: merge_test.go",
		"WP-7 Gate merges on checks [Done; PR #12; changed merge.go] — changed this gate",
		"Merge gate: What decides that a pull request may merge. — the task describes it", "Open in the task text:\n    - Is a WIP: title a draft?"} {
		if !strings.Contains(compact, want) {
			t.Errorf("compact lacks %q:\n%s", want, compact)
		}
	}
	if strings.Contains(compact, "Gone") || strings.Contains(compact, "checksPassed(id)") {
		t.Errorf("compact has a gone symbol or code:\n%s", compact)
	}
	full := renderPrepNotes(p, ix, 3000, false)
	if !strings.Contains(full, "8  \treturn checksPassed(id)") {
		t.Errorf("full form:\n%s", full)
	}
}

// prepModel answers the prep model's requests with a fixed reading and
// anything else (estimates) with points.
type prepModel struct{}

func (prepModel) Models(ctx context.Context, p *core.AIProvider) ([]core.AIModel, error) {
	return []core.AIModel{{ID: "opencode/big-pickle", Name: "Big Pickle", Free: true}}, nil
}

func (prepModel) Complete(ctx context.Context, p *core.AIProvider, model, system, prompt string, schema map[string]any) (json.RawMessage, string, error) {
	if !strings.HasPrefix(prompt, "Task ") {
		return json.RawMessage(`{"points": 3, "confidence": "high", "reason": "Small."}`), "", nil
	}
	return json.RawMessage(`{"files": [{"path": "merge.go", "why": "the gate"}],
		"symbols": [{"path": "merge.go", "name": "MergeReady", "why": "decides"}], "tests": ["merge_test.go"],
		"related": [], "terms": [], "questions": ["Is a WIP: title a draft?"]}`), "", nil
}

// wp prep sends this checkout's bundle; once the model has read it, the
// brief of the task and wp show (as the agent reads them) have the prep,
// with the symbol where it is in the checkout.
func TestPrepAgainstServer(t *testing.T) {
	e := newEnv(t)
	me := e.login()
	ctx := context.Background()
	e.svc.LLM = prepModel{}
	if err := e.svc.EnsureDefaultAI(ctx, "http://opencode:4096", "", "opencode/big-pickle"); err != nil {
		t.Fatal(err)
	}
	name := "Merges"
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	is, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "MergeReady refuses drafts", Description: "`MergeReady` should say no for a draft."})
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := prepRepo(t)
	t.Chdir(dir)
	if out := e.wp("prep", is.Key); !strings.Contains(out, "sent 1 file from") {
		t.Fatalf("wp prep: %s", out)
	}
	e.svc.Wait()
	out := e.wp("brief", is.Key)
	for _, want := range []string{"A weaker model, `opencode/big-pickle`, suggested these places to start.\n  merge.go:6-9", "merge.go:6-9 Service.MergeReady — decides", "Is a WIP: title a draft?", "return checksPassed(id)"} {
		if !strings.Contains(out, want) {
			t.Errorf("brief lacks %q:\n%s", want, out)
		}
	}
	if _, err := e.svc.AddComment(ctx, me, is.Key, "Drafts too."); err != nil {
		t.Fatal(err)
	}
	out = e.wp("show", is.Key)
	if p, c := strings.Index(out, "merge.go:6-9 Service.MergeReady — decides"), strings.Index(out, "Drafts too."); p < 0 || c < 0 || p < c {
		t.Errorf("wp show has no prep after the comments:\n%s", out)
	}
	if out := e.wp("prep", "show", is.Key); !strings.Contains(out, is.Key+" prep: ready") || !strings.Contains(out, "Is a WIP: title a draft?") {
		t.Errorf("wp prep show:\n%s", out)
	}
}
