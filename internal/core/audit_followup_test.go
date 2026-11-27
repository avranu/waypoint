package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// auditJob is the runner job input of an audit.
func (be *bsEnv) auditJob(id int64) AgentJob {
	be.t.Helper()
	be.s.Wait()
	var job int64
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT coalesce(job_id, 0) FROM code_audits WHERE id = ?`, id).Scan(&job); err != nil || job == 0 {
		be.t.Fatalf("audit %d has no job: %v", id, err)
	}
	return be.jobInput(job)
}

// finishAuditWith ends the newest audit's job with a done result that also holds
// extra (what the runner says of its range).
func (be *bsEnv) finishAuditWith(box *Runner, extra map[string]any) {
	be.t.Helper()
	be.s.Wait()
	w := be.env.poll(be.admin, box, RunnerPoll{})
	if w.Job == nil {
		be.t.Fatal("no audit job was queued")
	}
	res := auditResult("Read it.")
	for k, v := range extra {
		res[k] = v
	}
	raw, _ := json.Marshal(res)
	if _, err := be.s.FinishRunnerJob(be.ctx, be.admin, w.Job.ID, RunnerFinish{State: "done", Result: raw}); err != nil {
		be.t.Fatal(err)
	}
	be.s.Wait()
}

func (be *bsEnv) auditMarks(id int64) (over, followed string) {
	be.t.Helper()
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT over_ref, followed_up FROM code_audits WHERE id = ?`, id).Scan(&over, &followed); err != nil {
		be.t.Fatal(err)
	}
	return over, followed
}

// The first head's audit reads the whole change; a later head's, with an
// earlier audit finished, asks the runner for what changed since it, and
// still carries the ledger.
func TestAuditFollowUpOnALaterHead(t *testing.T) {
	be := newBabysitEnv(t)
	box := be.auditBox()
	be.auditOn()
	be.ready(be.openMine())
	first := be.auditID()
	if in := be.auditJob(first); in.DiffOver != "" || in.OverPrompt != "" {
		t.Fatalf("the first head's audit is a follow-up: %+v", in)
	}
	if over, _ := be.auditMarks(first); over != "" {
		t.Fatalf("over_ref of the first audit = %q", over)
	}
	be.endAudit(box, "done", [2]string{"high", "Open point"})

	be.pushMine("8888888bbbb")
	be.ready(be.pr(7))
	second := be.auditID()
	if second == first {
		t.Fatalf("no audit of the later head: %v", be.audits())
	}
	in := be.auditJob(second)
	if in.DiffOver != "7777777aaaa" || in.OverPrompt != followUpInstructions {
		t.Fatalf("the later head's job: diff_over %q over_prompt %.40q", in.DiffOver, in.OverPrompt)
	}
	if !strings.Contains(in.Prompt, "Points already raised") || !strings.Contains(in.Prompt, "Open point") || strings.Contains(in.Prompt, "This is a follow-up audit") {
		t.Errorf("the prompt must carry the ledger, and the instructions go in over_prompt:\n%s", in.Prompt)
	}
	if !strings.Contains(in.Prompt, "What it is for:") {
		t.Errorf("the follow-up's prompt doesn't say what the pull request is for:\n%s", in.Prompt)
	}
	if over, followed := be.auditMarks(second); over != "7777777aaaa" || followed != "" {
		t.Errorf("marks while it runs: over %q followed %q", over, followed)
	}
	for _, want := range []string{"refers_to", "critical or high", "neither changes nor affects", "undo it or split it out"} {
		if !strings.Contains(followUpInstructions, want) {
			t.Errorf("the instructions lack %q", want)
		}
	}
}

// What the runner says decides the mark: a result with diff_over is a
// follow-up, one without says the whole change was read.
func TestAuditFollowUpMarkFollowsTheRunnersResult(t *testing.T) {
	be := newBabysitEnv(t)
	box := be.auditBox()
	be.auditOn()
	be.ready(be.openMine())
	be.endAudit(box, "done")
	be.pushMine("8888888bbbb")
	be.ready(be.pr(7))
	second := be.auditID()
	be.finishAuditWith(box, map[string]any{"diff_over": "7777777aaaa"})
	if over, followed := be.auditMarks(second); over != "7777777aaaa" || followed != "7777777aaaa" {
		t.Fatalf("a follow-up that was made: over %q followed %q", over, followed)
	}
	a, err := be.s.GetAudit(be.ctx, be.admin, second)
	if err != nil || a.FollowUpOf != "7777777aaaa" {
		t.Fatalf("audit = %+v %v", a, err)
	}

	be.pushMine("9999999cccc")
	be.ready(be.pr(7))
	third := be.auditID()
	if in := be.auditJob(third); in.DiffOver != "8888888bbbb" {
		t.Errorf("the newest finished audit is the one followed: %q", in.DiffOver)
	}
	be.finishAuditWith(box, map[string]any{"over_fallback": "The earlier audited commit 888 isn't in this branch's history (rebased?): auditing the whole change."})
	if over, followed := be.auditMarks(third); over != "8888888bbbb" || followed != "" {
		t.Errorf("a follow-up that fell back: over %q followed %q", over, followed)
	}
	if a, err = be.s.GetAudit(be.ctx, be.admin, third); err != nil || a.FollowUpOf != "" {
		t.Errorf("a fallback is shown as a follow-up: %+v %v", a, err)
	}
}

// An earlier audit that failed, one that is a footprint check, or one of the
// head's own change gives nothing to follow up.
func TestAuditedBeforeSkipsWhatCannotBeFollowedUp(t *testing.T) {
	be := newBabysitEnv(t)
	box := be.auditBox()
	be.auditOn()
	be.ready(be.openMine())
	first := be.auditID()
	be.endAudit(box, "done")

	pr := be.pr(7)
	if got, err := be.s.auditedBefore(be.ctx, pr); err != nil || got != "" {
		t.Fatalf("the audit of this very head is followed up: %q %v", got, err)
	}
	be.pushMine("8888888bbbb")
	pr = be.pr(7)
	if got, err := be.s.auditedBefore(be.ctx, pr); err != nil || got != "7777777aaaa" {
		t.Fatalf("earlier audit = %q %v", got, err)
	}
	for _, c := range []struct{ name, set string }{
		{"failed", `status = 'failed'`},
		{"a footprint check", `purpose = 'footprint'`},
		{"cancelled", `status = 'cancelled'`},
	} {
		if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE code_audits SET status = 'done', purpose = '' WHERE id = ?`, first); err != nil {
			t.Fatal(err)
		}
		if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE code_audits SET `+c.set+` WHERE id = ?`, first); err != nil {
			t.Fatal(err)
		}
		if got, err := be.s.auditedBefore(be.ctx, pr); err != nil || got != "" {
			t.Errorf("an earlier audit that is %s is followed up: %q %v", c.name, got, err)
		}
	}
}

// A later head with a follow-up audit owes no footprint check even when the
// rounds grew the pull request; the follow-up that read the whole change, or
// failed, or doesn't exist (audits off) leaves the check as it was.
func TestFootprintRidesInTheFollowUpAudit(t *testing.T) {
	be, _, pr := footprintEnv(t)
	due := func() string {
		t.Helper()
		why, _, err := be.s.footprintDue(be.ctx, be.pr(7))
		if err != nil {
			t.Fatal(err)
		}
		return why
	}
	if due() == "" {
		t.Fatal("the footprint check isn't due before any follow-up: the fixture grows the pull request")
	}
	// Audits off for the project: no audit of the head, unchanged.
	if due() == "" {
		t.Fatal("due changed")
	}
	set := func(status, over, followed string) {
		t.Helper()
		if _, err := be.s.db.W.ExecContext(be.ctx, `DELETE FROM code_audits WHERE request_key = ?`, "pr-audit:"+itoa64(pr.ID)+":8888888bbbb"); err != nil {
			t.Fatal(err)
		}
		if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO code_audits(project_id, forge_id, repo, target, ref, pr_id, pr_number, status, owner_id, request_key, created_at, over_ref, followed_up)
			SELECT ?, forge_id, repo, 'pr', '8888888bbbb', id, number, ?, ?, ?, 1, ?, ? FROM pull_requests WHERE id = ?`,
			be.p.ID, status, be.admin.User.ID, "pr-audit:"+itoa64(pr.ID)+":8888888bbbb", over, followed, pr.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name, status, over, followed string
		held                         bool
	}{
		{"queued follow-up", "queued", "7777777aaaa", "", false},
		{"running follow-up", "running", "7777777aaaa", "", false},
		{"follow-up done and made", "done", "7777777aaaa", "7777777aaaa", false},
		{"follow-up done after a fallback", "done", "7777777aaaa", "", true},
		{"follow-up failed", "failed", "7777777aaaa", "", true},
		{"full audit going", "running", "", "", true},
	} {
		set(c.status, c.over, c.followed)
		if got := due() != ""; got != c.held {
			t.Errorf("%s: footprint due = %v, want %v", c.name, got, c.held)
		}
	}
}

// The follow-up fields are checked on their way into a job: a commit that
// could be read as an option is refused, one without a diff start is refused,
// the instructions are dropped without a commit to follow up, and a job that
// isn't an audit carries neither.
func TestAuditFollowUpFieldsAreChecked(t *testing.T) {
	job := func(kind string, in AgentJob) (AgentJob, error) {
		in.Prompt, in.Repo, in.Base = "Audit it.", "https://git.example.com/me/app.git", "main"
		raw, _ := json.Marshal(in)
		_, _, out, err := checkRunnerInput(kind, raw)
		var got AgentJob
		_ = json.Unmarshal(out, &got)
		return got, err
	}
	if got, err := job(JobAudit, AgentJob{CLI: "claude", DiffFrom: "main", DiffOver: "abc123def456", OverPrompt: "FOLLOW"}); err != nil || got.DiffOver != "abc123def456" || got.OverPrompt != "FOLLOW" {
		t.Errorf("a follow-up audit = %+v %v", got, err)
	}
	if _, err := job(JobAudit, AgentJob{CLI: "claude", DiffFrom: "main", DiffOver: "--upload-pack=x", OverPrompt: "FOLLOW"}); !IsInvalid(err) {
		t.Errorf("an option as the earlier commit: %v", err)
	}
	if _, err := job(JobAudit, AgentJob{CLI: "claude", DiffOver: "abc123def456", OverPrompt: "FOLLOW"}); !IsInvalid(err) {
		t.Errorf("an earlier commit without diff_from: %v", err)
	}
	if got, err := job(JobAudit, AgentJob{CLI: "claude", DiffFrom: "main", OverPrompt: "FOLLOW"}); err != nil || got.OverPrompt != "" {
		t.Errorf("instructions without a commit = %+v %v", got, err)
	}
	if got, err := job(JobReview, AgentJob{CLI: "claude", DiffOver: "abc123def456", OverPrompt: "FOLLOW"}); err != nil || got.DiffOver != "" || got.OverPrompt != "" {
		t.Errorf("a review = %+v %v", got, err)
	}
}
