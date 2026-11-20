package core

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// auditAnswer is a runner's structured answer.
func auditResult(summary string, findings ...map[string]any) map[string]any {
	list := []any{}
	for _, f := range findings {
		list = append(list, f)
	}
	return map[string]any{"structured": map[string]any{"summary": summary, "findings": list}, "answer": "(structured)"}
}

func (ae *agentEnv) audit(id int64) *CodeAudit {
	ae.t.Helper()
	ae.work()
	a, err := ae.s.GetAudit(ae.ctx, ae.admin, id)
	if err != nil {
		ae.t.Fatal(err)
	}
	return a
}

// A pull request's audit (WP-348): Grok reads the head read-only, against
// the base; its findings are kept worst first, posted on the pull request
// with a link back, and handed to Claude only when someone asks.
func TestAuditOfAPullRequest(t *testing.T) {
	ae := newAgentEnv(t)
	ae.prHook("opened", ForgePR{Number: 7, Title: "Export the board", Body: "Adds @sam's export. <script>", HeadRef: "export", HeadSHA: "abc1234def", HeadRepo: "me/app", Author: ae.admin.User.Username})
	pr := ae.pr(7)
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{PRID: pr.ID, Via: "web"})
	if err != nil {
		t.Fatal(err)
	}
	// Dispatch starts it in the background, so it may already be running.
	if a.Target != "pr" || (a.Status != AuditQueued && a.Status != AuditRunning) || !a.PostComment || a.Fix || a.Instructions != DefaultAuditInstructions ||
		strings.Join(a.Models, ",") != "cursor/grok-4.7-high,cursor/grok-4.7-medium" {
		t.Fatalf("audit = %+v", a)
	}
	ae.work()
	claim := ae.take()
	in := agentJobOf(t, claim)
	if claim.Kind != JobAudit || in.CLI != "cursor" || in.Model != "grok-4.7-high" || !in.ReadOnly || in.Permission != AgentRead || in.Base != "abc1234def" ||
		in.DiffFrom != "main" || in.JSONSchema == "" || in.Branch != "" {
		t.Fatalf("job = %s %+v", claim.Kind, in)
	}
	for _, want := range []string{"me/app#7", "Export the board", "<untrusted>", DefaultAuditInstructions, "Change nothing"} {
		if !strings.Contains(in.Prompt, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, in.Prompt)
		}
	}
	if got := ae.audit(a.ID); got.Status != AuditRunning || got.JobID == nil || *got.JobID != claim.ID {
		t.Fatalf("running audit = %s job %v", got.Status, got.JobID)
	}
	ae.finish(claim.ID, "done", auditResult("Read the export.",
		map[string]any{"title": "A typo in a log line", "path": "internal/log.go", "line": "L9", "severity": "nit", "problem": "It says recieve."},
		map[string]any{"title": "Export skips the project check | anyone", "path": "./internal/api/export.go", "line": 42, "severity": "P1", "problem": "Any user exports any board.", "confirm": "GET /export?board=2 as a stranger"},
		map[string]any{"title": "Reads /etc", "path": "/etc/passwd", "line": 1, "severity": "critical", "problem": "x"},
	), "")
	got := ae.audit(a.ID)
	if got.Status != AuditDone || got.Model != "cursor/grok-4.7-high" || got.Summary != "Read the export." || got.Counts.Total != 3 || got.Counts.Open != 3 {
		t.Fatalf("done audit = %s %q %q %+v (%s)", got.Status, got.Model, got.Summary, got.Counts, got.Error)
	}
	if got.RequestedBy == nil {
		t.Error("the audit doesn't say who asked")
	}
	if list, err := ae.s.ListAudits(ae.ctx, ae.admin, AuditFilter{}); err != nil || len(list) == 0 || list[0].RequestedBy == nil {
		t.Errorf("listed without who asked: %v", err)
	}
	f := got.Findings
	if f[0].Severity != "critical" || f[0].Path != "" || f[1].Severity != "high" || f[1].Path != "internal/api/export.go" || f[1].Line != 42 || f[1].ID != itoa64(a.ID)+".2" ||
		f[2].Severity != "low" || f[2].Line != 9 {
		t.Fatalf("findings = %+v", f)
	}
	if !strings.HasSuffix(f[1].URL, "/me/app/src/commit/abc1234def/internal/api/export.go#L42") {
		t.Errorf("finding url = %s", f[1].URL)
	}
	comments := ae.comments(7)
	if len(comments) != 1 {
		t.Fatalf("comments = %q", comments)
	}
	c := comments[0]
	for _, want := range []string{"<!-- waypoint-audit id=", "### Audit: 3 findings", `Export skips the project check \| anyone`, "`internal/api/export.go:42`",
		"/audits/" + itoa64(a.ID), "a model's claims"} {
		if !strings.Contains(c, want) {
			t.Errorf("the comment lacks %q:\n%s", want, c)
		}
	}
	if got.CommentURL == "" || got.FixIssue != "" {
		t.Errorf("comment %q fix %q", got.CommentURL, got.FixIssue)
	}
	ns, _, _ := ae.s.ListNotifications(ae.ctx, ae.admin, false, 5)
	if len(ns) == 0 || ns[0].Kind != NotifyAudit || !strings.Contains(ns[0].Title, "3 findings") {
		t.Errorf("notifications = %+v", ns)
	}

	// Verdicts, then Claude's task for what is still open.
	if _, err := ae.s.MarkAuditFindings(ae.ctx, ae.admin, MarkFindings{IDs: []string{f[0].ID}, State: "rejected", Note: "outside the repository"}); err != nil {
		t.Fatal(err)
	}
	// A confirmed finding is still to do: real, not yet fixed.
	if _, err := ae.s.MarkAuditFindings(ae.ctx, ae.admin, MarkFindings{IDs: []string{f[1].ID}, State: "confirmed"}); err != nil {
		t.Fatal(err)
	}
	if got := ae.audit(a.ID); got.Counts.Open != 2 {
		t.Errorf("to do after a rejection and a confirmation = %d, want 2", got.Counts.Open)
	}
	fixed, err := ae.s.FixAudit(ae.ctx, ae.admin, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	task, err := ae.s.GetIssue(ae.ctx, ae.admin, fixed.FixIssue)
	if err != nil {
		t.Fatal(err)
	}
	if task.Agent != "claude" || task.TextAgent != "cursor/grok-4.7-high" || !task.Unreviewed || len(task.Labels) != 1 || task.Labels[0].Name != "audit" ||
		!strings.Contains(task.Title, "2 findings") || strings.Contains(task.Description, "Reads /etc") || !strings.Contains(task.Description, "Confirmed by ") ||
		!strings.Contains(task.Description, "wp audit mark "+itoa64(a.ID)+".1 fixed") || !strings.Contains(task.Description, "fix on its branch") {
		t.Errorf("task = %q agent %q text %q unreviewed %v labels %+v\n%s", task.Title, task.Agent, task.TextAgent, task.Unreviewed, task.Labels, task.Description)
	}
	if again, err := ae.s.FixAudit(ae.ctx, ae.admin, a.ID); err != nil || again.FixIssue != fixed.FixIssue {
		t.Errorf("asking twice = %v %v", again, err)
	}
	if got := ae.audit(a.ID); got.Findings[0].State != "rejected" || got.Findings[0].Note != "outside the repository" || got.Findings[0].By == "" {
		t.Errorf("verdict = %+v", got.Findings[0])
	}
}

// Busy models fall back down the chain, then the whole chain is tried again
// later; a spent plan ends it as limited.
func TestAuditFallsBackAndWaitsOutBusyModels(t *testing.T) {
	ae := newAgentEnv(t)
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Target: "branch", Ref: "wp-222-queue", Instructions: "races in the merge queue"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Base != "main" || a.Ref != "wp-222-queue" {
		t.Fatalf("audit = %+v", a)
	}
	ae.work()
	claim := ae.take()
	if in := agentJobOf(t, claim); in.Base != "wp-222-queue" || in.DiffFrom != "main" || !strings.Contains(in.Prompt, "races in the merge queue") {
		t.Fatalf("job = %+v", in)
	}
	ae.finish(claim.ID, "failed", map[string]any{}, "429 Too Many Requests: the model is at capacity")
	ae.work()
	claim = ae.take()
	if in := agentJobOf(t, claim); in.Model != "grok-4.7-medium" {
		t.Fatalf("the fallback = %+v", in)
	}
	ae.finish(claim.ID, "failed", map[string]any{}, "rate limit exceeded")
	got := ae.audit(a.ID)
	if got.Status != AuditQueued || got.Retries != 1 || got.RetryAt == nil || len(got.Attempts) != 2 || got.Attempts[0].Outcome != "busy" {
		t.Fatalf("waiting audit = %s retries %d at %v attempts %+v", got.Status, got.Retries, got.RetryAt, got.Attempts)
	}
	ae.s.CheckAudits(ae.ctx)
	ae.work()
	if w := ae.rpoll(ae.admin, ae.box); w.Job != nil {
		t.Fatal("a retry started before its time")
	}
	ae.setClock(ae.clockNow().Add(16 * time.Minute))
	ae.s.CheckAudits(ae.ctx)
	ae.work()
	for range 2 {
		claim = ae.take()
		ae.finish(claim.ID, "failed", map[string]any{}, "You've hit your usage limit for Cursor models")
		ae.work()
	}
	got = ae.audit(a.ID)
	if got.Status != AuditLimited || !strings.Contains(got.Error, "usage limit") || len(got.Attempts) != 4 {
		t.Fatalf("limited audit = %s %q %+v", got.Status, got.Error, got.Attempts)
	}
}

// A failure another model won't change ends the audit at once; an answer
// out of form is kept and the next model tried; cancel stops the job.
func TestAuditFailuresAndCancel(t *testing.T) {
	ae := newAgentEnv(t)
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: []string{"grok-4.7-high", "claude/opus"}})
	if err != nil {
		t.Fatal(err)
	}
	ae.work()
	claim := ae.take()
	ae.finish(claim.ID, "done", map[string]any{"answer": "I looked around and it seems fine overall, though hard to say."}, "")
	ae.work()
	claim = ae.take()
	if in := agentJobOf(t, claim); in.CLI != "claude" || in.Model != "opus" {
		t.Fatalf("second model = %+v", in)
	}
	ae.finish(claim.ID, "failed", map[string]any{}, "can't get the repository: authentication failed")
	got := ae.audit(a.ID)
	if got.Status != AuditFailed || !strings.Contains(got.Error, "authentication failed") || got.Attempts[0].Outcome != "unreadable" || got.Answer == "" {
		t.Fatalf("failed audit = %s %q %+v", got.Status, got.Error, got.Attempts)
	}

	b, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Ref: "v1.0..v1.1"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Target != "range" || b.Base != "v1.0" || b.Ref != "v1.1" {
		t.Fatalf("range = %+v", b)
	}
	ae.work()
	claim = ae.take()
	if _, err := ae.s.CancelAudit(ae.ctx, ae.admin, b.ID); err != nil {
		t.Fatal(err)
	}
	if got := ae.audit(b.ID); got.Status != AuditCancelled {
		t.Fatalf("cancelled = %s", got.Status)
	}
	if j, _ := ae.s.stepJobByID(ae.ctx, claim.ID); j == nil || j.State != "cancelled" {
		t.Errorf("the job = %+v", j)
	}
	if _, err := ae.s.CancelAudit(ae.ctx, ae.admin, b.ID); err == nil {
		t.Error("cancelling twice should say it has ended")
	}
}

// Asking checks what can be checked before anything is queued.
func TestRequestAuditRefusals(t *testing.T) {
	ae := newAgentEnv(t)
	for _, tc := range []struct {
		in   AuditInput
		want string
	}{
		{AuditInput{Project: ae.p.Key, Models: []string{"gpt-9"}}, "say which CLI"},
		{AuditInput{Project: ae.p.Key, Target: "range", Ref: "main"}, "FROM..TO"},
		{AuditInput{Project: ae.p.Key, Target: "commit", Ref: "abc;rm -rf"}, "isn't a branch"},
		{AuditInput{Project: ae.p.Key, Instructions: strings.Repeat("x", auditInstructionsMax+1)}, "at most"},
		{AuditInput{}, "say which project"},
	} {
		if _, err := ae.s.RequestAudit(ae.ctx, ae.admin, tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: %v, want %q", tc.in, err, tc.want)
		}
	}
	// A runner whose wp predates audits is named.
	sam := ae.user("sam", true)
	ae.runner(sam, "old", func(r *RunnerRegistration) { r.CLIs = []RunnerCLI{{Name: "cursor"}} })
	if _, err := ae.s.RequestAudit(ae.ctx, sam, AuditInput{Project: ae.p.Key}); err == nil || !strings.Contains(err.Error(), "old runs an older wp") {
		t.Errorf("old runner: %v", err)
	}
	kim := ae.user("kim", true)
	ae.runner(kim, "nocursor", func(r *RunnerRegistration) {
		r.CLIs = []RunnerCLI{{Name: "claude"}}
		r.Features = []string{FeatureAudit}
	})
	if _, err := ae.s.RequestAudit(ae.ctx, kim, AuditInput{Project: ae.p.Key}); err == nil || !strings.Contains(err.Error(), "cursor-agent installed") {
		t.Errorf("no cursor: %v", err)
	}
}

func TestParseAuditAnswer(t *testing.T) {
	for _, text := range []string{"NO FINDINGS", "**No findings.**\n", `{"summary": "Read it all.", "findings": []}`} {
		if a, err := parseAuditAnswer(map[string]any{"answer": text}); err != nil || len(a.Findings) != 0 || a.raised != 0 {
			t.Errorf("%q = %+v %v", text, a, err)
		}
	}
	// An answer that says nothing about the change is no answer: an object
	// with no findings list, words around "no findings", nothing at all.
	// Read as "found nothing" it ended a review round clean (WP-1320).
	for _, text := range []string{`{}`, `{"summary": "I can't review this."}`, `{"findings": null}`, "", "I found no findings in a.go, but b.go has a nil deref.",
		"There were NO FINDINGS because the diff could not be read."} {
		a, err := parseAuditAnswer(map[string]any{"answer": text})
		if err == nil {
			t.Errorf("%q read as an answer: %+v", text, a)
		}
	}
	if _, err := parseAuditAnswer(map[string]any{"answer": `{}`}); !errors.Is(err, errNoFindingsList) {
		t.Errorf("{} = %v", err)
	}
	// The words reviewers write for "barely matters" are low, not the
	// medium an unknown label gets; the worst are kept when there are more
	// than the cap, wherever the model wrote them.
	var many []string
	for i := range auditMaxFindings + 5 {
		many = append(many, fmt.Sprintf(`{"title": "Nit %d", "problem": "A small thing.", "severity": "trivial"}`, i))
	}
	many = append(many, `{"title": "Loses data", "problem": "The write is dropped.", "severity": "Severe"}`, `{"title": "Odd", "problem": "Hm.", "severity": "weird"}`)
	a, err := parseAuditAnswer(map[string]any{"answer": `{"summary": "", "findings": [` + strings.Join(many, ",") + `]}`})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Findings) != auditMaxFindings+7 || a.raised != auditMaxFindings+7 || a.Findings[0].Title != "Loses data" || a.Findings[0].Severity != "critical" ||
		a.Findings[1].Title != "Odd" || a.Findings[1].Severity != "medium" || a.Findings[2].Title != "Nit 0" || a.Findings[2].Severity != "low" {
		t.Errorf("many = %d findings, raised %d, first %+v", len(a.Findings), a.raised, a.Findings[:min(3, len(a.Findings))])
	}
	a, err = parseAuditAnswer(map[string]any{"answer": "```json\n{\"summary\":\"\",\"findings\":[{\"title\":\"\",\"problem\":\"Leaks a file handle\\nmore\",\"path\":\"a/x.go:17\",\"severity\":\"Major\"},{\"title\":\"\",\"problem\":\"\"},{\"title\":\"Climbs out\",\"path\":\"../../etc\",\"severity\":\"P0\"}]}\n```"})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Findings) != 2 || a.Findings[0].Title != "Climbs out" || a.Findings[0].Path != "" || a.Findings[0].Severity != "critical" ||
		a.Findings[1].Title != "Leaks a file handle" || a.Findings[1].Path != "x.go" || a.Findings[1].Line != 17 || a.Findings[1].Severity != "high" || a.Summary != "2 findings." {
		t.Errorf("parsed = %+v", a)
	}
	for _, v := range []string{"cursor/grok-4.7-high", "grok-4.7-medium", "claude/opus", "sonnet", "codex"} {
		if _, err := parseAuditModel(v); err != nil {
			t.Errorf("%s: %v", v, err)
		}
	}
	if m, _ := parseAuditModel("grok-4.7-medium"); m.CLI != "cursor" {
		t.Errorf("a bare grok is cursor's: %+v", m)
	}
}

// Asking for Claude's task before the audit finishes (WP-359) is kept, and
// the task is made when it finishes; an audit that ended without findings
// still says so.
func TestFixAnAuditThatHasNotFinished(t *testing.T) {
	ae := newAgentEnv(t)
	a, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key})
	if err != nil {
		t.Fatal(err)
	}
	early, err := ae.s.FixAudit(ae.ctx, ae.admin, a.ID)
	if err != nil || early.FixIssue != "" || !early.Fix {
		t.Fatalf("asked early = %+v %v", early, err)
	}
	if again, err := ae.s.FixAudit(ae.ctx, ae.admin, a.ID); err != nil || again.FixIssue != "" {
		t.Fatalf("asked twice early = %+v %v", again, err)
	}
	ae.work()
	claim := ae.take()
	ae.finish(claim.ID, "done", auditResult("Read it.",
		map[string]any{"title": "Export skips the project check", "path": "internal/api/export.go", "line": 42, "severity": "high", "problem": "Any user exports any board."},
	), "")
	got := ae.audit(a.ID)
	if got.Status != AuditDone || got.FixIssue == "" {
		t.Fatalf("finished audit = %s fix %q (%s)", got.Status, got.FixIssue, got.Error)
	}
	if again, err := ae.s.FixAudit(ae.ctx, ae.admin, a.ID); err != nil || again.FixIssue != got.FixIssue {
		t.Errorf("asked after = %+v %v", again, err)
	}

	b, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key, Models: []string{"grok-4.7-high"}})
	if err != nil {
		t.Fatal(err)
	}
	ae.work()
	claim = ae.take()
	ae.finish(claim.ID, "failed", map[string]any{}, "can't get the repository: authentication failed")
	if got := ae.audit(b.ID); got.Status != AuditFailed {
		t.Fatalf("audit = %s", got.Status)
	}
	if _, err := ae.s.FixAudit(ae.ctx, ae.admin, b.ID); err == nil || !strings.Contains(err.Error(), "ended failed") {
		t.Errorf("fix a failed audit = %v", err)
	}
}

// A runner takes a pull request's audit before audits asked for earlier of
// a repository, so a backlog of those never holds up the one a pull request
// waits for (WP-356).
func TestAPullRequestsAuditGoesBeforeOtherAudits(t *testing.T) {
	ae := newAgentEnv(t)
	repo, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{Project: ae.p.Key})
	if err != nil {
		t.Fatal(err)
	}
	ae.prHook("opened", ForgePR{Number: 8, Title: "Faster board", HeadRef: "fast", HeadSHA: "fed1234abc", HeadRepo: "me/app", Author: ae.admin.User.Username})
	pr, err := ae.s.RequestAudit(ae.ctx, ae.admin, AuditInput{PRID: ae.pr(8).ID, Via: "web"})
	if err != nil {
		t.Fatal(err)
	}
	ae.work()
	first := ae.take()
	second := ae.take()
	if a, b := ae.audit(pr.ID), ae.audit(repo.ID); a.JobID == nil || *a.JobID != first.ID || b.JobID == nil || *b.JobID != second.ID {
		t.Errorf("took jobs %d then %d; the pull request's audit has job %v, the repository's %v", first.ID, second.ID, a.JobID, b.JobID)
	}
}
