package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	out := e.wp("audit", "-m", "Look at the cache.")
	if !strings.Contains(out, "Audit #1 of robin/waypoint cache-fix against main asked of cursor/grok-4.7-high, then cursor/grok-4.7-medium") ||
		!strings.Contains(out, "you'll hear when it ends") {
		t.Errorf("wp audit:\n%s", out)
	}
	if out := e.wp("audit", "ls"); !strings.Contains(out, "#1") || !strings.Contains(out, "cache-fix against main") {
		t.Errorf("wp audit ls:\n%s", out)
	}

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

	out = e.wp("audit", "--since", "2w", "--focus", "security,data", "--path", "internal", "--exclude", "*_test.go", "--min-severity", "high", "--show", "5")
	if !strings.Contains(out, "Audit #2 of robin/waypoint main, the last 2 weeks (internal) asked of") {
		t.Errorf("wp audit --since:\n%s", out)
	}
	if out := e.wp("audit", "show", "2"); !strings.Contains(out, "Criteria: focus security, data; only internal; not *_test.go; high and worse; shows 5 findings at a time") {
		t.Errorf("wp audit show 2:\n%s", out)
	}

	e.svc.Wait()
	if w, err := e.svc.RunnerPoll(ctx, me, core.RunnerPoll{Runner: box.ID, Wait: &zero}); err != nil || w.Job == nil {
		t.Fatalf("poll for audit 2: %v %+v", err, w)
	} else {
		var twelve []any
		for i := 1; i <= 12; i++ {
			twelve = append(twelve, map[string]any{"title": fmt.Sprintf("Hole %d", i), "path": "internal/a.go", "line": i, "severity": "high", "problem": "x", "confirm": "y"})
		}
		res, _ := json.Marshal(map[string]any{"answer": "(structured)", "structured": map[string]any{"summary": "A dozen.", "findings": twelve}})
		if _, err := e.svc.FinishRunnerJob(ctx, me, w.Job.ID, core.RunnerFinish{State: "done", Result: res}); err != nil {
			t.Fatal(err)
		}
	}
	e.svc.Wait()
	e.svc.Wait()
	if out := e.wp("audit", "show", "2"); !strings.Contains(out, "Hole 5") || strings.Contains(out, "Hole 6") || !strings.Contains(out, "7 more: wp audit show 2 --all") {
		t.Errorf("wp audit show 2 (5 of 12):\n%s", out)
	}
	if out := e.wp("audit", "show", "2", "--all"); !strings.Contains(out, "Hole 12") || strings.Contains(out, "more: wp audit show") {
		t.Errorf("wp audit show 2 --all:\n%s", out)
	}
	if out := e.wp("audit", "show", "2", "--json"); strings.Count(out, "Hole ") != 12 {
		t.Errorf("wp audit show 2 --json holds %d findings", strings.Count(out, "Hole "))
	}
	if out := e.wp("audit", "recent", "--max-findings", "7"); !strings.Contains(out, "Audit #3 ") {
		t.Errorf("wp audit recent --max-findings:\n%s", out)
	} else if out := e.wp("audit", "show", "3"); !strings.Contains(out, "shows 7 findings at a time") {
		t.Errorf("wp audit show 3:\n%s", out)
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

// The server says how many findings are listed at a time (WP-1421): a check
// of a session's marks has more than its max_findings and lists every one,
// and an older server, which sends no number, gets the old rule.
func TestAuditShowListsWhatTheServerSaysIsShown(t *testing.T) {
	var fs []auditFinding
	for i := 1; i <= 5; i++ {
		fs = append(fs, auditFinding{ID: fmt.Sprintf("9.%d", i), Title: fmt.Sprintf("Mark %d", i), Severity: "low", State: "open"})
	}
	check := auditView{ID: 9, Status: "done", Purpose: "check", MaxFindings: 3, Shown: 100, Findings: fs}
	if out := check.text(); !strings.Contains(out, "Mark 5") || strings.Contains(out, "more: wp audit show") {
		t.Errorf("a check with max_findings 3 and 5 findings:\n%s", out)
	}
	for _, n := range []int{5, 4} {
		exact := auditView{ID: 9, Status: "done", Shown: 5, Findings: fs[:n]}
		if out := exact.text(); strings.Contains(out, "more: wp audit show") || !strings.Contains(out, fmt.Sprintf("Mark %d", n)) {
			t.Errorf("%d findings, shown 5:\n%s", n, out)
		}
	}
	if out := check.text(); strings.Contains(out, "shows 3 findings at a time") {
		t.Errorf("a check that lists every finding says it shows 3:\n%s", out)
	}
	set := auditView{ID: 9, Status: "done", MaxFindings: 4, Shown: 4, Findings: fs}
	if out := set.text(); !strings.Contains(out, "shows 4 findings at a time") || !strings.Contains(out, "1 more: wp audit show 9 --all") {
		t.Errorf("an audit set to show 4:\n%s", out)
	}
	var many []auditFinding
	for i := 1; i <= core.AuditDefaultShown+2; i++ {
		many = append(many, auditFinding{ID: fmt.Sprintf("9.%d", i), Title: fmt.Sprintf("Mark %d", i), Severity: "low", State: "open"})
	}
	bare := auditView{ID: 9, Status: "done", Findings: many}
	if out := bare.text(); !strings.Contains(out, "2 more: wp audit show 9 --all") || strings.Contains(out, "at a time") {
		t.Errorf("an audit with no number:\n%s", out)
	}
	older := auditView{ID: 9, Status: "done", MaxFindings: 3, Findings: fs}
	if out := older.text(); strings.Contains(out, "Mark 4") || !strings.Contains(out, "2 more: wp audit show 9 --all") {
		t.Errorf("an older server's audit:\n%s", out)
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

// wp audit report (WP-1320): the session's own review, reported as its
// round's findings. The CLI reads the file and says what the server made
// of it; what it can tell is wrong without asking, it says as usage.
func TestCLIAuditReport(t *testing.T) {
	var faked atomic.Bool
	var got atomic.Value
	e := newEnvServing(t, nil, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if faked.Load() && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/review-report") {
				raw, _ := io.ReadAll(r.Body)
				got.Store(r.URL.Path + " " + string(raw))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"audit": {"id": 7, "findings": [{"id": "7.1", "severity": "high", "state": "fixed", "title": "Merge loses an update"}]},
					"round": 3, "replaced": true, "dropped": 2, "next": "Run ` + "`wp ship`" + `."}`))
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	me := e.login()
	ctx := context.Background()
	p, err := e.svc.CreateProject(ctx, me, core.ProjectInput{Name: ptr("Waypoint")})
	if err != nil {
		t.Fatal(err)
	}
	i, err := e.svc.CreateIssue(ctx, me, core.IssueInput{Project: p.Key, Title: "First"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Chdir(dir)
	file := filepath.Join(dir, "review.json")
	report := `{"findings": [{"title": "Merge loses an update", "severity": "high", "problem": "p", "outcome": "fixed", "note": "locked"}]}`
	if err := os.WriteFile(file, []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{i.Key}, "give the findings as JSON"},
		{[]string{i.Key, "--none"}, "say what the review covered"},
		{[]string{i.Key, "--none", "-m", "x", "-f", file}, "not both"},
		{[]string{i.Key, "extra", "-f", file}, `unexpected "extra"`},
		{[]string{i.Key, "-f", empty}, "is empty"},
	} {
		if out, code := e.run(append([]string{"audit", "report"}, tc.args...)...); code != 2 || !strings.Contains(out, tc.want) {
			t.Errorf("wp audit report %v: %d %s (want %q)", tc.args, code, out, tc.want)
		}
	}
	if out, code := e.run("audit", "report", i.Key, "--none", "-m", "Read the diff."); code == 0 || !strings.Contains(out, "run it in that session") {
		t.Errorf("outside a session: %d %s", code, out)
	}
	faked.Store(true)
	out := e.wp("audit", "report", i.Key, "-f", file)
	for _, want := range []string{"Replaced your review of " + i.Key + " as audit #7: 1 finding.", "2 more, the least severe, were left out", "7.1  high     fixed    Merge loses an update", "Run `wp ship`."} {
		if !strings.Contains(out, want) {
			t.Errorf("wp audit report lacks %q:\n%s", want, out)
		}
	}
	sent, _ := got.Load().(string)
	path, raw, _ := strings.Cut(sent, " ")
	var body struct {
		None    bool   `json:"none"`
		Summary string `json:"summary"`
		Report  string `json:"report"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil || path != "/api/v1/issues/"+i.Key+"/review-report" || body.None || body.Report != report {
		t.Errorf("sent %q: %+v, %v", sent, body, err)
	}
	e.wp("audit", "report", i.Key, "--none", "-m", "The subagent read the diff; nothing stood.")
	sent, _ = got.Load().(string)
	_, raw, _ = strings.Cut(sent, " ")
	body.Report = ""
	if err := json.Unmarshal([]byte(raw), &body); err != nil || !body.None || body.Summary != "The subagent read the diff; nothing stood." || body.Report != "" {
		t.Errorf("sent %q: %+v, %v", sent, body, err)
	}
}

// wp audit show of an audit that did not end done lists what the Haiku
// checks confirmed (the reviewing model's answer is missing); a done audit,
// a check that didn't end done and a problem the second read refuted are not
// listed (WP-1320).
func TestAuditShowListsThePanelOfAnAuditThatDidNotEndDone(t *testing.T) {
	runs := []core.AuditHelperRun{
		{Check: "goals", State: "done", Leads: []core.PanelLead{
			{ID: "goals.1", Title: "Check's title", Severity: "low", Path: "a.go", Line: 1, Problem: "check's problem", Confirmed: true,
				ReadTitle: "Read title", ReadPath: "b.go", ReadLine: 9, ReadSeverity: "high", ReadProblem: "read problem", Evidence: "b.go:9 shows it"},
			{ID: "goals.2", Title: "Refuted one", Severity: "low", Problem: "nope"}}},
		{Check: "tests", State: "failed", Leads: []core.PanelLead{{ID: "tests.1", Title: "From a failed check", Confirmed: true}}},
	}
	for _, status := range []string{"failed", "cancelled", "limited"} {
		a := auditView{Status: status, HelperRuns: runs}
		out := a.unansweredPanel()
		for _, want := range []string{"The reviewing model did not answer", "[goals] high", "b.go:9: Read title", "read problem", "Evidence: b.go:9 shows it"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s audit: output lacks %q:\n%s", status, want, out)
			}
		}
		for _, not := range []string{"Refuted one", "From a failed check", "Check's title"} {
			if strings.Contains(out, not) {
				t.Errorf("%s audit: output has %q:\n%s", status, not, out)
			}
		}
		if text := a.text(); !strings.Contains(text, "Read title") {
			t.Errorf("%s audit: wp audit show lacks the section:\n%s", status, text)
		}
	}
	if out := (&auditView{Status: "done", HelperRuns: runs}).unansweredPanel(); out != "" {
		t.Errorf("a done audit listed %q", out)
	}
	if out := (&auditView{Status: "failed"}).unansweredPanel(); out != "" {
		t.Errorf("an audit with no runs listed %q", out)
	}
}

// An audit that left the gateway for its size says so where it is read, and
// what the model wasn't given.
func TestAuditViewTriesAndLeftOut(t *testing.T) {
	type attempt = struct {
		Model   string `json:"model"`
		Round   int    `json:"round"`
		Outcome string `json:"outcome"`
		Error   string `json:"error"`
		LeftOut string `json:"left_out"`
	}
	a := auditView{ID: 9, Status: "done", Attempts: []attempt{
		{Model: "gateway/audit", Outcome: "error", Error: "The checkout packs to more than 20 MiB, the most the gateway takes."},
		{Model: "claude/opus", Outcome: "answered", LeftOut: "350 binary files (16.9 MiB)"},
	}}
	out := a.text()
	for _, want := range []string{"Tried: gateway/audit error, claude/opus answered", "gateway/audit: The checkout packs to more than 20 MiB", "Not given to claude/opus: 350 binary files (16.9 MiB)"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	// The audit's own error isn't told twice.
	b := auditView{ID: 9, Status: "failed", Attempts: []attempt{{Model: "gateway/audit", Outcome: "error", Error: "too big"}}}
	b.Error = "too big"
	if out := b.text(); strings.Count(out, "too big") != 1 {
		t.Errorf("the error twice:\n%s", out)
	}
}
