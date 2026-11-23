package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Another model's audit first in adversarial review rounds (WP-687): the
// default audit, which is Grok through Cursor on a runner and then the LLM
// gateway (WP-871, WP-1725). Most rounds here have no runner with Cursor, so
// the gateway runs theirs.

// auditAPEnv is reviewAPEnv with the LLM gateway set up and a runner of
// the person's that sends audits to it. The runner has Claude Code signed in
// too, which a round never asks; it has no Cursor, so a round's audit goes to
// the gateway (with Cursor online it would go to Grok first, WP-1725).
func auditAPEnv(t *testing.T) (*apEnv, *Runner) {
	t.Helper()
	a := reviewAPEnv(t)
	taskGateway(t, a.s)
	box := a.runner(a.admin, "auditbox", func(r *RunnerRegistration) {
		r.CLIs = []RunnerCLI{{Name: "claude"}}
		r.Features = []string{FeatureConfine, FeatureAudit, FeatureGatewayAudit}
	})
	a.s.Forges = newPRForge()
	return a, box
}

// grokPR links the task's open pull request, which the forge shows too.
func (a *apEnv) grokPR(issueID int64, n int, sha string) {
	a.t.Helper()
	a.linkPR(issueID, n, sha)
	var forge int64
	if err := a.s.db.R.QueryRow(`SELECT forge_id FROM pull_requests WHERE number = ?`, n).Scan(&forge); err != nil {
		a.t.Fatal(err)
	}
	if _, err := a.s.db.W.Exec(`INSERT OR IGNORE INTO project_repos(project_id, forge_id, repo, created_at) VALUES (?, ?, 'acme/app', 0)`, a.p.ID, forge); err != nil {
		a.t.Fatal(err)
	}
	pf := a.s.Forges.(*prForge)
	pf.mu.Lock()
	defer pf.mu.Unlock()
	pf.prs[mk(&Forge{ID: forge}, "acme/app", n)] = &ForgePR{Number: n, Title: "First", State: "open", Author: a.admin.User.Username,
		BaseRef: "main", HeadRef: "wp-1", HeadSHA: sha, HeadRepo: "acme/app"}
}

// roundAudit is the round's audit as Waypoint keeps it.
func (a *apEnv) roundAuditRow(id int64) (roundAudit, *CodeAudit) {
	a.t.Helper()
	a.s.Wait()
	ra, err := a.s.roundAuditOf(a.ctx, id)
	if err != nil {
		a.t.Fatal(err)
	}
	if ra.id == 0 {
		return ra, nil
	}
	au, err := a.s.auditRow(a.ctx, ra.id)
	if err != nil {
		a.t.Fatal(err)
	}
	return ra, au
}

// endAudit runs the queued audit job on the box and ends it with findings
// given by title.
func (a *apEnv) endAudit(box *Runner, state string, titles ...string) {
	a.t.Helper()
	a.s.Wait()
	w := a.poll(a.admin, box, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobAudit {
		a.t.Fatalf("no audit job: %+v", w.Job)
	}
	a.finishAudit(w.Job, state, titles...)
}

// endReviewer runs the round's Sonnet reviewer, a Claude Code job on the
// box, and ends it as endAudit does.
func (a *apEnv) endReviewer(box *Runner, state string, titles ...string) {
	a.t.Helper()
	a.endReviewerAs(box, "sonnet", state, titles...)
}

// endReviewerAs is endReviewer for a reviewer of the model given (Haiku
// below high stakes, WP-1306).
func (a *apEnv) endReviewerAs(box *Runner, model, state string, titles ...string) {
	a.t.Helper()
	a.s.Wait()
	w := a.poll(a.admin, box, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobAudit {
		a.t.Fatalf("no reviewer job: %+v", w.Job)
	}
	if job := agentJobOf(a.t, w.Job); job.CLI != "claude" || job.Model != model || !job.ReadOnly {
		a.t.Fatalf("the reviewer's job = %s/%s (read-only %v)", job.CLI, job.Model, job.ReadOnly)
	}
	if state != "done" {
		if _, err := a.s.FinishRunnerJob(a.ctx, a.admin, w.Job.ID, RunnerFinish{State: state, Error: "claude exited with status 1"}); err != nil {
			a.t.Fatal(err)
		}
		a.s.Wait()
		a.s.Wait()
		return
	}
	a.finishAudit(w.Job, state, titles...)
}

func (a *apEnv) finishAudit(j *RunnerClaim, state string, titles ...string) {
	a.t.Helper()
	w := struct{ Job *RunnerClaim }{j}
	var list []map[string]any
	for i, title := range titles {
		list = append(list, map[string]any{"title": title, "path": "internal/merge.go", "line": 10 + i, "severity": "major",
			"problem": "Problem of " + title, "confirm": "Merge twice"})
	}
	res, errText := auditResult("Read it.", list...), ""
	if state != "done" {
		res, errText = nil, "plan spent"
	}
	if _, err := a.s.FinishRunnerJob(a.ctx, a.admin, w.Job.ID, RunnerFinish{State: state, Result: []byte(mustJSON(res)), Error: errText}); err != nil {
		a.t.Fatal(err)
	}
	a.s.Wait()
	a.s.Wait()
}

// The round waits for another model's audit of the pull request, then types its
// findings for the session to check, fix and mark, instead of the round's
// own prompt; the merge stays held meanwhile.
func TestReviewRoundAuditFirst(t *testing.T) {
	a, box := auditAPEnv(t)
	// Cursor is on a runner too, but signed out: the round's Grok entries have
	// nowhere to run, so its audit goes to the gateway, and Cursor isn't called.
	cursor := a.runner(a.admin, "cursorbox", func(r *RunnerRegistration) {
		out := false
		r.CLIs = []RunnerCLI{{Name: "cursor", SignedIn: &out}}
		r.Features = []string{FeatureConfine, FeatureAudit}
	})
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.grokPR(first.ID, 7, "aaa")
	a.stopWith("Shipped as #7.")
	roundID := a.step().reviewID.Int64

	a.tick(61 * time.Second)
	if len(a.jobs) != 0 {
		ra, _ := a.roundAuditRow(roundID)
		t.Fatalf("typed the review before the audit (%q): %+v", ra.note, a.jobs)
	}
	st := a.step()
	if st.state != StepWaiting || st.reason != WaitAudit || !strings.Contains(st.detail, "another model's audit") {
		t.Fatalf("step = %+v", st)
	}
	ra, au := a.roundAuditRow(roundID)
	if got := strings.Join(au.Models, " "); got != "cursor/grok-4.7-high cursor/grok-4.7-medium gateway/audit" {
		t.Fatalf("the round's audit asks %s", got)
	}
	a.s.Wait()
	if w := a.poll(a.admin, cursor, RunnerPoll{}); w.Job != nil {
		t.Fatalf("the round called Cursor: %+v", agentJobOf(t, w.Job))
	}
	var owner int64
	_ = a.s.db.R.QueryRow(`SELECT owner_id FROM code_audits WHERE id = ?`, ra.id).Scan(&owner)
	if au == nil || au.Purpose != AuditReview || au.PostComment || owner != a.admin.User.ID || ra.note != "" {
		t.Fatalf("round audit = %+v %+v (owner %d)", ra, au, owner)
	}
	pr := &PullRequest{URL: "https://git.example/acme/app/pulls/7", Repo: "acme/app", Number: 7}
	if why, _, _ := a.s.reviewHold(a.ctx, pr); why == "" {
		t.Error("the merge isn't held while the audit runs")
	}
	// A second look while it runs asks for no second audit.
	a.tick(reviewAuditRecheck + time.Second)
	if ra2, _ := a.roundAuditRow(roundID); ra2.id != ra.id {
		t.Fatalf("a second audit: %d then %d", ra.id, ra2.id)
	}

	a.endAudit(box, "done", "Merge reads a nil map", "The retry loops")
	if st := a.step(); st.dueAt > a.clockNow().UnixMilli() {
		t.Errorf("the audit's end didn't wake the step: %+v", st)
	}
	a.tick(time.Second)
	if len(a.jobs) != 1 {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	if in.ReviewOf != first.Key || !strings.HasPrefix(in.Review, AutopilotReviewMark+first.Key+": ") {
		t.Fatalf("review job = %+v", in)
	}
	for _, want := range []string{"Merge reads a nil map", "The retry loops", "wp audit mark " + itoa64(au.ID) + ".1 fixed", "<untrusted>"} {
		if !strings.Contains(in.Review, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, in.Review)
		}
	}
	if DefaultReviewPrompt != "" && strings.Contains(in.Review, DefaultReviewPrompt) {
		t.Error("typed the round's own prompt too")
	}
	if r := a.round(roundID); r.prompt == DefaultReviewPrompt || !strings.Contains(r.prompt, "Merge reads a nil map") {
		t.Errorf("the round's prompt wasn't the audit's findings: %q", r.prompt)
	}
	// The round runs as any other: its prompt starts it.
	a.prompt(in.Review)
	if r := a.round(roundID); r.state != RoundRunning {
		t.Fatalf("after the prompt: %+v", r)
	}
}

// The audit found nothing: the round ends with that as its report and the
// session moves on without being asked.
func TestReviewRoundAuditFoundNothing(t *testing.T) {
	a, box := auditAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	second := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.grokPR(first.ID, 7, "aaa")
	a.stopWith("Shipped as #7.")
	roundID := a.step().reviewID.Int64
	a.tick(61 * time.Second)
	a.endAudit(box, "done")
	a.tick(time.Second)
	r := a.round(roundID)
	if r.state != RoundDone || !strings.Contains(r.body, "found nothing") || r.readState != readDone {
		t.Fatalf("round = %+v", r)
	}
	var in AutopilotJob
	if len(a.jobs) != 1 || json.Unmarshal(a.jobs[0].Input, &in) != nil || in.Key != second.Key || in.ReviewOf != "" {
		t.Fatalf("jobs = %+v", a.jobs)
	}
}

// A Waypoint with no gateway asks Cursor on the runner, as audits did before
// the gateway could run them. When that fails the Sonnet reviewer does: the
// round asks no second audit of the same models.
func TestReviewRoundWithoutAGateway(t *testing.T) {
	a := reviewAPEnv(t)
	a.s.Forges = newPRForge()
	box := a.runner(a.admin, "cursorbox", func(r *RunnerRegistration) {
		r.CLIs = []RunnerCLI{{Name: "cursor"}, {Name: "claude"}}
		r.Features = []string{FeatureConfine, FeatureAudit, FeatureGatewayAudit}
	})
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.grokPR(first.ID, 7, "aaa")
	a.stopWith("Shipped as #7.")
	id := a.step().reviewID.Int64
	a.tick(61 * time.Second)
	ra, au := a.roundAuditRow(id)
	if got := strings.Join(au.Models, " "); got != "cursor/grok-4.7-high cursor/grok-4.7-medium" {
		t.Fatalf("the round's audit asks %s", got)
	}
	for range 2 {
		a.s.Wait()
		w := a.poll(a.admin, box, RunnerPoll{})
		if w.Job == nil || w.Job.Kind != JobAudit {
			t.Fatalf("no audit job: %+v", w.Job)
		}
		if _, err := a.s.FinishRunnerJob(a.ctx, a.admin, w.Job.ID, RunnerFinish{State: "failed", Error: "cursor-agent reported an error: 502 Bad Gateway"}); err != nil {
			t.Fatal(err)
		}
		a.s.Wait()
		a.s.Wait()
	}
	a.tick(time.Second)
	ra2, _ := a.roundAuditRow(id)
	if ra2.id != ra.id || ra2.alt != "" || !strings.Contains(ra2.note, "Grok's audit #") {
		t.Fatalf("round = %+v", ra2)
	}
	// The Sonnet reviewer, a new Claude Code session, reviews it in the
	// audit's place, and its findings are typed into the waiting session
	// (WP-1206).
	if len(a.jobs) != 0 || !strings.Contains(a.step().detail, "Sonnet review #") {
		t.Fatalf("didn't wait for the Sonnet review: %+v %+v", a.step(), a.jobs)
	}
	a.endReviewer(box, "done", "Merge reads a nil map")
	if st := a.step(); st.dueAt > a.clockNow().UnixMilli() {
		t.Errorf("the reviewer's end didn't wake the step: %+v", st)
	}
	a.tick(time.Second)
	var in AutopilotJob
	if len(a.jobs) != 1 || json.Unmarshal(a.jobs[0].Input, &in) != nil {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	for _, want := range []string{"Merge reads a nil map", "Claude Sonnet in a new Claude Code session"} {
		if !strings.Contains(in.Review, want) {
			t.Errorf("the prompt lacks %q:\n%s", want, in.Review)
		}
	}
	if strings.Contains(in.Review, DefaultReviewPrompt) {
		t.Error("the session was asked to review it itself")
	}
}

// Only a person cancelling ends an audit: its runner restarting doesn't
// put the round on its own prompt (WP-820).
func TestReviewRoundRunnerStopped(t *testing.T) {
	a, box := auditAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.grokPR(first.ID, 7, "aaa")
	a.stopWith("Shipped as #7.")
	id := a.step().reviewID.Int64
	a.tick(61 * time.Second)
	a.s.Wait()
	w := a.poll(a.admin, box, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobAudit {
		t.Fatalf("no audit job: %+v", w.Job)
	}
	if _, err := a.s.FinishRunnerJob(a.ctx, a.admin, w.Job.ID, RunnerFinish{State: "cancelled", Error: runnerStoppedError}); err != nil {
		t.Fatal(err)
	}
	a.s.Wait()
	a.s.Wait()
	a.tick(time.Second)
	if ra, au := a.roundAuditRow(id); ra.alt != "" || ra.note != "" || au.Status == AuditCancelled {
		t.Fatalf("round = %+v, audit = %+v", ra, au)
	}
	a.endAudit(box, "done", "Merge reads a nil map")
	if _, au := a.roundAuditRow(id); au.Status != AuditDone || au.Model != "gateway/audit" {
		t.Fatalf("audit = %+v", au)
	}
}

// A read that fails while the round asks for its audit isn't the audit
// having failed: the round keeps waiting and asks at its step's next look,
// rather than leaving the review to the session for good.
func TestReviewRoundReadFails(t *testing.T) {
	a, _ := auditAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.grokPR(first.ID, 7, "aaa")
	a.stopWith("Shipped as #7.")
	st := a.step()
	id := st.reviewID.Int64
	rename := func(from, to string) {
		t.Helper()
		if _, err := a.s.db.W.Exec(`ALTER TABLE ` + from + ` RENAME TO ` + to); err != nil {
			t.Fatal(err)
		}
	}
	rename("pull_requests", "gone_pull_requests")
	plan, err := a.s.planReview(a.ctx, st, a.round(id), a.clockNow().UnixMilli())
	rename("gone_pull_requests", "pull_requests")
	if err == nil {
		t.Fatalf("a failed read was taken for an answer: %+v", plan)
	}
	if ra, _ := a.roundAuditRow(id); ra.note != "" || ra.id != 0 {
		t.Fatalf("the round gave up on its audit after a failed read: %+v", ra)
	}
	plan, err = a.s.planReview(a.ctx, st, a.round(id), a.clockNow().UnixMilli())
	if err != nil || plan.wait == "" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if ra, au := a.roundAuditRow(id); ra.id == 0 || au == nil || ra.note != "" {
		t.Fatalf("the round didn't ask for its audit at the next look: %+v", ra)
	}
}

// The gateway's audit failing is the end of the round's audit (WP-871): the
// gateway has its own failover, so Waypoint asks no other model behind it,
// not its free models under another name. Sonnet reviews it in a new Claude
// Code session (WP-1206, WP-ADR117), and the session that waits reviews it
// itself only when that fails too.
func TestReviewRoundGatewayFails(t *testing.T) {
	a, box := auditAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.grokPR(first.ID, 7, "aaa")
	a.stopWith("Shipped as #7.")
	id := a.step().reviewID.Int64
	a.tick(61 * time.Second)
	a.s.Wait()
	w := a.poll(a.admin, box, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobAudit {
		t.Fatalf("no audit job: %+v", w.Job)
	}
	if job := agentJobOf(t, w.Job); job.CLI != GatewayCLI || job.Model != gatewayAuditModel {
		t.Fatalf("the round's job = %s/%s", job.CLI, job.Model)
	}
	if _, err := a.s.FinishRunnerJob(a.ctx, a.admin, w.Job.ID, RunnerFinish{State: "failed", Error: "502 Bad Gateway"}); err != nil {
		t.Fatal(err)
	}
	a.s.Wait()
	a.s.Wait()
	if w := a.poll(a.admin, box, RunnerPoll{}); w.Job != nil {
		t.Fatalf("another model was asked behind the gateway: %+v", agentJobOf(t, w.Job))
	}
	a.tick(time.Second)
	ra, au := a.roundAuditRow(id)
	// Attempts: the two Cursor entries passed over (no runner with it), then the gateway's.
	if au.Status != AuditFailed || len(au.Attempts) != 3 || ra.alt != "" || !strings.Contains(ra.note, "Another model's audit #") {
		t.Fatalf("round = %+v, audit = %s %+v", ra, au.Status, au.Attempts)
	}
	if len(a.jobs) != 0 {
		t.Fatalf("the session was asked before the Sonnet review: %+v", a.jobs)
	}
	a.endReviewer(box, "failed")
	a.tick(time.Second)
	var in AutopilotJob
	if len(a.jobs) != 1 || json.Unmarshal(a.jobs[0].Input, &in) != nil || in.Review != AutopilotReviewMark+first.Key+": "+ownReviewBrief(DefaultReviewPrompt, AutopilotLimits{}) {
		t.Fatalf("the session wasn't asked for its own review: %+v", a.jobs)
	}
}

// A pull request's own audit that hasn't ended is the round's when no model
// it may still ask is of the session's family.
func TestOtherFamilyChain(t *testing.T) {
	if got := auditModelLabel("opencode/opencode/deepseek-v4.1-flash-free"); got != "opencode/deepseek-v4.1-flash-free" {
		t.Errorf("label = %q", got)
	}
	gateway := []AgentModel{{CLI: GatewayCLI, Model: gatewayAuditModel}}
	for _, c := range []struct {
		name    string
		chain   []AgentModel
		author  string
		session string
		want    bool
	}{
		{"the gateway, kept from the session's family", gateway, "claude-opus-5-5", "claude-fable-5-2", true},
		{"the gateway, by the pull request's text", gateway, "anthropic/claude-code", "claude-opus-5-5", true},
		{"the gateway, free to choose any", gateway, "", "claude-opus-5-5", false},
		{"the gateway, kept from another family only", gateway, "gpt-6", "claude-opus-5-5", false},
		{"Grok for a Claude session", cursorAuditChain, "", "claude-opus-5-5", true},
		{"Grok for a Grok session", cursorAuditChain, "", "grok-4.7", false},
		{"Claude for a Claude session", []AgentModel{{CLI: "cursor", Model: "grok-4.7-high"}, {CLI: "claude", Model: "opus"}}, "", "claude-sonnet-5-5", false},
		{"a session of no known family", gateway, "", "", true},
		{"no chain", nil, "", "claude-opus-5-5", false},
	} {
		if got := otherFamilyChain(c.chain, c.author, gatewayExcludeFor(c.session)); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// When the audit can't be had, the round's own prompt, and the round says
// why.
func TestReviewRoundFallsBack(t *testing.T) {
	ownPrompt := func(t *testing.T, a *apEnv, roundID int64, why string) {
		t.Helper()
		if len(a.jobs) != 1 {
			t.Fatalf("jobs = %+v", a.jobs)
		}
		var in AutopilotJob
		_ = json.Unmarshal(a.jobs[0].Input, &in)
		if in.Review != AutopilotReviewMark+in.ReviewOf+": "+ownReviewBrief(DefaultReviewPrompt, AutopilotLimits{}) {
			t.Errorf("review = %q", in.Review)
		}
		if ra, _ := a.roundAuditRow(roundID); !strings.Contains(ra.note, why) || ra.alt != "" {
			t.Errorf("round = %+v, want a note with %q", ra, why)
		}
	}
	// reviewed: the Sonnet reviewer's findings were typed in, not the
	// round's own prompt (WP-1206).
	reviewed := func(t *testing.T, a *apEnv, roundID int64, why string) {
		t.Helper()
		if len(a.jobs) != 1 {
			t.Fatalf("jobs = %+v", a.jobs)
		}
		var in AutopilotJob
		_ = json.Unmarshal(a.jobs[0].Input, &in)
		if !strings.Contains(in.Review, "Merge reads a nil map") || !strings.Contains(in.Review, "Claude Sonnet in a new Claude Code session") || strings.Contains(in.Review, DefaultReviewPrompt) {
			t.Errorf("review = %q", in.Review)
		}
		if ra, _ := a.roundAuditRow(roundID); !strings.Contains(ra.note, why) || ra.alt != "" {
			t.Errorf("round = %+v, want a note with %q", ra, why)
		}
	}
	start := func(a *apEnv) int64 {
		first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
		a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
		a.finish(first.Key)
		if _, ok := a.s.Forges.(*prForge); ok {
			a.grokPR(first.ID, 7, "aaa")
		} else {
			a.linkPR(first.ID, 7, "aaa")
		}
		a.stopWith("Shipped as #7.")
		return a.step().reviewID.Int64
	}

	t.Run("no runner that audits", func(t *testing.T) {
		a := reviewAPEnv(t)
		id := start(a)
		a.tick(61 * time.Second)
		ownPrompt(t, a, id, "no other model could audit it")
		if ra, _ := a.roundAuditRow(id); ra.id != 0 {
			t.Errorf("an audit without a runner: %+v", ra)
		}
	})
	// Every model of the round's own audit failed: no second audit of the
	// same models; Sonnet reviews it in a new session.
	t.Run("failed", func(t *testing.T) {
		a, box := auditAPEnv(t)
		id := start(a)
		a.tick(61 * time.Second)
		first, _ := a.roundAuditRow(id)
		a.endAudit(box, "failed")
		a.tick(time.Second)
		a.endReviewer(box, "done", "Merge reads a nil map")
		a.tick(time.Second)
		reviewed(t, a, id, "Another model's audit #")
		if ra, _ := a.roundAuditRow(id); ra.id != first.id {
			t.Errorf("a second audit of the same models: %d then %d", first.id, ra.id)
		}
	})
	// Behind other jobs on a runner that's there isn't a fault (WP-761,
	// WP-820): no other model is asked and the audit isn't cancelled. The
	// round waits for it up to its patience, and so does the merge.
	t.Run("queued behind others", func(t *testing.T) {
		a, box := auditAPEnv(t)
		id := start(a)
		a.tick(61 * time.Second)
		for range 5 {
			a.tick(10 * time.Minute)
		}
		ra, au := a.roundAuditRow(id)
		if a.step().reason != WaitAudit || len(a.jobs) != 0 || ra.alt != "" || ra.note != "" || au == nil || au.Status == AuditCancelled {
			t.Fatalf("gave up with a runner online: %+v %+v %+v %+v", a.step(), a.jobs, ra, au)
		}
		pr := &PullRequest{URL: "https://git.example/acme/app/pulls/7", Repo: "acme/app", Number: 7}
		if why, until, _ := a.s.reviewHold(a.ctx, pr); why == "" || until == nil || !until.After(a.clockNow()) {
			t.Errorf("the merge went on without the queued review: %q %v", why, until)
		}
		// And when a runner gets to it in time, the round uses it.
		a.endAudit(box, "done", "Merge reads a nil map")
		a.tick(time.Second)
		if len(a.jobs) != 1 {
			t.Fatalf("jobs = %+v", a.jobs)
		}
		var in AutopilotJob
		_ = json.Unmarshal(a.jobs[0].Input, &in)
		if !strings.Contains(in.Review, "Merge reads a nil map") {
			t.Errorf("review = %q", in.Review)
		}
	})
	// Past the round's patience Sonnet reviews it in a new session beside
	// the audit, which isn't cancelled (WP-1206). The reviewer ends first:
	// its findings are the round's, and the audit goes on as the pull
	// request's (TestRoundOutwaitsItsAudit).
	t.Run("queued past its patience", func(t *testing.T) {
		a, box := auditAPEnv(t)
		id := start(a)
		a.tick(61 * time.Second)
		if d := a.step().detail; !strings.Contains(d, "a Sonnet review in a new session starts beside it in 58m") {
			t.Errorf("the wait doesn't say when the Sonnet review starts: %q", d)
		}
		for range 7 {
			a.tick(10 * time.Minute)
		}
		if d := a.step().detail; len(a.jobs) != 0 || !strings.Contains(d, "and for Sonnet review #") || !strings.Contains(d, "whichever ends first") {
			t.Fatalf("not waiting for both: %q %+v", d, a.jobs)
		}
		// A runner takes the audit; the reviewer ends first.
		a.s.Wait()
		if w := a.poll(a.admin, box, RunnerPoll{}); w.Job == nil || agentJobOf(t, w.Job).CLI != GatewayCLI {
			t.Fatalf("no audit job: %+v", w.Job)
		}
		a.endReviewer(box, "done", "Merge reads a nil map")
		a.tick(time.Second)
		reviewed(t, a, id, roundAuditOutwaited)
		if _, au := a.roundAuditRow(id); au == nil || au.Status != AuditRunning {
			t.Errorf("its audit didn't go on: %+v", au)
		}
	})
	// The audit ends before the reviewer: its findings are the round's and
	// the reviewer is cancelled.
	t.Run("the audit ends first", func(t *testing.T) {
		a, box := auditAPEnv(t)
		id := start(a)
		a.tick(61 * time.Second)
		for range 7 {
			a.tick(10 * time.Minute)
		}
		rv, err := a.s.roundReviewer(a.ctx, id)
		if err != nil || rv == nil {
			t.Fatalf("no reviewer: %+v %v", rv, err)
		}
		a.endAudit(box, "done", "Merge reads a nil map")
		a.tick(time.Second)
		var in AutopilotJob
		if len(a.jobs) != 1 || json.Unmarshal(a.jobs[0].Input, &in) != nil || !strings.Contains(in.Review, "Merge reads a nil map") || strings.Contains(in.Review, "Claude Code session") {
			t.Fatalf("jobs = %+v", a.jobs)
		}
		if ra, _ := a.roundAuditRow(id); ra.note != "" {
			t.Errorf("round = %+v", ra)
		}
		if rv, _ = a.s.roundReviewer(a.ctx, id); rv.Status != AuditCancelled {
			t.Errorf("the reviewer wasn't cancelled: %s", rv.Status)
		}
	})
	// The round ends while both are going (skipped here): the reviewer is
	// cancelled, and the audit goes on as the pull request's.
	t.Run("the round ends first", func(t *testing.T) {
		a, _ := auditAPEnv(t)
		id := start(a)
		a.tick(61 * time.Second)
		for range 7 {
			a.tick(10 * time.Minute)
		}
		a.s.setRoundNow(a.ctx, id, []string{RoundPending}, RoundSkipped, "")
		a.s.sweepRounds(a.ctx)
		if rv, _ := a.s.roundReviewer(a.ctx, id); rv == nil || rv.Status != AuditCancelled {
			t.Errorf("the reviewer wasn't cancelled: %+v", rv)
		}
		if _, au := a.roundAuditRow(id); au == nil || au.Status == AuditCancelled {
			t.Errorf("the audit was cancelled: %+v", au)
		}
	})
	// Recent audits like it took longer than the round's patience: the
	// reviewer starts at once rather than after a wait that wouldn't help;
	// when they ended well within it, the round waits.
	t.Run("started early", func(t *testing.T) {
		for _, c := range []struct {
			took time.Duration
			now  bool
		}{{90 * time.Minute, true}, {30 * time.Minute, false}} {
			a, _ := auditAPEnv(t)
			at := a.clockNow().UnixMilli()
			for i := range reviewerMinRuns + 2 {
				end := at - int64(i)*int64(time.Hour/time.Millisecond)
				if _, err := a.s.db.W.Exec(`INSERT INTO runner_jobs(owner_id, kind, title, cli, input, state, timeout, source, result, created_at, started_at, finished_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
					a.admin.User.ID, JobAudit, "an audit", GatewayCLI, "{}", "done", 3600, "audit", "{}", end-c.took.Milliseconds(), end-c.took.Milliseconds(), end); err != nil {
					t.Fatal(err)
				}
			}
			id := start(a)
			a.tick(61 * time.Second)
			rv, err := a.s.roundReviewer(a.ctx, id)
			if err != nil || (rv != nil) != c.now || len(a.jobs) != 0 {
				t.Errorf("audits taking %s: reviewer %+v (%v), jobs %+v", c.took, rv, err, a.jobs)
			}
			if d := a.step().detail; !c.now && !strings.Contains(d, "10 of 10 recent runs") {
				t.Errorf("the wait doesn't give the odds: %q", d)
			}
		}
	})
	// WP-1306: audits the gateway refused (no model at the bar) say nothing
	// of how long one that runs takes: they don't lower the odds, so the
	// round still waits for an audit that likely lands. Other quick failures
	// (a broken runner, a missing CLI) count like any failure: every attempt
	// failing is a reason to start the reviewer, not to wait (audit #1039).
	for _, c := range []struct {
		name, result string
		counts       bool
	}{
		{"refusals don't count", `{"gateway_error":"no_model_at_bar"}`, false},
		{"other quick failures count", `{"gateway_error":"server"}`, true},
		{"quick failures with no gateway answer count", "{}", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, _ := auditAPEnv(t)
			at := a.clockNow().UnixMilli()
			add := func(i int, state string, took time.Duration, result string) {
				end := at - int64(i)*int64(time.Hour/time.Millisecond)
				if _, err := a.s.db.W.Exec(`INSERT INTO runner_jobs(owner_id, kind, title, cli, input, state, timeout, source, result, created_at, started_at, finished_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
					a.admin.User.ID, JobAudit, "an audit", GatewayCLI, "{}", state, 3600, "audit", result, end-took.Milliseconds(), end-took.Milliseconds(), end); err != nil {
					t.Fatal(err)
				}
			}
			for i := range reviewerMinRuns + 2 {
				add(i, "done", 30*time.Minute, "{}")
			}
			for i := range 20 {
				add(i, "failed", 20*time.Second, c.result)
			}
			id := start(a)
			a.tick(61 * time.Second)
			rv, _ := a.s.roundReviewer(a.ctx, id)
			if d := a.step().detail; (rv != nil) != c.counts || (!c.counts && !strings.Contains(d, "10 of 10 recent runs")) {
				t.Errorf("reviewer %+v; step %q", rv, d)
			}
		})
	}
	// A reviewer started early that fails leaves the round waiting for its
	// audit up to the hour, not to the session at once.
	t.Run("an early reviewer fails", func(t *testing.T) {
		a, box := auditAPEnv(t)
		at := a.clockNow().UnixMilli()
		for i := range reviewerMinRuns {
			end := at - int64(i)*int64(time.Hour/time.Millisecond)
			if _, err := a.s.db.W.Exec(`INSERT INTO runner_jobs(owner_id, kind, title, cli, input, state, timeout, source, result, created_at, started_at, finished_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
				a.admin.User.ID, JobAudit, "an audit", GatewayCLI, "{}", "done", 3600, "audit", "{}", end-(90*time.Minute).Milliseconds(), end-(90*time.Minute).Milliseconds(), end); err != nil {
				t.Fatal(err)
			}
		}
		id := start(a)
		a.tick(61 * time.Second)
		a.s.Wait()
		audit := a.poll(a.admin, box, RunnerPoll{}).Job
		if audit == nil || agentJobOf(t, audit).CLI != GatewayCLI {
			t.Fatalf("no audit job: %+v", audit)
		}
		a.endReviewer(box, "failed")
		a.tick(time.Second)
		if ra, _ := a.roundAuditRow(id); len(a.jobs) != 0 || ra.note != "" || !strings.Contains(a.step().detail, "if the audit hasn't ended") {
			t.Fatalf("gave up on the audit: %+v %q %+v", ra, a.step().detail, a.jobs)
		}
		a.finishAudit(audit, "done", "Merge reads a nil map")
		a.tick(time.Second)
		var in AutopilotJob
		if len(a.jobs) != 1 || json.Unmarshal(a.jobs[0].Input, &in) != nil || !strings.Contains(in.Review, "Merge reads a nil map") {
			t.Fatalf("jobs = %+v", a.jobs)
		}
	})
	// The queue's wait doesn't use up the hold: it lasts while a runner has
	// the audit, and from when that ends to the round's start.
	t.Run("the hold outlasts the queue", func(t *testing.T) {
		a, box := auditAPEnv(t)
		start(a)
		a.tick(61 * time.Second)
		a.tick(reviewAuditPatience - 5*time.Minute)
		if _, err := a.s.db.W.Exec(`UPDATE runners SET last_seen_at = ?`, a.clockNow().UnixMilli()); err != nil {
			t.Fatal(err)
		}
		pr := &PullRequest{URL: "https://git.example/acme/app/pulls/7", Repo: "acme/app", Number: 7}
		a.s.Wait()
		w := a.poll(a.admin, box, RunnerPoll{})
		if w.Job == nil || w.Job.Kind != JobAudit {
			t.Fatalf("no audit job: %+v", w.Job)
		}
		if why, _, _ := a.s.reviewHold(a.ctx, pr); why == "" {
			t.Error("the hold lapsed when a runner took the audit")
		}
		res := auditResult("Read it.", map[string]any{"title": "Merge reads a nil map", "path": "internal/merge.go", "line": 10, "severity": "major",
			"problem": "A problem", "confirm": "Merge twice"})
		if _, err := a.s.FinishRunnerJob(a.ctx, a.admin, w.Job.ID, RunnerFinish{State: "done", Result: []byte(mustJSON(res))}); err != nil {
			t.Fatal(err)
		}
		a.s.Wait()
		a.s.Wait()
		if why, _, _ := a.s.reviewHold(a.ctx, pr); why == "" {
			t.Error("the hold lapsed between the audit and the round")
		}
	})
	// A runner took it after a long queue and the model was busy: between
	// tries isn't "no runner took it", and neither is a job that has just
	// ended.
	t.Run("between attempts", func(t *testing.T) {
		a, box := auditAPEnv(t)
		id := start(a)
		a.tick(61 * time.Second)
		a.tick(reviewAuditQueueWait + time.Minute)
		a.s.Wait()
		w := a.poll(a.admin, box, RunnerPoll{})
		if w.Job == nil || w.Job.Kind != JobAudit {
			t.Fatalf("no audit job: %+v", w.Job)
		}
		if _, err := a.s.FinishRunnerJob(a.ctx, a.admin, w.Job.ID, RunnerFinish{State: "failed", Error: "429 too many requests"}); err != nil {
			t.Fatal(err)
		}
		a.s.Wait()
		a.s.Wait()
		if _, au := a.roundAuditRow(id); au == nil || au.Status != AuditQueued || au.JobID != nil || len(au.Attempts) == 0 {
			t.Fatalf("not between attempts: %+v", au)
		}
		a.tick(time.Minute)
		if a.step().reason != WaitAudit || len(a.jobs) != 0 {
			t.Fatalf("gave up between attempts: %+v %+v", a.step(), a.jobs)
		}
		ended := &RunnerJobInfo{State: "failed"}
		if why := a.s.auditQueueFault(a.ctx, &CodeAudit{ID: 9}, ended, 0, reviewAuditQueueWait.Milliseconds()+1, "another model's"); why != "" {
			t.Errorf("a job that has just ended: %q", why)
		}
		a.tick(reviewAuditStalled + 20*time.Minute)
		a.endReviewer(box, "done", "Merge reads a nil map")
		a.tick(time.Second)
		reviewed(t, a, id, "between attempts")
		if _, au := a.roundAuditRow(id); au == nil || au.Status != AuditCancelled {
			t.Errorf("its audit wasn't cancelled: %+v", au)
		}
	})
	t.Run("not taken", func(t *testing.T) {
		a, _ := auditAPEnv(t)
		id := start(a)
		a.tick(61 * time.Second)
		if len(a.jobs) != 0 {
			t.Fatalf("jobs = %+v", a.jobs)
		}
		// Only the session's own runner goes on polling: none that audits.
		a.s.Wait()
		a.setClock(a.clockNow().Add(reviewAuditQueueWait + time.Minute))
		if _, err := a.s.db.W.Exec(`UPDATE runners SET last_seen_at = ? WHERE name = 'box'`, a.clockNow().UnixMilli()); err != nil {
			t.Fatal(err)
		}
		a.s.RunAutopilot(a.ctx)
		// The Sonnet reviewer has no runner either, and none was there since
		// the round began: that look, or the next once its job is queued,
		// leaves the review to the session.
		if rv, _ := a.s.roundReviewer(a.ctx, id); rv == nil {
			t.Fatal("no reviewer was asked")
		}
		if len(a.jobs) == 0 {
			a.s.Wait()
			a.setClock(a.clockNow().Add(reviewAuditRecheck + time.Second))
			if _, err := a.s.db.W.Exec(`UPDATE runners SET last_seen_at = ? WHERE name = 'box'`, a.clockNow().UnixMilli()); err != nil {
				t.Fatal(err)
			}
			a.s.RunAutopilot(a.ctx)
		}
		ownPrompt(t, a, id, "no runner took another model's")
		if rv, _ := a.s.roundReviewer(a.ctx, id); rv == nil || rv.Status != AuditCancelled {
			t.Errorf("its reviewer wasn't cancelled: %+v", rv)
		}
		if _, au := a.roundAuditRow(id); au == nil || au.Status != AuditCancelled {
			t.Errorf("its audit wasn't cancelled: %+v", au)
		}
		// Final for the round: the next look doesn't ask again.
		if _, err := a.s.db.W.Exec(`UPDATE work_log SET state = 'pending' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
		st := a.step()
		plan, err := a.s.planReview(a.ctx, st, a.round(id), a.clockNow().UnixMilli())
		if err != nil || plan.wait != "" || plan.prompt != ownReviewBrief(DefaultReviewPrompt, AutopilotLimits{}) {
			t.Errorf("plan = %+v, %v", plan, err)
		}
	})
}

// An audit that asked other models than a round asks today (the pull
// request's own, here one a person asked of Cursor; or a round's Grok audit
// from before WP-871) that fails is followed by the round's own, once, and
// the round says why.
func TestReviewRoundAfterACursorAudit(t *testing.T) {
	a, box := auditAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.grokPR(first.ID, 7, "aaa")
	var prID, forge int64
	if err := a.s.db.R.QueryRow(`SELECT id, forge_id FROM pull_requests WHERE number = 7`).Scan(&prID, &forge); err != nil {
		t.Fatal(err)
	}
	res, err := a.s.db.W.Exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, pr_id, pr_number, ref, status, model, models, title, owner_id, created_at)
		VALUES (?, ?, 'acme/app', 'pr', ?, 7, 'aaa', 'queued', '', '[{"cli":"cursor","model":"grok-4.7-high"}]', 'acme/app#7', ?, ?)`, a.p.ID, forge, prID, a.admin.User.ID, a.clockNow().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	grok, _ := res.LastInsertId()
	a.stopWith("Shipped as #7.")
	id := a.step().reviewID.Int64
	a.askedAsRound(grok)
	a.tick(61 * time.Second)
	if ra, _ := a.roundAuditRow(id); ra.id != grok || ra.alt != "" {
		t.Fatalf("the pull request's audit isn't the round's: %+v", ra)
	}
	if st := a.step(); st.reason != WaitAudit || !strings.Contains(st.detail, "Grok's audit") {
		t.Fatalf("step = %+v", st)
	}
	if _, err := a.s.db.W.Exec(`UPDATE code_audits SET status = 'limited', error = 'the plan is out of usage' WHERE id = ?`, grok); err != nil {
		t.Fatal(err)
	}
	a.tick(reviewAuditRecheck + time.Second)
	ra, au := a.roundAuditRow(id)
	if ra.id == grok || ra.note != "" || !strings.Contains(ra.alt, "Grok's audit #") || !strings.Contains(ra.alt, "the Cursor plan's pool") ||
		au == nil || au.Purpose != AuditReview || au.Models[len(au.Models)-1] != "gateway/audit" {
		t.Fatalf("round = %+v, audit = %+v", ra, au)
	}
	// That one failing too is the end of it: the session reviews.
	a.endAudit(box, "failed")
	a.tick(time.Second)
	if ra2, _ := a.roundAuditRow(id); ra2.id != ra.id || !strings.Contains(ra2.note, "Another model's audit #") {
		t.Fatalf("round = %+v", ra2)
	}
}

// todaysDefaultModels is a stored audit's models when nobody named any, on a
// Waypoint with a gateway (WP-1725).
const todaysDefaultModels = `[{"cli":"cursor","model":"grok-4.7-high"},{"cli":"cursor","model":"grok-4.7-medium"},{"cli":"gateway","model":"audit"}]`

// Whether a pull request's own audit that failed is followed by the round's
// own: not when it asked every model the round's would, at the round's bar;
// it is when a person named other models beside the gateway (Grok wasn't
// asked).
func TestReviewRoundAsksItsOwnAuditAfterAnother(t *testing.T) {
	for name, c := range map[string]struct {
		models, weakest string
		again           bool
	}{
		"today's default at the round's bar": {todaysDefaultModels, reviewFloorBar, false},
		"the gateway beside a named model":   {`[{"cli":"claude","model":"sonnet"},{"cli":"gateway","model":"audit"}]`, reviewFloorBar, true},
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := auditAPEnv(t)
			first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
			a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
			a.finish(first.Key)
			a.grokPR(first.ID, 7, "aaa")
			var prID, forge int64
			if err := a.s.db.R.QueryRow(`SELECT id, forge_id FROM pull_requests WHERE number = 7`).Scan(&prID, &forge); err != nil {
				t.Fatal(err)
			}
			res, err := a.s.db.W.Exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, pr_id, pr_number, ref, status, model, models, author_model, title, owner_id, created_at, weakest)
				VALUES (?, ?, 'acme/app', 'pr', ?, 7, 'aaa', 'queued', '', ?, 'anthropic/claude-code', 'acme/app#7', ?, ?, ?)`, a.p.ID, forge, prID, c.models, a.admin.User.ID, a.clockNow().UnixMilli(), c.weakest)
			if err != nil {
				t.Fatal(err)
			}
			theirs, _ := res.LastInsertId()
			a.stopWith("Shipped as #7.")
			id := a.step().reviewID.Int64
			a.askedAsRound(theirs)
			a.tick(61 * time.Second)
			if ra, _ := a.roundAuditRow(id); ra.id != theirs {
				t.Fatalf("the pull request's audit isn't the round's: %+v", ra)
			}
			if _, err := a.s.db.W.Exec(`UPDATE code_audits SET status = 'failed', error = 'no model could take the task' WHERE id = ?`, theirs); err != nil {
				t.Fatal(err)
			}
			a.tick(reviewAuditRecheck + time.Second)
			ra, au := a.roundAuditRow(id)
			if again := ra.id != theirs; again != c.again {
				t.Fatalf("the round's own audit asked: %v, want %v (round %+v)", again, c.again, ra)
			}
			if c.again && (au == nil || au.Purpose != AuditReview || strings.Join(au.Models, " ") != "cursor/grok-4.7-high cursor/grok-4.7-medium gateway/audit") {
				t.Fatalf("the round's own audit = %+v", au)
			}
		})
	}
}

// With a gateway and a runner with Cursor online, a round's audit is Grok's
// through Cursor first, and the gateway's when the Cursor plan is out of usage
// (WP-1725).
func TestReviewRoundAsksCursorFirst(t *testing.T) {
	a, box := auditAPEnv(t)
	cursor := a.runner(a.admin, "cursorbox", func(r *RunnerRegistration) {
		r.CLIs = []RunnerCLI{{Name: "cursor"}}
		r.Features = []string{FeatureConfine, FeatureAudit}
	})
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.grokPR(first.ID, 7, "aaa")
	a.stopWith("Shipped as #7.")
	id := a.step().reviewID.Int64
	a.tick(61 * time.Second)
	a.s.Wait()
	if st := a.step(); st.reason != WaitAudit || !strings.Contains(st.detail, "Grok's audit") {
		t.Fatalf("step = %+v", st)
	}
	for _, want := range []string{"grok-4.7-high", "grok-4.7-medium"} {
		w := a.poll(a.admin, cursor, RunnerPoll{})
		if w.Job == nil || w.Job.Kind != JobAudit {
			t.Fatalf("no audit job for Cursor (%s): %+v", want, w.Job)
		}
		if job := agentJobOf(t, w.Job); job.CLI != "cursor" || job.Model != want {
			t.Fatalf("the round's job = %s/%s, want cursor/%s", job.CLI, job.Model, want)
		}
		if _, err := a.s.FinishRunnerJob(a.ctx, a.admin, w.Job.ID, RunnerFinish{State: "failed", Error: "You've hit your usage limit"}); err != nil {
			t.Fatal(err)
		}
		a.s.Wait()
		a.s.Wait()
	}
	w := a.poll(a.admin, box, RunnerPoll{})
	if w.Job == nil || agentJobOf(t, w.Job).CLI != GatewayCLI {
		t.Fatalf("the gateway wasn't asked after Cursor: %+v", w.Job)
	}
	if _, au := a.roundAuditRow(id); au.Status != AuditRunning || len(au.Attempts) != 2 || au.Attempts[0].Outcome != "spent" {
		t.Fatalf("audit = %s %+v", au.Status, au.Attempts)
	}
}

// The pull request's own audit by the gateway failing is the gateway having
// failed: the round asks it no second time; the Sonnet reviewer does.
func TestReviewRoundAfterThePRsGatewayAudit(t *testing.T) {
	a, box := auditAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.grokPR(first.ID, 7, "aaa")
	var prID, forge int64
	if err := a.s.db.R.QueryRow(`SELECT id, forge_id FROM pull_requests WHERE number = 7`).Scan(&prID, &forge); err != nil {
		t.Fatal(err)
	}
	res, err := a.s.db.W.Exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, pr_id, pr_number, ref, status, model, models, author_model, title, owner_id, created_at, weakest)
		VALUES (?, ?, 'acme/app', 'pr', ?, 7, 'aaa', 'queued', '', ?, 'anthropic/claude-code', 'acme/app#7', ?, ?, 'deepseek-v4.1-flash')`, a.p.ID, forge, prID, todaysDefaultModels, a.admin.User.ID, a.clockNow().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	theirs, _ := res.LastInsertId()
	a.stopWith("Shipped as #7.")
	id := a.step().reviewID.Int64
	a.askedAsRound(theirs)
	a.tick(61 * time.Second)
	if ra, _ := a.roundAuditRow(id); ra.id != theirs {
		t.Fatalf("the pull request's audit isn't the round's: %+v", ra)
	}
	if _, err := a.s.db.W.Exec(`UPDATE code_audits SET status = 'failed', error = 'The gateway: no model could take the task' WHERE id = ?`, theirs); err != nil {
		t.Fatal(err)
	}
	a.tick(reviewAuditRecheck + time.Second)
	if ra, _ := a.roundAuditRow(id); ra.id != theirs || ra.alt != "" || !strings.Contains(ra.note, "audit #") {
		t.Fatalf("round = %+v", ra)
	}
	// The Sonnet reviewer, not the gateway again; when it fails too, the
	// session reviews.
	a.endReviewer(box, "failed")
	if w := a.poll(a.admin, box, RunnerPoll{}); w.Job != nil {
		t.Fatalf("the gateway was asked again: %+v", agentJobOf(t, w.Job))
	}
	a.tick(time.Second)
	var in AutopilotJob
	if len(a.jobs) != 1 || json.Unmarshal(a.jobs[0].Input, &in) != nil || !strings.HasSuffix(in.Review, ownReviewBrief(DefaultReviewPrompt, AutopilotLimits{})) {
		t.Fatalf("jobs = %+v", a.jobs)
	}
}

// askedAsRound makes the audits' prompts ask what the current round's
// audit would (roundAsks), as a pull request's own audit must to stand in
// for it.
func (a *apEnv) askedAsRound(audits ...int64) {
	a.t.Helper()
	st := a.step()
	_, lim, err := a.s.autopilotFor(a.ctx, a.s.db.R, st.userID, st.projectID)
	if err != nil {
		a.t.Fatal(err)
	}
	asks := roundAsks(lim.reviewText(), a.s.roundContext(a.ctx, a.round(st.reviewID.Int64)))
	if len(asks) != 2 {
		a.t.Fatalf("the round asks %q", asks)
	}
	for _, id := range audits {
		if _, err := a.s.db.W.Exec(`UPDATE code_audits SET prompt = ? WHERE id = ?`, "Audit it.\n\n"+strings.Join(asks, "")+"What to look for: all.", id); err != nil {
			a.t.Fatal(err)
		}
	}
}

// The pull request's own audit at its head is used rather than a second
// one, whichever way it ran, when its reviewer isn't of the session's
// family and it asked what the round's audit would; one at an older head,
// or one that didn't ask that, isn't.
func TestReviewRoundReusesPRAudit(t *testing.T) {
	a, _ := auditAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.finish(first.Key)
	a.linkPR(first.ID, 7, "aaa")
	var prID, forge int64
	if err := a.s.db.R.QueryRow(`SELECT id, forge_id FROM pull_requests WHERE number = 7`).Scan(&prID, &forge); err != nil {
		t.Fatal(err)
	}
	insert := func(ref, model, status, models, author string) int64 {
		res, err := a.s.db.W.Exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, pr_id, pr_number, ref, status, model, models, author_model, title, owner_id, created_at, weakest)
			VALUES (?, ?, 'acme/app', 'pr', ?, 7, ?, ?, ?, ?, ?, 'acme/app#7', ?, 0, 'deepseek-v4.1-flash')`, a.p.ID, forge, prID, ref, status, model, models, author, a.admin.User.ID)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	const gateway = `[{"cli":"gateway","model":"audit"}]`
	insert("old", "gateway/deepseek/deepseek-v4.1-flash", AuditDone, gateway, "claude-opus-5-5")
	insert("aaa", "cursor/grok-4.7-high", AuditDone, "[]", "")
	want := insert("aaa", "gateway/deepseek/deepseek-v4.1-flash", AuditDone, gateway, "claude-opus-5-5")
	// Newer, and not for this session: a model of its own family answered
	// one, and the other may still be given to one.
	insert("aaa", "gateway/anthropic/claude-sonnet-5-5", AuditDone, gateway, "")
	insert("aaa", "", AuditQueued, gateway, "")
	a.stopWith("Shipped as #7.")
	st := a.step()
	a.askedAsRound(want)
	// Newer, and like it in every way but its prompt: it didn't ask the
	// brief, the task or the earlier rounds the round's audit would.
	insert("aaa", "gateway/deepseek/deepseek-v4.1-flash", AuditDone, gateway, "claude-opus-5-5")
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET model = 'claude-opus-5-5' WHERE id = ?`, st.sessionID); err != nil {
		t.Fatal(err)
	}
	roundID := st.reviewID.Int64
	a.tick(61 * time.Second)
	if ra, _ := a.roundAuditRow(roundID); ra.id != want {
		t.Fatalf("round audit = %d, want %d", ra.id, want)
	}
	// That audit found nothing: the round ends without the session.
	if r := a.round(roundID); r.state != RoundDone {
		t.Errorf("round = %+v", r)
	}
}

// A reused audit's findings are shared out: those the pull request's
// babysitter was given stay with it, the rest go to the round, and the
// babysitter leaves them to the round while it is live.
func TestReviewRoundSharesPRAuditWithBabysitter(t *testing.T) {
	a, _ := auditAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.linkPR(first.ID, 7, "aaa")
	var prID, forge int64
	if err := a.s.db.R.QueryRow(`SELECT id, forge_id FROM pull_requests WHERE number = 7`).Scan(&prID, &forge); err != nil {
		t.Fatal(err)
	}
	res, err := a.s.db.W.Exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, pr_id, pr_number, ref, status, model, models, title, owner_id, created_at)
		VALUES (?, ?, 'acme/app', 'pr', ?, 7, 'aaa', 'done', 'cursor/grok-4.7-high', '[]', 'acme/app#7', ?, 0)`, a.p.ID, forge, prID, a.admin.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	audit, _ := res.LastInsertId()
	for i, title := range []string{"Babysitter has it", "Left for the round", "Still for the round"} {
		if _, err := a.s.db.W.Exec(`INSERT INTO code_audit_findings(audit_id, ordinal, title, severity, problem) VALUES (?, ?, ?, 'high', 'p')`, audit, i+1, title); err != nil {
			t.Fatal(err)
		}
	}
	given := mustJSON([]BabysitRound{{N: 1, Findings: []string{itoa64(audit) + ".1"}}})
	if _, err := a.s.db.W.Exec(`INSERT INTO babysits(pr_id, round, owner_id, rounds, created_at, updated_at) VALUES (?, 1, ?, ?, 0, 0)`, prID, a.admin.User.ID, given); err != nil {
		t.Fatal(err)
	}
	a.stopWith("Shipped as #7.")
	roundID := a.step().reviewID.Int64
	a.askedAsRound(audit)
	a.tick(61 * time.Second)
	if ra, _ := a.roundAuditRow(roundID); ra.id != audit {
		t.Fatalf("round audit = %+v, want %d", ra, audit)
	}
	if len(a.jobs) != 1 {
		t.Fatalf("jobs = %+v", a.jobs)
	}
	var in AutopilotJob
	_ = json.Unmarshal(a.jobs[0].Input, &in)
	if strings.Contains(in.Review, "Babysitter has it") || !strings.Contains(in.Review, "Left for the round") {
		t.Errorf("prompt:\n%s", in.Review)
	}
	b, err := a.s.latestBabysit(a.ctx, a.s.db.R, prID)
	if err != nil || b == nil {
		t.Fatal(b, err)
	}
	if got := a.s.auditRoundWork(a.ctx, b, prID); len(got) != 0 {
		t.Errorf("the babysitter takes the round's findings: %v", got)
	}
	a.s.setRoundNow(a.ctx, roundID, []string{RoundPending, RoundRunning}, RoundDone, "")
	if got := a.s.auditRoundWork(a.ctx, b, prID); len(got) != 2 {
		t.Errorf("after the round, the babysitter's work = %v", got)
	}
}

// A high-stakes change's round holds its audit to the trusted bar (WP-916):
// the pull request's own audit without that bar isn't reused, the job and the
// gateway's task carry it, and when the gateway has no model at the bar
// Sonnet reviews it in a new session, with no other gateway audit asked.
func TestReviewRoundHighStakesBar(t *testing.T) {
	a, box := auditAPEnv(t)
	gw := a.s.LLM.(*taskLLM)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.grokPR(first.ID, 7, "aaa")
	var prID, forge int64
	if err := a.s.db.R.QueryRow(`SELECT id, forge_id FROM pull_requests WHERE number = 7`).Scan(&prID, &forge); err != nil {
		t.Fatal(err)
	}
	// Another family's audit at the head, by whichever model the purpose had.
	res, err := a.s.db.W.Exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, pr_id, pr_number, ref, status, model, models, author_model, title, owner_id, created_at)
		VALUES (?, ?, 'acme/app', 'pr', ?, 7, 'aaa', 'done', 'gateway/deepseek/deepseek-v4.1-flash', '[{"cli":"gateway","model":"audit"}]', 'claude-opus-5-5', 'acme/app#7', ?, 0)`,
		a.p.ID, forge, prID, a.admin.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	unbarred, _ := res.LastInsertId()
	a.stopWith("Shipped as #7.")
	st := a.step()
	roundID := st.reviewID.Int64
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET model = 'claude-opus-5-5' WHERE id = ?`, st.sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.W.Exec(`INSERT INTO review_triage(round_id, issue_id, project_id, repo, head, rule, action, features, created_at)
		VALUES (?, ?, ?, 'acme/app', 'aaa', 'floor:sensitive_path', 'review', '{"files":1,"added":3,"sensitive":["internal/auth/login.go"]}', 0)`,
		roundID, first.ID, a.p.ID); err != nil {
		t.Fatal(err)
	}
	a.tick(61 * time.Second)
	ra, au := a.roundAuditRow(roundID)
	if ra.id == 0 || ra.id == unbarred || au.Weakest != trustedAuditBar {
		t.Fatalf("round audit = %+v, %+v", ra, au)
	}
	w := a.poll(a.admin, box, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobAudit {
		t.Fatalf("no audit job: %+v", w.Job)
	}
	job := agentJobOf(t, w.Job)
	if job.CLI != GatewayCLI || job.GatewayWeakest != trustedAuditBar || strings.Join(job.GatewayExclude, ",") != "*claude*" {
		t.Fatalf("job = %s/%s weakest %q exclude %v", job.CLI, job.Model, job.GatewayWeakest, job.GatewayExclude)
	}
	v, err := a.s.StartGatewayTask(a.ctx, a.admin, w.Job.ID, "Audit this.", strings.NewReader("tar"), 3)
	if err != nil || v.ID == "" {
		t.Fatalf("task = %+v %v", v, err)
	}
	if _, spec := gw.sent(); spec.Weakest != trustedAuditBar {
		t.Fatalf("spec = %+v", spec)
	}
	// Nothing outside the author's family is that good: the gateway says
	// so, and the session's own reviewer reviews.
	result := map[string]any{"gateway_error": gatewayNoModelAtBar, "gateway_task": v.ID}
	if _, err := a.s.FinishRunnerJob(a.ctx, a.admin, w.Job.ID, RunnerFinish{State: "failed", Error: "The gateway has no model at the bar", Result: []byte(mustJSON(result))}); err != nil {
		t.Fatal(err)
	}
	a.s.Wait()
	a.s.Wait()
	if w := a.poll(a.admin, box, RunnerPoll{}); w.Job != nil {
		t.Fatalf("another audit was asked: %+v", agentJobOf(t, w.Job))
	}
	a.tick(time.Second)
	ra, au = a.roundAuditRow(roundID)
	if au.Status != AuditFailed || ra.alt != "" || ra.note == "" {
		t.Fatalf("round = %+v, audit = %s %+v", ra, au.Status, au.Attempts)
	}
	// Sonnet, the bar itself, reviews it in a new session; its job asks the
	// gateway nothing.
	a.endReviewer(box, "done", "Login skips the lockout")
	a.tick(time.Second)
	var in AutopilotJob
	if len(a.jobs) != 1 || json.Unmarshal(a.jobs[0].Input, &in) != nil || !strings.Contains(in.Review, "Login skips the lockout") {
		t.Fatalf("the Sonnet review's findings weren't typed in: %+v", a.jobs)
	}
	if rv, _ := a.s.roundReviewer(a.ctx, roundID); rv == nil || rv.Weakest != "" {
		t.Errorf("reviewer = %+v", rv)
	}
}

// A round of an ordinary change holds its audit to the floor (WP-1440), and reuses the
// pull request's own.
func TestReviewRoundOrdinaryFloor(t *testing.T) {
	a, _ := auditAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.finish(first.Key)
	a.linkPR(first.ID, 7, "aaa")
	var prID, forge int64
	if err := a.s.db.R.QueryRow(`SELECT id, forge_id FROM pull_requests WHERE number = 7`).Scan(&prID, &forge); err != nil {
		t.Fatal(err)
	}
	res, err := a.s.db.W.Exec(`INSERT INTO code_audits(project_id, forge_id, repo, target, pr_id, pr_number, ref, status, model, models, author_model, title, owner_id, created_at, weakest)
		VALUES (?, ?, 'acme/app', 'pr', ?, 7, 'aaa', 'done', 'gateway/deepseek/deepseek-v4.1-flash', '[{"cli":"gateway","model":"audit"}]', 'claude-opus-5-5', 'acme/app#7', ?, 0, 'deepseek-v4.1-flash')`,
		a.p.ID, forge, prID, a.admin.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := res.LastInsertId()
	a.stopWith("Shipped as #7.")
	st := a.step()
	a.askedAsRound(want)
	if _, err := a.s.db.W.Exec(`UPDATE agent_sessions SET model = 'claude-opus-5-5' WHERE id = ?`, st.sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.W.Exec(`INSERT INTO review_triage(round_id, issue_id, project_id, rule, action, features, created_at)
		VALUES (?, ?, ?, 'floor:large', 'review', '{"files":4,"added":300,"provenance":"medium"}', 0)`, st.reviewID.Int64, first.ID, a.p.ID); err != nil {
		t.Fatal(err)
	}
	a.tick(61 * time.Second)
	if ra, _ := a.roundAuditRow(st.reviewID.Int64); ra.id != want {
		t.Fatalf("round audit = %d, want the pull request's %d", ra.id, want)
	}
}

// What makes a change high-stakes, from triage's facts (WP-916).
func TestHighStakes(t *testing.T) {
	for _, c := range []struct {
		f    TriageFeatures
		want string
	}{
		{TriageFeatures{Files: 30, Added: 900}, ""},
		{TriageFeatures{Provenance: RiskMedium}, ""},
		{TriageFeatures{Provenance: RiskHigh}, "risk is high"},
		{TriageFeatures{Sensitive: []string{"internal/db/migrations/0001.sql"}}, "sensitive paths (internal/db/migrations/0001.sql)"},
		{TriageFeatures{RiskyFiles: 2}, "2 files with past bug fixes"},
	} {
		got := highStakes(&c.f)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("highStakes(%+v) = %q, want %q", c.f, got, c.want)
		}
	}
}

// No reviewing model answered, but the round's Haiku checks confirmed
// problems before the reviewer failed: the session's own-review prompt
// lists them, the work log holds them once as the session's own review (so
// a report of nothing is refused), and the audit keeps them (WP-1320).
func TestReviewRoundPanelWithoutReviewer(t *testing.T) {
	a, box := auditAPEnv(t)
	first := a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "First", Agent: "claude", Priority: "urgent"})
	a.issue(a.admin, IssueInput{Project: a.p.Key, Title: "Second", Agent: "claude"})
	a.finish(first.Key)
	a.grokPR(first.ID, 7, "aaa")
	a.stopWith("Shipped as #7.")
	id := a.step().reviewID.Int64
	a.tick(61 * time.Second)
	a.s.Wait()
	w := a.poll(a.admin, box, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobAudit {
		t.Fatalf("no audit job: %+v", w.Job)
	}
	if _, err := a.s.FinishRunnerJob(a.ctx, a.admin, w.Job.ID, RunnerFinish{State: "failed", Error: "502 Bad Gateway"}); err != nil {
		t.Fatal(err)
	}
	a.s.Wait()
	a.s.Wait()
	a.tick(time.Second)
	// The Sonnet reviewer fails with its panel's two confirmed problems.
	a.s.Wait()
	w = a.poll(a.admin, box, RunnerPoll{})
	if w.Job == nil || w.Job.Kind != JobAudit {
		t.Fatalf("no reviewer job: %+v", w.Job)
	}
	result := map[string]any{"helpers": []any{map[string]any{"check": "goals", "state": "done", "from": "aaaaaaaa", "to": "bbbbbbbb", "leads": []any{
		map[string]any{"id": "goals.1", "title": "First", "severity": "high", "problem": "p1", "confirmed": true, "read_title": "Reads a nil map", "read_path": "a.go", "read_line": 4, "read_severity": "high", "read_problem": "nil map write", "evidence": "a.go:4 writes m[k]"},
		map[string]any{"id": "goals.2", "title": "Second", "severity": "low", "problem": "p2", "confirmed": true, "read_path": "b.go", "read_line": 8, "evidence": "b.go:8"},
	}}}}
	if _, err := a.s.FinishRunnerJob(a.ctx, a.admin, w.Job.ID, RunnerFinish{State: "failed", Error: "claude exited with status 1", Result: []byte(mustJSON(result))}); err != nil {
		t.Fatal(err)
	}
	a.s.Wait()
	a.s.Wait()
	a.tick(time.Second)
	a.tick(time.Second)
	var in AutopilotJob
	if len(a.jobs) != 1 || json.Unmarshal(a.jobs[0].Input, &in) != nil {
		t.Fatalf("the session wasn't asked for its own review: %+v", a.jobs)
	}
	for _, want := range []string{"No reviewing model answered. Before it failed, Waypoint's Haiku checks confirmed these problems", "Reads a nil map (a.go:4, high)", "a.go:4 writes m[k]", "b.go:8"} {
		if !strings.Contains(in.Review, want) {
			t.Errorf("the session's prompt lacks %q: %q", want, in.Review)
		}
	}
	r := a.round(id)
	reviews, raised, err := a.s.ownReviewRaised(a.ctx, r.sessionID.Int64, r.issueID, id)
	if err != nil || reviews != 1 || raised != 2 {
		t.Errorf("the work log's own reviews = %d raising %d, %v; want 1 raising 2", reviews, raised, err)
	}
	// Planned again, it is written once.
	if _, err := a.s.planReview(a.ctx, a.step(), a.round(id), a.clockNow().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = a.s.db.R.QueryRow(`SELECT count(*) FROM work_log WHERE kind = ? AND reason = ?`, WorkOwnReview, ownReviewPanel).Scan(&n)
	if n != 1 {
		t.Errorf("the panel's problems were written %d times", n)
	}
	said, _, _ := a.s.ownReviewSaid(a.ctx, r.sessionID.Int64, r.issueID, id)
	if !strings.Contains(said, "Reads a nil map") {
		t.Errorf("the check of the session's report wouldn't read them: %q", said)
	}
}
