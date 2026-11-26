package core

import (
	"strings"
	"testing"
	"time"
)

// auditOn turns the project's "audit pull requests" setting on.
func (be *bsEnv) auditOn() {
	be.t.Helper()
	p, err := be.s.GetProject(be.ctx, be.admin, be.p.Key)
	if err != nil {
		be.t.Fatal(err)
	}
	st := p.Settings
	st.AuditPRs = true
	if _, err := be.s.UpdateProject(be.ctx, be.admin, be.p.Key, ProjectInput{Settings: &st}); err != nil {
		be.t.Fatal(err)
	}
}

// auditBox registers the admin's runner that can audit (Grok through Cursor).
func (be *bsEnv) auditBox() *Runner {
	return be.runner(be.admin, "auditbox", func(r *RunnerRegistration) {
		r.CLIs = []RunnerCLI{{Name: "cursor"}}
		r.Features = []string{FeatureConfine, FeatureAudit}
	})
}

// audits are the pull request's audits, oldest first, as "id:ref:status".
func (be *bsEnv) audits() []string {
	be.t.Helper()
	be.s.Wait()
	rows, err := be.s.db.R.QueryContext(be.ctx, `SELECT id, ref, status FROM code_audits WHERE pr_id = ? ORDER BY id`, be.pr(7).ID)
	if err != nil {
		be.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id int64
		var ref, st string
		_ = rows.Scan(&id, &ref, &st)
		out = append(out, itoa64(id)+":"+ref+":"+st)
	}
	return out
}

// auditID is the id of the pull request's newest audit.
func (be *bsEnv) auditID() int64 {
	be.t.Helper()
	be.s.Wait()
	var id int64
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT id FROM code_audits WHERE pr_id = ? ORDER BY id DESC LIMIT 1`, be.pr(7).ID).Scan(&id); err != nil {
		be.t.Fatalf("no audit: %v", err)
	}
	return id
}

// endAudit runs the newest audit's job on the box and ends it with the
// findings given as (severity, title) pairs.
func (be *bsEnv) endAudit(box *Runner, state string, findings ...[2]string) {
	be.t.Helper()
	be.s.Wait()
	w := be.env.poll(be.admin, box, RunnerPoll{})
	if w.Job == nil {
		be.t.Fatal("no audit job was queued")
	}
	be.finishAudit(w.Job, state, findings...)
}

// finishAudit ends a taken audit job.
func (be *bsEnv) finishAudit(job *RunnerClaim, state string, findings ...[2]string) {
	be.t.Helper()
	w := struct{ Job *RunnerClaim }{job}
	var list []map[string]any
	for i, f := range findings {
		list = append(list, map[string]any{"title": f[1], "path": "internal/export.go", "line": 10 + i, "severity": f[0],
			"problem": "Problem of " + f[1], "confirm": "Run the export as a stranger"})
	}
	res := auditResult("Read it.", list...)
	errText := ""
	if state != "done" {
		res, errText = nil, "plan spent"
	}
	raw := []byte(mustJSON(res))
	if _, err := be.s.FinishRunnerJob(be.ctx, be.admin, w.Job.ID, RunnerFinish{State: state, Result: raw, Error: errText}); err != nil {
		be.t.Fatal(err)
	}
	be.s.Wait()
	be.s.Wait()
}

// ready is Waypoint marking the pull request ready (draft-first, WP-171).
func (be *bsEnv) ready(pr *PullRequest) {
	be.t.Helper()
	err := be.s.write(be.ctx, SystemActor(), func(t *tx) error {
		t.emit(&Event{Kind: EvPRActivity, ProjectID: be.p.ID, New: "ready", Data: map[string]any{"pr_id": pr.ID, "head_sha": pr.HeadSHA}})
		return nil
	})
	if err != nil {
		be.t.Fatal(err)
	}
	be.s.Wait()
}

func (be *bsEnv) markFinding(id, state string) {
	be.t.Helper()
	if _, err := be.s.MarkAuditFindings(be.ctx, be.agent, MarkFindings{IDs: []string{id}, State: state, Note: "checked"}); err != nil {
		be.t.Fatal(err)
	}
	be.s.Wait()
}

// openMine opens the agent's pull request under the admin's own forge login,
// as a member's pull request is.
func (be *bsEnv) openMine() *PullRequest {
	be.t.Helper()
	p := bsPR
	p.Author = be.admin.User.Username
	be.pullHook("opened", p)
	be.s.Wait()
	be.drain()
	return be.pr(7)
}

func (be *bsEnv) pushMine(sha string) {
	be.t.Helper()
	p := bsPR
	p.Author = be.admin.User.Username
	p.HeadSHA = sha
	be.pullHook("synchronize", p)
	be.s.Wait()
	be.drain()
	be.s.Wait()
}

func TestPRAuditRequestedOncePerHead(t *testing.T) {
	be := newBabysitEnv(t)
	be.auditBox()
	pr0 := be.openMine()
	be.ready(pr0)
	if got := be.audits(); len(got) != 0 {
		t.Fatalf("audit with the setting off: %v", got)
	}
	be.auditOn()
	be.ready(pr0)
	if got := be.audits(); len(got) != 1 || !strings.HasPrefix(got[0][strings.Index(got[0], ":")+1:], "7777777aaaa") {
		t.Fatalf("audits at the first head: %v", got)
	}
	be.ready(pr0)
	if got := be.audits(); len(got) != 1 {
		t.Fatalf("a second audit at the same head: %v", got)
	}
	be.pushMine("8888888bbbb")
	if got := be.audits(); len(got) != 1 {
		t.Fatalf("a push alone asked for an audit: %v", got)
	}
	pr := be.pr(7)
	be.ready(pr)
	if got := be.audits(); len(got) != 2 || !strings.Contains(got[1], "8888888bbbb") {
		t.Fatalf("audits after ready at a new head: %v", got)
	}
	be.ready(pr)
	if got := be.audits(); len(got) != 2 {
		t.Fatalf("ready twice at one head: %v", got)
	}
}

func TestPRAuditNotForOutsiders(t *testing.T) {
	be := newBabysitEnv(t)
	be.auditBox()
	be.auditOn()
	p := bsPR
	p.Author = "stranger"
	p.Body = "Adds export."
	be.pullHook("opened", p)
	be.s.Wait()
	be.drain()
	if got := be.audits(); len(got) != 0 {
		t.Fatalf("a stranger's pull request was audited: %v", got)
	}
}

// Opening the pull request asks for the audit when the setting is on.
func TestPRAuditRequestedOnOpen(t *testing.T) {
	be := newBabysitEnv(t)
	be.auditBox()
	be.auditOn()
	be.openMine()
	if got := be.audits(); len(got) != 1 || !strings.Contains(got[0], "7777777aaaa") {
		t.Fatalf("audits after opening: %v", got)
	}
	a, err := be.s.GetAudit(be.ctx, be.admin, be.auditID())
	if err != nil || a.Target != "pr" || !a.PostComment || a.Via != "system" {
		t.Fatalf("audit = %+v %v", a, err)
	}
}

// A running audit holds the babysitter back like a review that is going; its
// medium and worse findings become a round, low ones only ride along.
func TestPRAuditFindingsAreRoundWork(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	box := be.auditBox()
	be.auditOn()
	pr := be.openMine()
	be.start(pr)
	be.s.Wait()
	running := be.env.poll(be.admin, box, RunnerPoll{}).Job
	if running == nil {
		t.Fatal("no audit job")
	}
	be.ci("7777777aaaa", CheckSuccess)
	be.step()
	b := be.babysit()
	if len(be.jobs()) != 0 || b.Round != 0 || b.State != BabysitWaitingReview || !strings.Contains(b.Wait, "audit #") || be.pr(7).State == "merged" {
		t.Fatalf("while the audit runs: %+v jobs %v", b, be.jobs())
	}
	if r := be.reasons(7); !strings.Contains(r, "waiting for audit #") {
		t.Errorf("reasons while it runs: %q", r)
	}
	id := be.auditID()
	be.finishAudit(running, "done", [2]string{"high", "Anyone exports"}, [2]string{"medium", "No limit on rows"}, [2]string{"low", "A typo"})
	b = be.babysit()
	jobs := be.jobs()
	if b.Round != 1 || len(jobs) != 1 || len(b.Rounds[0].Findings) != 2 {
		t.Fatalf("after the audit: %+v jobs %v", b, jobs)
	}
	in := be.jobInput(jobs[0])
	for _, want := range []string{"## Audit findings (3)", itoa64(id) + ".1 high internal/export.go:10", itoa64(id) + ".2 medium internal/export.go:11",
		itoa64(id) + ".3 low", "Problem of Anyone exports", "To confirm: Run the export as a stranger", "wp audit mark", "<untrusted>"} {
		if !strings.Contains(in.Prompt, want) {
			t.Errorf("the round's prompt lacks %q:\n%s", want, in.Prompt)
		}
	}
	be.finish(jobs[0], false)
	be.step()
	b = be.babysit()
	if len(be.jobs()) != 1 || b.Round != 1 {
		t.Fatalf("a second round for the same findings: %+v jobs %v", b, be.jobs())
	}
	if r := be.reasons(7); !strings.Contains(r, "audit #") {
		t.Errorf("the open high finding doesn't hold: %q", r)
	}
	be.markFinding(itoa64(id)+".1", "rejected")
	be.markFinding(itoa64(id)+".2", "fixed")
	if r := be.reasons(7); strings.Contains(r, "audit #") {
		t.Errorf("still held after the verdicts: %q", r)
	}
}

// Low findings alone start nothing.
func TestPRAuditLowFindingsStartNoRound(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	box := be.auditBox()
	be.auditOn()
	pr := be.openMine()
	be.start(pr)
	be.endAudit(box, "done", [2]string{"low", "A typo"})
	be.step()
	if b := be.babysit(); b.Round != 0 || len(be.jobs()) != 0 {
		t.Fatalf("a round for a low finding: %+v jobs %v", b, be.jobs())
	}
	if r := be.reasons(7); strings.Contains(r, "audit #") {
		t.Errorf("a low finding holds the merge: %q", r)
	}
}

// Medium and low findings the session rejected, that nothing checked, are
// listed in the pull request's todo (so the packet shows them) and hold
// nothing: no round starts for them and the merge isn't held (WP-1370).
func TestPRAuditUncheckedRejectionsAreListedAndHoldNothing(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	box := be.auditBox()
	be.auditOn()
	pr := be.openMine()
	be.start(pr)
	be.endAudit(box, "done", [2]string{"medium", "No limit on rows"}, [2]string{"low", "A typo"})
	id := be.auditID()
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE code_audit_findings SET state = 'rejected', note = ? WHERE audit_id = ?`, uncheckedNote+"it is intended", id); err != nil {
		t.Fatal(err)
	}
	todo, err := be.s.auditTodoErr(be.ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range todo {
		got[f.Title] = f.Unchecked
	}
	if len(todo) != 2 || !got["No limit on rows"] || !got["A typo"] {
		t.Errorf("the todo should list both unchecked rejections as unchecked: %+v", todo)
	}
	if work := be.s.auditRoundWork(be.ctx, &Babysit{}, pr.ID); len(work) != 0 {
		t.Errorf("unchecked rejections start a round: %v", work)
	}
	if r := be.reasons(7); strings.Contains(r, "audit #") {
		t.Errorf("unchecked rejections hold the merge: %q", r)
	}
}

// The setting's audit posts its findings on the pull request, and nothing
// when it found none (WP-867): agents would only read and answer it.
func TestPRAuditPostsOnlyFindings(t *testing.T) {
	be := newBabysitEnv(t)
	box := be.auditBox()
	be.auditOn()
	be.openMine()
	id := be.auditID()
	be.endAudit(box, "done")
	if posted := strings.Join(be.sg.posted, "\n---\n"); strings.Contains(posted, "waypoint-audit") {
		t.Errorf("a clean audit was posted:\n%s", posted)
	}
	if a, err := be.s.GetAudit(be.ctx, be.admin, id); err != nil || a.Status != AuditDone || a.CommentURL != "" {
		t.Fatalf("clean audit = %+v %v", a, err)
	}
	be.pushMine("8888888bbbb")
	be.ready(be.pr(7))
	be.endAudit(box, "done", [2]string{"medium", "No limit on rows"})
	if posted := strings.Join(be.sg.posted, "\n---\n"); !strings.Contains(posted, "No limit on rows") {
		t.Errorf("the finding wasn't posted:\n%s", posted)
	}
}

// An audit that ended limited holds nothing, and the readiness says so.
func TestPRAuditLimitedHoldsNothing(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	box := be.auditBox()
	be.auditOn()
	pr := be.openMine()
	be.start(pr)
	be.endAudit(box, "failed")
	be.setClock(be.clockNow().Add(auditRetryStep + time.Minute))
	be.runnerOnline()
	be.s.CheckAudits(be.ctx)
	be.endAudit(box, "failed")
	be.step()
	if b := be.babysit(); b.Round != 0 || len(be.jobs()) != 0 {
		t.Fatalf("a round after a failed audit: %+v jobs %v", b, be.jobs())
	}
	rd, err := be.s.PRReadiness(be.ctx, be.admin, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rd.Reasons {
		if strings.Contains(r, "audit #") {
			t.Errorf("a failed audit holds: %q", r)
		}
	}
	if len(rd.Notes) != 1 || !strings.Contains(rd.Notes[0], "not holding the merge") || !strings.HasPrefix(rd.Notes[0], "audit #") {
		t.Errorf("notes = %q", rd.Notes)
	}
}

// An audit no runner has taken holds the pull request for auditQueueWait,
// not for as long as the queue is: a busy runner must not stall it. One a
// runner took holds it until it ends (WP-356).
func TestPRAuditWaitsBrieflyForARunner(t *testing.T) {
	be := newBabysitEnv(t)
	box := be.auditBox()
	be.auditOn()
	pr := be.openMine()
	if id, going := be.s.auditGoing(be.ctx, pr.ID); !going || id != be.auditID() {
		t.Fatalf("a just-asked audit doesn't hold the pull request: %d %v", id, going)
	}
	be.setClock(be.clockNow().Add(auditQueueWait + time.Minute))
	if id, going := be.s.auditGoing(be.ctx, pr.ID); going {
		t.Fatalf("audit #%d still holds after waiting %s for a runner", id, auditQueueWait+time.Minute)
	}
	be.runnerOnline()
	if w := be.env.poll(be.admin, box, RunnerPoll{}); w.Job == nil || w.Job.Kind != JobAudit {
		t.Fatalf("no audit job to take: %+v", w.Job)
	}
	if _, going := be.s.auditGoing(be.ctx, pr.ID); !going {
		t.Error("an audit a runner is reading doesn't hold the pull request")
	}
	be.setClock(be.clockNow().Add(auditWaitMax + time.Minute))
	if _, going := be.s.auditGoing(be.ctx, pr.ID); going {
		t.Errorf("an audit still holds %s after a runner took it", auditWaitMax+time.Minute)
	}
}
