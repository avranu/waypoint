package core

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

// task links a new task in the repo's project to the pull request.
func (be *bsEnv) task(title, desc string) *IssueDetail {
	be.t.Helper()
	task := be.issue(be.admin, IssueInput{Project: be.p.Key, Title: title, Description: desc})
	if _, err := be.s.AddLink(be.ctx, be.admin, task.Key, be.pr(7).URL, ""); err != nil {
		be.t.Fatal(err)
	}
	return task
}

// Every round's packet opens with what the pull request is for: its task,
// its description, the decisions they cite.
func TestDriftGuardIntentInThePacket(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	adr, err := be.s.CreateDesign(be.ctx, be.admin, DesignInput{Title: str("Exports stay CSV"), Kind: str("decision"), Body: str("We export CSV only, no XLSX."), Project: &be.p.Key})
	if err != nil {
		t.Fatal(err)
	}
	pr := be.open()
	task := be.task("Add a CSV export", "Members export the issue list as CSV, per "+adr.Code+".")
	be.start(pr)
	be.ci("7777777aaaa", CheckFailed)
	jobs := be.jobs()
	if len(jobs) != 1 {
		t.Fatalf("no round: %+v", be.babysit())
	}
	p := be.jobInput(jobs[0]).Prompt
	for _, want := range []string{"## What the PR is for", "### Task " + task.Key + ": Add a CSV export", "Members export the issue list as CSV",
		"### PR description when it shipped", "Adds export.", "### Decisions cited", adr.Code + " (", "Exports stay CSV", "We export CSV only"} {
		if !strings.Contains(p, want) {
			t.Errorf("the packet lacks %q:\n%s", want, p)
		}
	}
	if i, j := strings.Index(p, "## What the PR is for"), strings.Index(p, "## Failed checks"); j >= 0 && i > j {
		t.Errorf("the intent comes after the work:\n%s", p)
	}
	if fp := be.s.footprintOf(be.ctx, pr.ID); fp.Ship == nil || fp.Ship.SHA != "7777777aaaa" || !strings.Contains(fp.Ship.Body, "Adds export.") || !fp.Ship.Known {
		t.Errorf("ship footprint = %+v", fp.Ship)
	}
}

// --defer files a follow-up task related to the pull request's task,
// replies "Filed as" on the thread and resolves it; the comment no longer
// holds the merge.
func TestDriftGuardDeferFilesATask(t *testing.T) {
	be := newBabysitEnv(t)
	be.sg.setNotes(PRNote{ID: "603", Kind: "comment", Author: "carol", Body: "Also add XLSX export while you're here.", At: time.Now()})
	pr := be.open()
	task := be.task("Add a CSV export", "CSV only.")
	be.start(pr)
	if !strings.Contains(be.reasons(7), "1 review comment not dealt with") {
		t.Fatalf("not held: %q", be.reasons(7))
	}
	c := be.comment("603")
	if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{c.Ref}, Verdict: VerdictDeferred}); !IsInvalid(err) {
		t.Errorf("a deferral without a reason: %v", err)
	}
	if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{c.Ref}, Verdict: VerdictDeferred, Note: "XLSX is its own feature."}); err != nil {
		t.Fatal(err)
	}
	got := be.comment("603")
	if got.Verdict != VerdictDeferred || !strings.HasPrefix(got.Note, "Filed as "+be.p.Key+"-") || !strings.HasSuffix(got.Note, ": XLSX is its own feature.") {
		t.Fatalf("comment = %+v", got)
	}
	key := strings.TrimPrefix(strings.SplitN(got.Note, ":", 2)[0], "Filed as ")
	filed := be.get(be.admin, key)
	if !strings.Contains(filed.Title, "Follow-up from me/app#7: Also add XLSX export") || !strings.Contains(filed.Description, "Also add XLSX export while you're here.") ||
		!strings.Contains(filed.Description, "XLSX is its own feature.") || !strings.Contains(filed.Description, task.Key) {
		t.Errorf("filed = %q\n%s", filed.Title, filed.Description)
	}
	related := false
	for _, r := range filed.Relations {
		related = related || (r.Kind == "relates" && r.Issue.Key == task.Key)
	}
	if !related {
		t.Errorf("not related to %s: %+v", task.Key, filed.Relations)
	}
	if r := be.reasons(7); strings.Contains(r, "review comment") {
		t.Errorf("a deferred comment still holds: %q", r)
	}
}

// A deferred CodeRabbit finding gets a deferred marker in its reply, so
// CodeRabbit learns of it (WP-442's replies).
func TestDriftGuardDeferTellsCodeRabbit(t *testing.T) {
	be := newBabysitEnv(t)
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT OR IGNORE INTO forge_people(forge_id, login, user_id) VALUES (?, 'mirror', NULL)`, be.f.ID); err != nil {
		t.Fatal(err)
	}
	heavy := strings.Replace(crLine, "**Minor** · **Close the file.**", "**Minor** · **Close the file.** · **Heavy lift**", 1)
	be.sg.setNotes(PRNote{ID: "703", Kind: "comment", Author: "mirror", Body: heavy, At: at})
	pr := be.open()
	be.start(pr)
	line := be.comment("703")
	if !line.heavyLift() {
		t.Fatalf("not a heavy lift: %q", line.Body)
	}
	if p := (&PRTodo{Repo: "me/app", Number: 7, Comments: []TodoComment{{PRReviewComment: line, HeavyLift: true}}}).render(); !strings.Contains(p, "[heavy lift: defer unless the PR needs it]") {
		t.Errorf("no heavy-lift tag:\n%s", p)
	}
	if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{line.Ref}, Verdict: VerdictDeferred, Note: "Not this change."}); err != nil {
		t.Fatal(err)
	}
	be.sg.sm.Lock()
	posted := strings.Join(be.sg.posted, "\n---\n")
	be.sg.sm.Unlock()
	if !strings.Contains(posted, "<!-- review-verdict github 4439419700 deferred -->") || !strings.Contains(posted, "Filed as "+be.p.Key+"-") {
		t.Errorf("replies:\n%s", posted)
	}
}

// With no project to file in, a deferral is refused.
func TestDriftGuardDeferNeedsAProject(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	if _, err := be.s.fileDeferred(be.ctx, be.admin, &PullRequest{ID: pr.ID, Repo: "elsewhere/x", Number: 1, ForgeID: pr.ForgeID}, []*PRReviewComment{{Ref: "c1", Body: "x"}}, "why"); !IsInvalid(err) {
		t.Errorf("filed with no project: %v", err)
	}
}

func TestFootprintMeasureRounds(t *testing.T) {
	ref := footprintAt{Paths: []string{"a.go", "b.go"}, Lines: 100}
	at := time.Now()
	round := func(n int, lines int, files ...string) BabysitRound {
		return BabysitRound{N: n, At: at, Pushed: true, Files: files, Lines: lines}
	}
	for _, c := range []struct {
		name   string
		rounds []BabysitRound
		after  int
		added  int
		grew   bool
	}{
		{"kept to the files, 50 lines", []BabysitRound{round(1, 50, "a.go")}, 0, 0, false},
		{"151 lines is more than half of 100", []BabysitRound{round(1, 51, "a.go"), round(2, 100, "b.go")}, 0, 0, true},
		{"a new file", []BabysitRound{round(1, 3, "a.go", "c.go")}, 0, 1, false},
		{"20 lines is under the minimum", []BabysitRound{round(1, 20, "a.go")}, 0, 0, false},
		{"rounds before the last check don't count", []BabysitRound{round(1, 500, "z.go"), round(2, 1, "a.go")}, 1, 0, false},
		{"a round that pushed nothing doesn't count", []BabysitRound{{N: 1, At: at, Lines: 900, Files: []string{"z.go"}}}, 0, 0, false},
	} {
		m := measureRounds(c.rounds, at, c.after, ref)
		if len(m.added) != c.added || m.grew(ref) != c.grew {
			t.Errorf("%s: added %v lines %d grew %v", c.name, m.added, m.lines, m.grew(ref))
		}
	}
	if m := measureRounds([]BabysitRound{{N: 1, At: at, Pushed: true, Diff: "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1,2 @@\n-a\n+b\n+c\n--- a comment\n"}}, at, 0, ref); m.lines != 4 || len(m.unknown) != 0 {
		t.Errorf("a round with only its diff: lines %d unknown %v, want 4 (a removed line starting with -- counts) and known", m.lines, m.unknown)
	}
}

// A round that spreads to new files gets a footprint check before the merge;
// what it names is round work and holds the merge, and it doesn't count as
// the pull request's audit.
func TestDriftGuardFootprintCheck(t *testing.T) {
	be, box, pr := footprintEnv(t)
	if r := be.reasons(7); !strings.Contains(r, "footprint check due") {
		t.Errorf("reasons before the check is asked for: %q", r)
	}
	be.ci("8888888bbbb", CheckSuccess)
	be.step()
	b := be.babysit()
	if !strings.Contains(b.Wait, "a footprint check") {
		t.Fatalf("no footprint check: %+v", b)
	}
	a, err := be.s.GetAudit(be.ctx, be.admin, be.auditID())
	if err != nil || a.Purpose != AuditFootprint || a.Target != "pr" {
		t.Fatalf("audit = %+v %v", a, err)
	}
	if !slices.Equal(a.Models, auditModelNames([]AgentModel{{CLI: "cursor", Model: "grok-4.7-high"}, {CLI: "cursor", Model: "grok-4.7-medium"}, {CLI: GatewayCLI, Model: gatewayAuditModel}})) {
		t.Errorf("the check's models = %v", a.Models)
	}
	for _, want := range []string{"Check this pull request's footprint", "app/xlsx.go", "Task " + be.p.Key + "-", "+bounded the table", "only scope"} {
		if !strings.Contains(a.prompt, want) {
			t.Errorf("the check's prompt lacks %q:\n%s", want, a.prompt)
		}
	}
	if id, _, _ := be.s.newestAudit(be.ctx, pr.ID); id != 0 {
		t.Errorf("a footprint check counted as the audit")
	}
	if read, err := be.s.headAudited(be.ctx, be.pr(7)); err != nil || read {
		t.Error("a footprint check stood in for the head's audit")
	}
	be.step()
	if n := len(be.audits()); n != 1 {
		t.Errorf("%d checks", n)
	}
	be.endAudit(box, "done", [2]string{"medium", "XLSX export is beyond the task"})
	b = be.babysit()
	jobs := be.jobs()
	if b.Round != 2 || len(jobs) != 2 || len(b.Rounds[1].Findings) != 1 {
		t.Fatalf("after the check: %+v jobs %v", b, jobs)
	}
	if p := be.jobInput(jobs[1]).Prompt; !strings.Contains(p, "Changes beyond the task") || !strings.Contains(p, "XLSX export is beyond the task") {
		t.Errorf("the round's prompt:\n%s", p)
	}
	if r := be.reasons(7); !strings.Contains(r, "a footprint check, named 1 change beyond the task") {
		t.Errorf("the finding doesn't hold: %q", r)
	}
	be.markFinding(itoa64(a.ID)+".1", "fixed")
	if r := be.reasons(7); strings.Contains(r, "footprint") {
		t.Errorf("still held once fixed: %q", r)
	}
}

// footprintEnv is a babysat pull request for "Add a CSV export" whose first
// round pushed two files it didn't touch, with the LLM gateway set up
// and the admin's runner that sends audits to it.
func footprintEnv(t *testing.T) (*bsEnv, *Runner, *PullRequest) {
	t.Helper()
	be := newBabysitEnv(t)
	be.runnerOnline()
	taskGateway(t, be.s)
	box := be.runner(be.admin, "checkbox", func(r *RunnerRegistration) {
		yes := true
		r.CLIs = []RunnerCLI{{Name: "claude", Version: "2.1", SignedIn: &yes}}
		r.Features = []string{FeatureConfine, FeatureAudit, FeatureGatewayAudit}
	})
	pr := be.openMine()
	be.task("Add a CSV export", "CSV only.")
	be.start(pr)
	be.ci("7777777aaaa", CheckFailed)
	jobs := be.jobs()
	if len(jobs) != 1 {
		t.Fatalf("no round: %+v", be.babysit())
	}
	be.finishPushing(jobs[0], true, []string{"app/xlsx.go", "app/xlsx_test.go"}, 40)
	be.pushMine("8888888bbbb")
	be.changes("8888888bbbb", "app/xlsx.go", "app/xlsx_test.go")
	return be, box, pr
}

// A footprint check that finds the rounds in scope posts nothing on the pull
// request (WP-867): its audit and the footprint record say it ran, and the
// merge goes on.
func TestDriftGuardFootprintInScopeIsQuiet(t *testing.T) {
	be, box, pr := footprintEnv(t)
	be.ci("8888888bbbb", CheckSuccess)
	be.step()
	id := be.auditID()
	be.endAudit(box, "done")
	if posted := strings.Join(be.sg.posted, "\n---\n"); strings.Contains(posted, "Footprint check") || strings.Contains(posted, "waypoint-audit") {
		t.Errorf("a clean footprint check was posted:\n%s", posted)
	}
	a, err := be.s.GetAudit(be.ctx, be.admin, id)
	if err != nil || a.Status != AuditDone || a.CommentURL != "" {
		t.Fatalf("audit = %+v %v", a, err)
	}
	fp := be.s.footprintOf(be.ctx, pr.ID)
	if len(fp.Checks) != 1 || fp.Checks[0].Audit != id || fp.Checks[0].Outcome != footprintAudited {
		t.Errorf("footprint = %+v", fp)
	}
	if r := be.reasons(7); strings.Contains(r, "footprint") {
		t.Errorf("a clean check holds the merge: %q", r)
	}
}

// Rounds that don't spread or grow it get no check, and a pull request
// shipped before the guard (no footprint) never does.
func TestDriftGuardFootprintNotForSmallRounds(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.openMine()
	be.start(pr)
	be.ci("7777777aaaa", CheckFailed)
	be.finish(be.jobs()[0], true)
	be.pushMine("8888888bbbb")
	be.changes("8888888bbbb")
	be.ci("8888888bbbb", CheckSuccess)
	be.step()
	if n := len(be.audits()); n != 0 {
		t.Errorf("a check for a round that kept to the files: %v", be.audits())
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET footprint = '' WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	be.changes("8888888bbbb", "app/new.go")
	if why, _, _ := be.s.footprintDue(be.ctx, be.pr(7)); why != "" {
		t.Errorf("due with no ship footprint: %q", why)
	}
}

// botRun records a review run of the pull request in a state ("done" ends
// now).
func (be *bsEnv) botRun(status string) {
	be.t.Helper()
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO bot_runs(project_id, pr_id, kind, head_sha, status, created_at, finished_at)
		VALUES ((SELECT min(id) FROM projects), ?, 'review', ?, ?, ?, ?)`, be.pr(7).ID, be.pr(7).HeadSHA, status, be.s.nowMS(),
		map[bool]any{true: be.s.nowMS(), false: nil}[status == "done"]); err != nil {
		be.t.Fatal(err)
	}
}

// footprintChecks are the footprint checks kept for the pull request.
func (be *bsEnv) footprintChecks() []footprintCheck {
	be.t.Helper()
	return be.s.footprintOf(be.ctx, be.pr(7).ID).Checks
}

// A review still going is no reason to ask for the footprint check: it
// usually ends in a round, and the round's push makes the audit stale. Once the
// run is over the check is asked (WP-1853).
func TestDriftGuardNoFootprintWhileAReviewRuns(t *testing.T) {
	be, _, _ := footprintEnv(t)
	be.botRun("running")
	be.ci("8888888bbbb", CheckSuccess)
	be.step()
	if n := len(be.audits()); n != 0 {
		t.Fatalf("audits = %v, want none while a review runs", be.audits())
	}
	if b := be.babysit(); !strings.Contains(b.Wait, "review") {
		t.Errorf("the wait is not the review's: %q", b.Wait)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE bot_runs SET status = 'done', finished_at = ?`, be.s.nowMS()); err != nil {
		t.Fatal(err)
	}
	be.step()
	if n := len(be.audits()); n != 1 {
		t.Fatalf("audits = %v, want the check once the review is over", be.audits())
	}
	if c := be.footprintChecks(); len(c) != 1 || !c[0].Ahead {
		t.Errorf("checks = %+v, want one asked ahead", c)
	}
}

// The quiet period holds the merge, not the check: it starts beside it.
func TestDriftGuardFootprintInTheQuietPeriod(t *testing.T) {
	be, _, _ := footprintEnv(t)
	be.policy(MergePolicyInput{ReviewWaitMinutes: ptr(0), QuietMinutes: ptr(10)})
	be.botRun("done")
	be.ci("8888888bbbb", CheckSuccess)
	be.step()
	if n := len(be.audits()); n != 1 {
		t.Fatalf("audits = %v, want the check asked in the quiet period; %+v", be.audits(), be.babysit())
	}
	if w := be.babysit().Wait; !strings.Contains(w, "quiet") {
		t.Errorf("the wait is not the quiet period's: %q", w)
	}
	be.step()
	if n := len(be.audits()); n != 1 {
		t.Errorf("asked again: %v", be.audits())
	}
}

// With a comment open that waits for a person, the check starts too.
func TestDriftGuardFootprintWithACommentOpen(t *testing.T) {
	be, _, _ := footprintEnv(t)
	be.sg.threads = humanThread()
	if _, err := be.s.syncReviewComments(be.ctx, be.pr(7)); err != nil {
		t.Fatal(err)
	}
	ref := be.comment("501").Ref
	if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{ref}, Verdict: "needs-person", Note: "A product call."}); err != nil {
		t.Fatal(err)
	}
	be.ci("8888888bbbb", CheckSuccess)
	be.step()
	if n := len(be.audits()); n != 1 {
		t.Fatalf("audits = %v, want the footprint check asked with a comment open", be.audits())
	}
}

// Not where the head is about to move: a head whose checks failed gets its
// round, and the check waits for the head that round makes.
func TestDriftGuardFootprintNotBesideARound(t *testing.T) {
	be, _, _ := footprintEnv(t)
	be.ci("8888888bbbb", CheckFailed)
	be.step()
	b := be.babysit()
	if b.Round != 2 || len(be.audits()) != 0 {
		t.Fatalf("round %d audits %v, want a round and no check", b.Round, be.audits())
	}
}

// A footprint audit that is going holds the merge, not the babysit: a comment
// that makes work gets its round while it runs, and with nothing else to do
// the babysit still says it waits for the check (WP-1853).
func TestDriftGuardFootprintAuditDoesNotHoldARound(t *testing.T) {
	be, _, pr := footprintEnv(t)
	be.ci("8888888bbbb", CheckSuccess)
	be.step()
	id := be.auditID()
	if g, going := be.s.auditGoing(be.ctx, pr.ID); !going || g != id {
		t.Fatalf("audit %d is not going: %d %v", id, g, going)
	}
	be.step()
	if b := be.babysit(); b.State != BabysitWatching || !strings.Contains(b.Wait, "footprint check") {
		t.Errorf("with nothing else to do: %s %q, want it waiting on the footprint check", b.State, b.Wait)
	}
	if r := be.reasons(7); !strings.Contains(r, "waiting for audit") {
		t.Errorf("the merge isn't held by the audit: %q", r)
	}
	be.sg.threads = humanThread()
	if _, err := be.s.syncReviewComments(be.ctx, be.pr(7)); err != nil {
		t.Fatal(err)
	}
	be.step()
	if b := be.babysit(); b.Round != 2 || len(be.jobs()) != 2 {
		t.Fatalf("no round for the comment while the footprint audit runs: round %d jobs %v %s %q", b.Round, be.jobs(), b.State, b.Wait)
	}
}

// A footprint audit asked for a head the pull request has left is stopped.
func TestDriftGuardStaleFootprintAuditIsCancelled(t *testing.T) {
	be, _, _ := footprintEnv(t)
	be.ci("8888888bbbb", CheckSuccess)
	be.step()
	be.pushMine("9999999cccc")
	be.changes("9999999cccc", "app/xlsx.go", "app/xlsx_test.go", "app/pdf.go", "app/pdf_test.go")
	be.ci("9999999cccc", CheckSuccess)
	be.step()
	got := be.audits()
	if len(got) != 2 || !strings.HasSuffix(got[0], ":cancelled") || strings.HasSuffix(got[1], ":cancelled") {
		t.Fatalf("audits = %v, want the first cancelled and a second asked for the new head", got)
	}
}

// A stopped check is nothing to measure growth from: a push that changes
// only what it was asked about still gets one.
func TestDriftGuardCancelledCheckIsNoReference(t *testing.T) {
	be, _, _ := footprintEnv(t)
	be.ci("8888888bbbb", CheckSuccess)
	be.step()
	c := be.footprintChecks()
	if len(c) != 1 {
		t.Fatalf("checks = %+v", c)
	}
	be.pushMine("9999999cccc")
	be.changes("9999999cccc", c[0].Paths...) // the same files, no bigger
	be.ci("9999999cccc", CheckSuccess)
	be.step()
	got := be.audits()
	if len(got) != 2 || !strings.HasSuffix(got[0], ":cancelled") || strings.HasSuffix(got[1], ":cancelled") {
		t.Fatalf("audits = %v, want the first cancelled and one for the new head", got)
	}
}

// A check asked ahead for a head that has since moved doesn't count toward
// the pull request's two: the late path still asks for the head it ends on,
// and ahead asks are capped.
func TestDriftGuardAheadChecksOfOldHeadsDontCount(t *testing.T) {
	be, _, _ := footprintEnv(t)
	be.ci("8888888bbbb", CheckSuccess)
	be.step()
	more := []string{"app/xlsx.go", "app/xlsx_test.go"}
	for _, sha := range []string{"9999999cccc", "aaaaaaadddd"} {
		more = append(more, "app/"+sha+".go")
		be.pushMine(sha)
		be.changes(sha, more...)
		be.ci(sha, CheckSuccess)
		be.step()
	}
	c := be.footprintChecks()
	var ahead []bool
	for _, x := range c {
		ahead = append(ahead, x.Ahead)
	}
	if len(c) != 3 || !slices.Equal(ahead, []bool{true, true, false}) || !sameSHA(c[2].SHA, "aaaaaaadddd") {
		t.Fatalf("checks = %+v, want two asked ahead (the old heads) and the late one for the head", c)
	}
	if n := len(be.audits()); n != 3 {
		t.Errorf("audits = %v", be.audits())
	}
}

// A check asked ahead whose audit is still queued once the pull request is
// otherwise ready holds the merge for the queue wait from then, not from the
// ask: the babysit stamps the first time it sees it then.
func TestDriftGuardQueuedAheadCheckWaitsFromReady(t *testing.T) {
	be, _, pr := footprintEnv(t)
	be.policy(MergePolicyInput{ReviewWaitMinutes: ptr(0), QuietMinutes: ptr(10)})
	be.botRun("done")
	be.ci("8888888bbbb", CheckSuccess)
	be.step() // the quiet period: the check is asked ahead
	if c := be.footprintChecks(); len(c) != 1 || !c[0].Ahead {
		t.Fatalf("checks = %+v", c)
	}
	// No runner takes it, and the queue wait passes before the babysit is ready.
	be.setClock(be.clockNow().Add(auditQueueWait + 5*time.Minute))
	if _, going := be.s.auditGoing(be.ctx, pr.ID); going {
		t.Fatal("the queued audit still counts as going")
	}
	be.step() // the babysit sees it queued: the wait starts here
	if fp := be.s.footprintOf(be.ctx, pr.ID); len(fp.Checks) != 1 || fp.Checks[0].Queued.IsZero() {
		t.Fatalf("not stamped: %+v", fp.Checks)
	}
	if r := be.reasons(7); !strings.Contains(r, "footprint check queued") {
		t.Fatalf("the merge isn't held by the queued check: %q", r)
	}
	// wp pr wait goes on waiting for it: reviewGoing no longer counts it.
	w, err := be.s.WaitPullRequest(be.ctx, be.agent, pr.ID, PRWaitInput{})
	if err != nil || w.Verdict != PRWaitWaiting || !strings.Contains(w.Why, "footprint check queued") {
		t.Errorf("wait = %+v %v", w, err)
	}
	be.setClock(be.clockNow().Add(auditQueueWait - time.Minute))
	if r := be.reasons(7); !strings.Contains(r, "footprint check queued") {
		t.Errorf("released early: %q", r)
	}
	be.setClock(be.clockNow().Add(2 * time.Minute))
	if r := be.reasons(7); strings.Contains(r, "footprint") {
		t.Errorf("held past the queue wait: %q", r)
	}
}

// An ahead check the babysit never stamped (it rests, or was set off) stops
// holding the merge once it is older than an audit may take.
func TestDriftGuardUnstampedAheadCheckStopsHolding(t *testing.T) {
	be, _, _ := footprintEnv(t)
	be.policy(MergePolicyInput{ReviewWaitMinutes: ptr(0), QuietMinutes: ptr(10)})
	be.botRun("done")
	be.ci("8888888bbbb", CheckSuccess)
	be.step() // asked ahead
	be.setClock(be.clockNow().Add(auditWaitMax - time.Minute))
	if r := be.reasons(7); !strings.Contains(r, "footprint check queued") {
		t.Fatalf("not held while unstamped: %q", r)
	}
	be.setClock(be.clockNow().Add(2 * time.Minute))
	if r := be.reasons(7); strings.Contains(r, "footprint") {
		t.Errorf("held for good by an unstamped check: %q", r)
	}
}

// An ahead ask that can't be made (nobody's runner) records nothing: the head
// stays unseen and the late path decides, as it always did.
func TestDriftGuardAheadSkipIsNotRecorded(t *testing.T) {
	be, _, pr := footprintEnv(t)
	be.policy(MergePolicyInput{ReviewWaitMinutes: ptr(0), QuietMinutes: ptr(10)})
	be.botRun("done")
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET author = '' WHERE id = ?`, pr.ID); err != nil {
		t.Fatal(err)
	}
	be.ci("8888888bbbb", CheckSuccess)
	be.step()
	if c := be.footprintChecks(); len(c) != 0 {
		t.Fatalf("the ahead ask recorded %+v", c)
	}
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET author = ? WHERE id = ?`, be.admin.User.Username, pr.ID); err != nil {
		t.Fatal(err)
	}
	be.setClock(be.clockNow().Add(11 * time.Minute))
	be.step()
	if c := be.footprintChecks(); len(c) != 1 || c[0].Outcome != footprintAudited {
		t.Errorf("checks = %+v, want the audit asked now", c)
	}
}

// sizedDiff is the forge's diff of the pull request at sha: lines added in
// the first path, and one changed line in each of the others.
func (be *bsEnv) sizedDiff(sha string, lines int, paths ...string) {
	be.t.Helper()
	var d strings.Builder
	for i, p := range paths {
		n := 1
		if i == 0 {
			n = lines
		}
		fmt.Fprintf(&d, "diff --git a/%[1]s b/%[1]s\n--- a/%[1]s\n+++ b/%[1]s\n@@ -1 +1,%[2]d @@\n", p, n)
		d.WriteString(strings.Repeat("+x\n", n))
	}
	be.sg.mu.Lock()
	be.sg.diffs[sha] = d.String()
	be.sg.mu.Unlock()
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET files_sha = '' WHERE id = ?`, be.pr(7).ID); err != nil {
		be.t.Fatal(err)
	}
}

// shippedEnv is a babysat pull request that shipped touching app/login.go
// with shipLines lines, whose first round (its job is returned) is out.
func shippedEnv(t *testing.T, shipLines int) (*bsEnv, int64) {
	t.Helper()
	be := newBabysitEnv(t)
	be.runnerOnline()
	taskGateway(t, be.s)
	pr := be.openMine()
	be.task("Add a CSV export", "CSV only.")
	be.sizedDiff("7777777aaaa", shipLines, "app/login.go")
	be.start(pr)
	be.ci("7777777aaaa", CheckFailed)
	jobs := be.jobs()
	if len(jobs) != 1 {
		t.Fatalf("no round: %+v", be.babysit())
	}
	if fp := be.s.footprintOf(be.ctx, pr.ID); fp.Ship == nil || fp.Ship.Lines != shipLines || !slices.Equal(fp.Ship.Paths, []string{"app/login.go"}) {
		t.Fatalf("ship = %+v", fp.Ship)
	}
	return be, jobs[0]
}

// footprintWhy is why a check is due at the head, "" when none is.
func (be *bsEnv) footprintWhy() string {
	be.t.Helper()
	why, _, err := be.s.footprintDue(be.ctx, be.pr(7))
	if err != nil {
		be.t.Fatal(err)
	}
	return why
}

// What main gained while the forge's merge base was stale is not the rounds'
// doing: a round that pushed 5 lines in a file the pull request had owes no
// check however big the forge says the pull request grew (WP-1939, #590).
func TestFootprintIgnoresWhatMainGained(t *testing.T) {
	be, job := shippedEnv(t, 100)
	be.finishPushing(job, true, []string{"app/login.go"}, 5)
	be.pushMine("8888888bbbb")
	paths := []string{"app/login.go"}
	for i := range 80 {
		paths = append(paths, fmt.Sprintf("main/moved%d.go", i))
	}
	be.sizedDiff("8888888bbbb", 50000, paths...)
	if why := be.footprintWhy(); why != "" {
		t.Errorf("due for what main gained: %q", why)
	}
	if pr := be.pr(7); len(pr.files) != 0 {
		be.s.loadPRFiles(be.ctx, pr)
		if n, _ := pr.changedLines(); n < 50000 || len(pr.files) != 81 {
			t.Errorf("the forge's numbers weren't the jump the test needs: %d lines, %d files", n, len(pr.files))
		}
	}
}

func TestFootprintDueForANewFileOfARound(t *testing.T) {
	be, job := shippedEnv(t, 100)
	be.finishPushing(job, true, []string{"app/login.go", "app/export.go"}, 5)
	be.pushMine("8888888bbbb")
	if why := be.footprintWhy(); !strings.Contains(why, "its fix-up rounds touched 1 new file") {
		t.Errorf("why = %q", why)
	}
}

func TestFootprintDueForALotOfLinesInRounds(t *testing.T) {
	for _, c := range []struct {
		lines int
		due   bool
	}{{200, true}, {20, false}, {40, false}} {
		be, job := shippedEnv(t, 100)
		be.finishPushing(job, true, []string{"app/login.go"}, c.lines)
		be.pushMine("8888888bbbb")
		why := be.footprintWhy()
		if (why != "") != c.due || c.due && !strings.Contains(why, "its fix-up rounds changed 200 lines (it was 100 before them)") {
			t.Errorf("%d lines: why = %q", c.lines, why)
		}
	}
}

// A check measures from the round it covered: a later round's small push
// owes none, its new file does, and two checks are all a pull request gets.
func TestFootprintCountsRoundsAfterTheLastCheck(t *testing.T) {
	be, box, _ := footprintEnv(t)
	be.ci("8888888bbbb", CheckSuccess)
	be.step()
	be.endAudit(box, "done", [2]string{"medium", "XLSX export is beyond the task"})
	c := be.footprintChecks()
	if len(c) != 1 || c[0].Round != 1 || !slices.Contains(c[0].Paths, "app/xlsx.go") {
		t.Fatalf("checks = %+v", c)
	}
	// Round 2 is the check's finding, round 3 comes of a red check on its head.
	jobs := be.jobs()
	if len(jobs) != 2 {
		t.Fatalf("no round for the finding: %+v", be.babysit())
	}
	be.finishPushing(jobs[1], true, []string{"app/xlsx.go"}, 5)
	be.pushMine("9999999cccc")
	be.sizedDiff("9999999cccc", 3, "app/login.go")
	if why := be.footprintWhy(); why != "" {
		t.Fatalf("due again for a small push in a file the check saw: %q", why)
	}
	be.ci("9999999cccc", CheckFailed)
	jobs = be.jobs()
	if len(jobs) != 3 {
		t.Fatalf("no round for the red check: %+v", be.babysit())
	}
	be.finishPushing(jobs[2], true, []string{"app/more.go"}, 5)
	be.pushMine("aaaaaaadddd")
	be.sizedDiff("aaaaaaadddd", 3, "app/login.go")
	if why := be.footprintWhy(); !strings.Contains(why, "touched 1 new file") {
		t.Fatalf("not due for a new file after the check: %q", why)
	}
	be.ci("aaaaaaadddd", CheckSuccess)
	be.step()
	c = be.footprintChecks()
	if len(c) != 2 || c[1].Round != 3 || !slices.Contains(c[1].Paths, "app/more.go") {
		t.Fatalf("checks = %+v", c)
	}
}

// A round that reported neither files nor a diff can't be measured: no
// check is due, and that is logged once.
func TestFootprintFailsOpenForARoundThatReportedNothing(t *testing.T) {
	be, job := shippedEnv(t, 100)
	logs := new(strings.Builder)
	be.s.log = slog.New(slog.NewTextHandler(logs, nil))
	raw, _ := json.Marshal(map[string]any{"pushed": true, "commits": []string{"abc"}, "commit": "abc1234", "answer": "Round over."})
	if err := be.s.write(be.ctx, SystemActor(), func(t *tx) error { return t.finishRunnerJob(be.ctx, job, "done", nil, raw, "") }); err != nil {
		t.Fatal(err)
	}
	be.s.Wait()
	be.s.Wait()
	be.pushMine("8888888bbbb")
	be.sizedDiff("8888888bbbb", 5000, "app/login.go", "app/new.go")
	for range 3 {
		if why := be.footprintWhy(); why != "" {
			t.Fatalf("due for a round that reported nothing: %q", why)
		}
	}
	if n := strings.Count(logs.String(), "footprint not measured"); n != 1 {
		t.Errorf("logged %d times, want once:\n%s", n, logs)
	}
}
