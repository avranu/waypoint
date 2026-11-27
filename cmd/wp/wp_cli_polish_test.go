package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.jmann.me/claude/waypoint/internal/core"
)

func TestWP2011HelpVisibilityAndHiddenSubs(t *testing.T) {
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", "https://waypoint.example.com")
	var out bytes.Buffer
	if code := run([]string{"help"}, &out, &out); code != 0 {
		t.Fatalf("wp help: %d %s", code, out.String())
	}
	help := out.String()
	// Previously orphaned commands under "Agent work" must appear in wp help.
	for _, want := range []string{"  server ", "  ai ", "  objective ", "  experiment "} {
		if !strings.Contains(help, want) {
			t.Errorf("wp help missing %q", want)
		}
	}
	// Hidden duplicates stay out of top-level help.
	for _, hidden := range []string{"  assign ", "  unassign ", "  goal "} {
		if strings.Contains(help, hidden) {
			t.Errorf("wp help still lists hidden command %q", hidden)
		}
	}
	// Hidden subcommands stay out of parent help and usage errors, but still dispatch.
	out.Reset()
	if code := run([]string{"pr", "-h"}, &out, &out); code != 0 {
		t.Fatalf("wp pr -h: %d %s", code, out.String())
	}
	for _, hidden := range []string{"wp pr react", "wp pr unresolve"} {
		if strings.Contains(out.String(), hidden) {
			t.Errorf("wp pr -h still lists hidden subcommand %q", hidden)
		}
	}
	out.Reset()
	if code := run([]string{"pr", "unresolve", "-h"}, &out, &out); code != 0 || !strings.Contains(out.String(), "wp pr unresolve") {
		t.Errorf("hidden alias wp pr unresolve -h failed: %d %s", code, out.String())
	}
	out.Reset()
	if code := run([]string{"glossary", "-h"}, &out, &out); code != 0 {
		t.Fatalf("wp glossary -h: %d %s", code, out.String())
	}
	if strings.Contains(out.String(), "wp glossary show") {
		t.Errorf("wp glossary -h still lists hidden subcommand show")
	}
}

func TestWP2011LsParentSearchAndNewRelate(t *testing.T) {
	s := newSessionEnv(t)
	s.asHuman()
	k := s.project.Key
	s.wp("new", "epic", "Parent epic")
	s.wp("new", "Child task", "--parent", k+"-1")
	s.wp("new", "Follow-up task", "--follows", k+"-2", "--relates", k+"-1")

	if out := s.wp("ls", "--parent", k+"-1"); !strings.Contains(out, "Child task") || strings.Contains(out, "Follow-up task") {
		t.Errorf("wp ls --parent:\n%s", out)
	}
	if out := s.wp("ls", "--search", "Child"); !strings.Contains(out, "Child task") || strings.Contains(out, "Parent epic") {
		t.Errorf("wp ls --search:\n%s", out)
	}
	if out := s.wp("ls", "-q", "Follow-up"); !strings.Contains(out, "Follow-up task") {
		t.Errorf("wp ls -q:\n%s", out)
	}
	if out := s.wp("show", k+"-3", "--comments", "--full"); !strings.Contains(out, "follows") || !strings.Contains(out, k+"-2") || !strings.Contains(out, k+"-1") {
		t.Errorf("wp new --follows/--relates + show flags:\n%s", out)
	}
}

func TestWP2011DesignAndPageRawOut(t *testing.T) {
	s := newSessionEnv(t)
	s.asHuman()
	s.wp("design", "new", "feature", "Raw body test", "-d", "# Heading\n\nBody text here.")
	s.wp("page", "new", "plan", "Raw page test", "-d", "## Plan\n\nStep one.")

	rawDesign := s.wp("design", "show", "1", "--raw")
	if strings.TrimSpace(rawDesign) != "# Heading\n\nBody text here." {
		t.Errorf("design show --raw = %q", rawDesign)
	}
	designOut := filepath.Join(t.TempDir(), "note.md")
	if out := s.wp("design", "show", "1", "-o", designOut); !strings.Contains(out, "Wrote design #1 body") {
		t.Errorf("design show -o: %s", out)
	}
	if b, err := os.ReadFile(designOut); err != nil || strings.TrimSpace(string(b)) != "# Heading\n\nBody text here." {
		t.Errorf("design -o file: %v %q", err, string(b))
	}

	rawPage := s.wp("page", "show", "PL1", "--raw")
	if strings.TrimSpace(rawPage) != "## Plan\n\nStep one." {
		t.Errorf("page show --raw = %q", rawPage)
	}
	pageOut := filepath.Join(t.TempDir(), "plan.md")
	if out := s.wp("page", "show", "PL1", "-o", pageOut); !strings.Contains(out, "Wrote page #1 body") {
		t.Errorf("page show -o: %s", out)
	}
	if b, err := os.ReadFile(pageOut); err != nil || strings.TrimSpace(string(b)) != "## Plan\n\nStep one." {
		t.Errorf("page -o file: %v %q", err, string(b))
	}
	if out, code := s.run("design", "show", "1", "2", "-o", designOut); code != 2 {
		t.Errorf("design show -o with 2 notes: %d %s", code, out)
	}
}

func TestWP2011GoalsConsolidated(t *testing.T) {
	s := newSessionEnv(t)
	s.asHuman()
	s.wp("goal", "add", "Keep CLI intuitive")
	if out := s.wp("goal", "ls"); !strings.Contains(out, "Keep CLI intuitive") {
		t.Errorf("wp goal ls:\n%s", out)
	}
	if out := s.wp("goals", "ls"); !strings.Contains(out, "Keep CLI intuitive") {
		t.Errorf("wp goals ls:\n%s", out)
	}
	if out := s.wp("goal", "show", "1"); !strings.Contains(out, "Keep CLI intuitive") {
		t.Errorf("wp goal show 1:\n%s", out)
	}
	if out := s.wp("goal", "1"); !strings.Contains(out, "Keep CLI intuitive") {
		t.Errorf("wp goal 1:\n%s", out)
	}
}

func TestWP2011AuditFindingShowAndHints(t *testing.T) {
	e := newEnv(t)
	me := e.runnerUser("robin")
	ctx := context.Background()
	e.svc.Forges = &openForge{}
	kind, base, token := "forgejo", "https://git.example.com", "tok"
	f, err := e.svc.CreateForge(ctx, core.SystemActor(), core.ForgeInput{Kind: &kind, BaseURL: &base, Token: &token})
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Waypoint")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AddProjectRepo(ctx, me, p.Key, f.ID, "robin/waypoint", false); err != nil {
		t.Fatal(err)
	}
	box, err := e.svc.RegisterRunner(ctx, me, core.RunnerRegistration{Name: "box", AllowedDirs: []string{t.TempDir()},
		CLIs: []core.RunnerCLI{{Name: "cursor"}}, Features: []string{core.FeatureAudit}})
	if err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	dir := gitRepo(t, "git@git.example.com:robin/waypoint.git")
	sh(t, dir, "checkout", "-q", "-b", "audit-finding-test")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "first")

	e.wp("audit", "-m", "check")
	e.svc.Wait()
	zero := 0.0
	w, err := e.svc.RunnerPoll(ctx, me, core.RunnerPoll{Runner: box.ID, Wait: &zero})
	if err != nil || w.Job == nil {
		t.Fatalf("poll: %v", err)
	}
	res, _ := json.Marshal(map[string]any{"answer": "(structured)", "structured": map[string]any{"summary": "One bug.", "findings": []any{
		map[string]any{"title": "Nil deref", "path": "a.go", "line": 10, "severity": "high", "problem": "x can be nil."},
	}}})
	if _, err := e.svc.FinishRunnerJob(ctx, me, w.Job.ID, core.RunnerFinish{State: "done", Result: res}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	e.svc.Wait()

	// wp audit show includes concrete mark: example with the first open finding ID.
	if out := e.wp("audit", "show", "1"); !strings.Contains(out, "mark: wp audit mark 1.1 fixed") {
		t.Errorf("audit show lacks copy-pastable mark hint:\n%s", out)
	}
	// wp audit show 1.1 and wp show 1.1 both show the single finding.
	for _, args := range [][]string{{"audit", "show", "1.1"}, {"show", "1.1"}} {
		if out := e.wp(args...); !strings.Contains(out, "1.1") || !strings.Contains(out, "Nil deref") || !strings.Contains(out, "x can be nil.") {
			t.Errorf("wp %v:\n%s", args, out)
		}
	}
	// wp audit ls -n 1 works (consistent -n short flag).
	if out := e.wp("audit", "ls", "-n", "1"); !strings.Contains(out, "#1") {
		t.Errorf("audit ls -n 1:\n%s", out)
	}
	// wp audit mark accepts --evidence as an alias for -m.
	if out := e.wp("audit", "mark", "1.1", "rejected", "--evidence", "caller checks nil"); !strings.Contains(out, "Marked 1.1 rejected") {
		t.Errorf("audit mark --evidence:\n%s", out)
	}
}

func TestWP2011PRReopenCommentAndAckFixed(t *testing.T) {
	var paths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/review-comments/unresolve":
			_, _ = w.Write([]byte(`[{"id":"c12","verdict":"new","resolved":false}]`))
		case "/api/v1/review-comments/fixed":
			_, _ = w.Write([]byte(`[{"id":"c12","verdict":"fixed"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	t.Setenv("WAYPOINT_CONFIG_DIR", t.TempDir())
	t.Setenv("WAYPOINT_SERVER", ts.URL)
	t.Setenv("WAYPOINT_TOKEN", "tok")
	dir := gitRepo(t, "git@git.example.com:robin/waypoint.git")
	t.Chdir(dir)

	var out bytes.Buffer
	if code := run([]string{"pr", "reopen", "c12"}, &out, &out); code != 0 || !strings.Contains(out.String(), "c12 new") {
		t.Errorf("wp pr reopen c12: %d %s", code, out.String())
	}
	out.Reset()
	if code := run([]string{"pr", "ack", "c12", "--fixed", "-m", "done"}, &out, &out); code != 0 || !strings.Contains(out.String(), "c12 fixed") {
		t.Errorf("wp pr ack c12 --fixed: %d %s", code, out.String())
	}
	want := []string{"POST /api/v1/review-comments/unresolve", "POST /api/v1/review-comments/fixed"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("requests = %v, want %v", paths, want)
	}
}

func TestWP2011UsageCLISignalsAndRepoFix(t *testing.T) {
	linkCommands()
	// 1. A transcript with no Bash calls (e.g. Haiku single-shot helper) must not count as a CLI session.
	noBashPath := writeLedgerFile(t, "nobash.jsonl",
		`{"type":"assistant","timestamp":"2026-11-26T10:00:00Z","message":{"id":"m1","model":"claude-haiku-4-5","usage":{"input_tokens":100,"output_tokens":20},"content":[{"type":"text","text":"ok"}]}}`,
	)
	noBash := readCLIFile(t, noBashPath, usageFileState{})
	if c := cliCounts(noBash.Chunk.CLI); c["file||"] != 0 {
		t.Errorf("non-Bash transcript counted as CLI session: %v", c)
	}

	// 2. pipe_json and multi_wp friction signals are recorded for provable effectiveness tracking.
	fricPath := writeLedgerFile(t, "fric.jsonl",
		riskyToolUse("m1", "t1", `wp design show 330 --json | python3 -c "import json,sys; print(json.load(sys.stdin)['body'])"`, "2026-11-26T10:00:00Z"),
		riskyResult("t1", false, "ok", "2026-11-26T10:00:01Z"),
		riskyToolUse("m2", "t2", `wp show WP-1 && wp show WP-2`, "2026-11-26T10:01:00Z"),
		riskyResult("t2", false, "ok", "2026-11-26T10:01:01Z"),
	)
	fric := readCLIFile(t, fricPath, usageFileState{})
	fc := cliCounts(fric.Chunk.CLI)
	if fc["file||"] != 1 || fc["signal||pipe_json"] != 1 || fc["signal||multi_wp"] != 1 {
		t.Errorf("friction signals = %v", fc)
	}

	// 3. Deleted runner job worktrees must not pollute repo names with jobNNNN-XXXXXX.
	if got := resolveRepo("/nonexistent/.cache/waypoint/runner/work/job7992-4039459767"); got != "" {
		t.Errorf("resolveRepo(deleted runner job) = %q, want empty", got)
	}
	if got := resolveRepo("/nonexistent/my-project"); got != "my-project" {
		t.Errorf("resolveRepo(regular dir) = %q, want my-project", got)
	}
}
