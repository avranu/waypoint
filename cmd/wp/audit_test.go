package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"git.jmann.me/claude/waypoint/internal/core"
)

// wp audit (WP-348): asking for one on the branch here, reading it, and
// recording verdicts on its findings.
func TestCLIAudit(t *testing.T) {
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
	sh(t, dir, "checkout", "-q", "-b", "cache-fix")
	sh(t, dir, "commit", "-q", "--allow-empty", "-m", "first")

	if out, code := e.run("audit", "nonsense"); code != 2 || !strings.Contains(out, "say what") {
		t.Errorf("a bare word: %d %s", code, out)
	}
	// No pull request for the branch here: the branch against the default.
	out := e.wp("audit", "-m", "Look at the cache.")
	if !strings.Contains(out, "Audit #1 of robin/waypoint cache-fix against main asked of cursor/grok-4.7-high, then cursor/grok-4.7-medium") ||
		!strings.Contains(out, "you'll hear when it ends") {
		t.Errorf("wp audit:\n%s", out)
	}
	if out := e.wp("audit", "ls"); !strings.Contains(out, "#1") || !strings.Contains(out, "cache-fix against main") {
		t.Errorf("wp audit ls:\n%s", out)
	}

	// The runner answers.
	e.svc.Wait()
	zero := 0.0
	w, err := e.svc.RunnerPoll(ctx, me, core.RunnerPoll{Runner: box.ID, Wait: &zero})
	if err != nil || w.Job == nil {
		t.Fatalf("poll: %v %+v", err, w)
	}
	res, _ := json.Marshal(map[string]any{"answer": "(structured)", "structured": map[string]any{"summary": "Two problems in the cache.", "findings": []any{
		map[string]any{"title": "Stale entries survive a write", "path": "cache.go", "line": 41, "severity": "high", "problem": "Put doesn't drop the old key.", "confirm": "Write twice, read once."},
		map[string]any{"title": "Unbounded growth", "path": "cache.go", "line": 9, "severity": "low", "problem": "Nothing evicts."},
	}}})
	if _, err := e.svc.FinishRunnerJob(ctx, me, w.Job.ID, core.RunnerFinish{State: "done", Result: res}); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	e.svc.Wait()

	out = e.wp("audit", "show", "1")
	for _, want := range []string{"done by cursor/grok-4.7-high: 2 findings (1 high, 1 low), 2 to do", "Summary: Two problems in the cache.",
		"1.1", "cache.go:41: Stale entries survive a write", "To confirm: Write twice, read once.", "1.2", "/audits/1"} {
		if !strings.Contains(out, want) {
			t.Errorf("wp audit show lacks %q:\n%s", want, out)
		}
	}
	if out := e.wp("audit", "mark", "1.2", "rejected", "-m", "the cache is per request"); !strings.Contains(out, "Marked 1.2 rejected") {
		t.Errorf("mark: %s", out)
	}
	if out, code := e.run("audit", "mark", "1.1", "maybe"); code != 2 || !strings.Contains(out, "verdict") {
		t.Errorf("a bad verdict: %d %s", code, out)
	}
	if out := e.wp("audit", "show", "1"); !strings.Contains(out, "rejected") || !strings.Contains(out, "the cache is per request") || !strings.Contains(out, "1 to do") {
		t.Errorf("after marking:\n%s", out)
	}
	if out, code := e.run("audit", "cancel", "1"); code == 0 || !strings.Contains(out, "has ended") {
		t.Errorf("cancel an ended audit: %d %s", code, out)
	}
	if out, code := e.run("audit", "cancel", "1,", "x"); code != 1 || !strings.Contains(out, "has ended") || !strings.Contains(out, `"x" isn't an audit's number`) {
		t.Errorf("cancel two, both refused: %d %s", code, out)
	}

	// Recent changes, with criteria (WP-548).
	out = e.wp("audit", "--since", "2w", "--focus", "security,data", "--path", "internal", "--exclude", "*_test.go", "--min-severity", "high", "--max-findings", "5")
	if !strings.Contains(out, "Audit #2 of robin/waypoint main, the last 2 weeks (internal) asked of") {
		t.Errorf("wp audit --since:\n%s", out)
	}
	if out := e.wp("audit", "show", "2"); !strings.Contains(out, "Criteria: focus security, data; only internal; not *_test.go; high and worse; at most 5 findings") {
		t.Errorf("wp audit show 2:\n%s", out)
	}
	if out := e.wp("audit", "recent", "release"); !strings.Contains(out, "robin/waypoint release, the last 7 days") {
		t.Errorf("wp audit recent release:\n%s", out)
	}
	if out, code := e.run("audit", "commit", "abc1234", "--since", "7d"); code != 2 || !strings.Contains(out, "--since is for recent changes") {
		t.Errorf("--since on a commit: %d %s", code, out)
	}
	if out, code := e.run("audit", "recent", "--focus", "vibes"); code == 0 || !strings.Contains(out, "isn't a focus area") {
		t.Errorf("a bad focus: %d %s", code, out)
	}
}

func TestAuditStateSaysWhenNoRunnerHasTakenIt(t *testing.T) {
	a := auditView{Status: "running", JobState: "queued", Models: []string{"cursor/grok-4.7-high", "cursor/grok-4.7-medium"}}
	if got := a.state(); got != "waiting for a runner to take it (cursor/grok-4.7-high)" {
		t.Errorf("queued job: %q", got)
	}
	a.JobState, a.Slot = "running", 1
	if got := a.state(); got != "running on cursor/grok-4.7-medium" {
		t.Errorf("running job: %q", got)
	}
}

// A long audit says where it is and how far it got (WP-1064): a person
// asking whether an audit two hours in still works had to read the
// runner's API by hand.
func TestAuditJobText(t *testing.T) {
	started := time.Date(2026, 11, 13, 20, 37, 0, 0, time.UTC)
	log := "Fetching git.test/a/b.git\nPacked the checkout\nGateway took the audit as task task_a (queued)\nTask task_a is running\nGateway restarted under task task_a; sending the audit again\nGateway took the audit as task task_b (queued)\nTask task_b is running\n"
	j := auditJobView{ID: 4391, State: "running", Runner: "runner1", StartedAt: &started, Timeout: 15000, Log: &log}
	j.Gateway = &struct {
		ID           string `json:"id"`
		Status       string `json:"status"`
		InputTokens  int    `json:"input_tokens"`
		OutputTokens int    `json:"output_tokens"`
	}{ID: "task_b", Status: "running", InputTokens: 3509536, OutputTokens: 94281}
	got := j.text(started.Add(105 * time.Minute))
	for _, want := range []string{
		"Job 4391 on runner1 since ", "(1h45m of at most 4h10m)",
		"Gateway task task_b: running, 3,509,536 tokens in and 94,281 out so far",
		"    Task task_b is running\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Fetching") {
		t.Errorf("printed more than the last %d lines:\n%s", auditLogLines, got)
	}
	q := auditJobView{ID: 4398, State: "queued", Waiting: "runner1 runs 6 audits through the gateway at once beside its other jobs; it is running 6 audits."}
	if got := q.text(started); got != "  Job 4398: runner1 runs 6 audits through the gateway at once beside its other jobs; it is running 6 audits.\n" {
		t.Errorf("queued: %q", got)
	}
	if got := (&auditJobView{State: "done"}).text(started); got != "" {
		t.Errorf("done: %q", got)
	}
}
