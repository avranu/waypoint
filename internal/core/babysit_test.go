package core

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// PRNotes and ReactTo make the shipping fake list review bodies and
// conversation comments and take emoji on them.
func (g *shipForge) PRNotes(ctx context.Context, f *Forge, repo string, n int) ([]PRNote, error) {
	if g.notesHold != nil {
		g.notesHold(n)
	}
	g.sm.Lock()
	defer g.sm.Unlock()
	return slices.Clone(g.notes), nil
}

func (g *shipForge) ReactTo(ctx context.Context, f *Forge, repo string, n int, kind, id, emoji string) error {
	g.sm.Lock()
	defer g.sm.Unlock()
	if kind == "review" {
		return ErrUnsupported
	}
	g.reactedTo = append(g.reactedTo, kind+":"+id+":"+emoji)
	return nil
}

func (g *shipForge) reacted() []string {
	g.sm.Lock()
	defer g.sm.Unlock()
	return slices.Clone(g.reactedTo)
}

func (g *shipForge) setNotes(notes ...PRNote) {
	g.sm.Lock()
	defer g.sm.Unlock()
	g.notes = notes
}

// bsEnv is a babysitting environment: a GitHub repo whose pull requests
// merge automatically with no waits, and an agent session that ships.
type bsEnv struct {
	*shipEnv
	agent  *Actor
	online bool
	away   bool
}

func newBabysitEnv(t *testing.T) *bsEnv { return newBabysitEnvOn(t, "github") }

func newBabysitEnvOn(t *testing.T, kind string) *bsEnv {
	se := newShipEnv(t, kind)
	se.llm.answers["opencode/muse-spark-1.3-contributor-free"] = noFindings
	se.noWaits()
	if _, err := se.s.UpdateForge(se.ctx, se.admin, se.f.ID, ForgeInput{MirrorAccount: str("mirror")}); err != nil {
		t.Fatal(err)
	}
	agent := *se.admin
	agent.Agent = "anthropic/claude-opus-5-5"
	if _, err := se.s.SessionHook(se.ctx, &agent, []HookEvent{{Event: "SessionStart", SessionID: "cli-1", Machine: "m1", CLI: "claude"}}); err != nil {
		t.Fatal(err)
	}
	agent.Session = se.s.SessionForHeader(se.ctx, se.admin.User.ID, "cli-1", "m1")
	if agent.Session == 0 {
		t.Fatal("no session")
	}
	return &bsEnv{shipEnv: se, agent: &agent}
}

// runnerOnline registers the admin's runner with Claude Code signed in.
func (be *bsEnv) runnerOnline() {
	yes := true
	be.online = true
	be.runner(be.admin, "laptop", func(r *RunnerRegistration) { r.CLIs = []RunnerCLI{{Name: "claude", Version: "2.1", SignedIn: &yes}} })
}

// bsPR is an agent's pull request (its body says Claude Code wrote it).
var bsPR = ForgePR{Number: 7, Title: "Add export", Author: "claude", Body: "Adds export.\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)",
	HeadRef: "add-export", HeadRepo: "me/app", HeadSHA: "7777777aaaa", State: "open"}

func (be *bsEnv) open() *PullRequest {
	be.t.Helper()
	be.pullHook("opened", bsPR)
	be.s.Wait()
	be.drain()
	return be.pr(7)
}

// push moves the pull request to a new head, reviewed.
func (be *bsEnv) push(sha string) {
	be.t.Helper()
	p := bsPR
	p.HeadSHA = sha
	be.pullHook("synchronize", p)
	be.s.Wait()
	be.drain()
	be.s.Wait()
}

func (be *bsEnv) start(pr *PullRequest) *Babysit {
	be.t.Helper()
	b, err := be.s.StartBabysit(be.ctx, be.agent, pr.ID, BabysitInput{Via: "ship"})
	if err != nil {
		be.t.Fatal(err)
	}
	be.s.Wait()
	return b
}

func (be *bsEnv) babysit() *Babysit {
	be.t.Helper()
	be.s.Wait()
	b, err := be.s.latestBabysit(be.ctx, be.s.db.R, be.pr(7).ID)
	if err != nil || b == nil {
		be.t.Fatalf("babysit: %v", err)
	}
	return b
}

// ci sets the head's one check and lets Waypoint read it. A failure is the
// code's, so it fails the babysitter's flake re-run too (WP-392): the round
// comes after that re-run's results.
func (be *bsEnv) ci(sha, state string) {
	be.t.Helper()
	be.sg.setChecks(sha, CommitCheck{Name: "ci / test", State: state, URL: "https://ci.example.com/run/1"})
	if be.online {
		be.runnerOnline()
	}
	from := be.clockNow().UnixMilli()
	be.poll()
	be.s.Wait()
	if state == CheckFailed && be.flakeRerunSince(sha, from) {
		be.rerunFails()
	}
}

// rerunFails: the babysitter's flake re-run of #7's head fails the same way
// (WP-392), so its round comes now.
func (be *bsEnv) rerunFails() {
	be.t.Helper()
	if fr := parseFlakeRerun(be.babysitOf(7).flakeRerun); !sameSHA(fr.SHA, be.pr(7).HeadSHA) {
		be.t.Fatalf("no flake re-run of %s: %+v", be.pr(7).HeadSHA, fr)
	}
	be.setClock(be.clockNow().Add(faultRerunSettle))
	if !be.away {
		if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE runners SET last_seen_at = ? WHERE last_seen_at IS NOT NULL`, be.clockNow().UnixMilli()); err != nil {
			be.t.Fatal(err)
		}
	}
	be.step()
}

// flakeRerunSince says whether the babysitter re-ran sha's failed checks to
// rule out a flake at or after from (ms).
func (be *bsEnv) flakeRerunSince(sha string, from int64) bool {
	be.t.Helper()
	b, err := be.s.latestBabysit(be.ctx, be.s.db.R, be.pr(7).ID)
	if err != nil || b == nil {
		return false
	}
	fr := parseFlakeRerun(b.flakeRerun)
	return sameSHA(fr.SHA, sha) && fr.At >= from
}

func (be *bsEnv) step() {
	be.t.Helper()
	be.s.babysitStep(be.ctx, be.pr(7).ID, true)
	be.s.Wait()
}

func (be *bsEnv) jobs() []int64 {
	be.t.Helper()
	rows, err := be.s.db.R.QueryContext(be.ctx, `SELECT id FROM runner_jobs WHERE source = 'babysit' ORDER BY id`)
	if err != nil {
		be.t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

func (be *bsEnv) jobInput(id int64) AgentJob {
	be.t.Helper()
	var raw string
	if err := be.s.db.R.QueryRowContext(be.ctx, `SELECT input FROM runner_jobs WHERE id = ?`, id).Scan(&raw); err != nil {
		be.t.Fatal(err)
	}
	var in AgentJob
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		be.t.Fatal(err)
	}
	return in
}

// finish ends a round's job as the runner would.
func (be *bsEnv) finish(id int64, pushed bool) {
	be.t.Helper()
	res, _ := json.Marshal(map[string]any{"pushed": pushed, "commits": []string{"abc"}, "commit": "abc1234", "answer": "Round over.", "diff": "+bounded the table\n"})
	if err := be.s.write(be.ctx, SystemActor(), func(t *tx) error { return t.finishRunnerJob(be.ctx, id, "done", nil, res, "") }); err != nil {
		be.t.Fatal(err)
	}
	be.s.Wait()
	be.s.Wait()
}

// told is what the shipping session has been told.
func (be *bsEnv) told() []string {
	be.t.Helper()
	rows, err := be.s.db.R.QueryContext(be.ctx, `SELECT body FROM session_messages WHERE session_id = ? ORDER BY id`, be.agent.Session)
	if err != nil {
		be.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		_ = rows.Scan(&b)
		out = append(out, b)
	}
	return out
}

func (be *bsEnv) notified(substr string) bool {
	be.t.Helper()
	var n int
	_ = be.s.db.R.QueryRowContext(be.ctx, `SELECT count(*) FROM notifications WHERE user_id = ? AND (title LIKE ? OR body LIKE ?)`,
		be.admin.User.ID, "%"+substr+"%", "%"+substr+"%").Scan(&n)
	return n > 0
}

func (be *bsEnv) comments() []*PRReviewComment {
	be.t.Helper()
	be.s.Wait()
	list, err := be.s.reviewComments(be.ctx, be.s.db.R, be.pr(7).ID)
	if err != nil {
		be.t.Fatal(err)
	}
	return list
}

func (be *bsEnv) comment(ext string) *PRReviewComment {
	be.t.Helper()
	for _, c := range be.comments() {
		if c.extID == ext {
			return c
		}
	}
	be.t.Fatalf("no comment %s", ext)
	return nil
}

func lastOf(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return list[len(list)-1]
}

func TestReviewCommentsFromTheForge(t *testing.T) {
	be := newBabysitEnv(t)
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	be.sg.threads = []ReviewThread{
		{ID: "T1", Path: "app/export.go", Line: 3, Comments: []ThreadComment{
			{ID: "501", Author: "alice", Body: "This loop never ends when rows is empty.", SHA: "7777777aaaa", At: at},
			{ID: "502", Author: "claude", Body: "Looking at it.", At: at},
			{ID: "503", Author: "waypoint-bot", Body: "Fixed.\n<!-- waypoint-reply -->", At: at},
		}},
		{ID: "T2", Path: "app/export.go", Line: 9, Comments: []ThreadComment{{ID: "511", Author: "dave", Body: "Typo: recieve.", At: at}}},
	}
	be.sg.setNotes(
		PRNote{ID: "601", Kind: "review", Author: "llm-review", Body: "Consider caching the export.", State: "commented", SHA: "7777777aaaa", At: at},
		PRNote{ID: "602", Kind: "review", Author: "bob", Body: "LGTM", State: "approved", At: at},
		PRNote{ID: "603", Kind: "comment", Author: "carol", Body: "Please add a test for the empty case.", At: at},
		PRNote{ID: "604", Kind: "comment", Author: "carol", Body: "@waypoint review", At: at},
		PRNote{ID: "605", Kind: "comment", Author: "claude", Body: "Ready for review.", At: at},
		PRNote{ID: "606", Kind: "review", Author: "erin", Body: "", State: "commented", At: at},
	)
	pr := be.open()
	if n := len(be.comments()); n != 0 {
		t.Fatalf("comments read before babysitting: %d", n)
	}
	be.start(pr)
	got := be.comments()
	var kinds []string
	for _, c := range got {
		kinds = append(kinds, c.Kind+":"+c.Author)
	}
	slices.Sort(kinds)
	if want := []string{"comment:carol", "inline:alice", "inline:dave", "review:llm-review"}; !slices.Equal(kinds, want) {
		t.Fatalf("comments = %v, want %v", kinds, want)
	}
	c := be.comment("501")
	if c.Path != "app/export.go" || c.Line != 3 || c.SHA != "7777777aaaa" || c.Verdict != VerdictNew || c.URL == "" || c.Bot {
		t.Errorf("inline comment = %+v", c)
	}
	if !be.comment("601").Bot {
		t.Error("llm-review's review isn't marked a bot's")
	}
	if r := be.reasons(7); !strings.Contains(r, "4 review comments not dealt with") {
		t.Errorf("readiness = %q", r)
	}

	be.sg.setNotes(
		PRNote{ID: "601", Kind: "review", Author: "llm-review", Body: "Consider caching the export.", State: "commented", SHA: "7777777aaaa", At: at},
		PRNote{ID: "603", Kind: "comment", Author: "carol", Body: "Please add a test for the empty and the nil case.", At: at, UpdatedAt: at.Add(time.Minute)},
	)
	be.sg.sm.Lock()
	be.sg.threads = be.sg.threads[:1]
	be.sg.sm.Unlock()
	be.sg.resolve(0, "alice")
	be.step()
	if c := be.comment("603"); !strings.Contains(c.Body, "nil case") || c.Verdict == VerdictDismissed {
		t.Errorf("edited = %+v", c)
	}
	if c := be.comment("511"); !c.Gone || !c.Dealt {
		t.Errorf("deleted = %+v", c)
	}
	if c := be.comment("501"); c.Verdict != VerdictDismissed || !c.Dealt {
		t.Errorf("resolved by a person = %+v", c)
	}
	var by string
	_ = be.s.db.R.QueryRowContext(be.ctx, `SELECT by_kind || ':' || by_name FROM review_comment_events WHERE comment_id = ? ORDER BY id DESC LIMIT 1`, be.comment("501").id).Scan(&by)
	if by != "forge:alice" {
		t.Errorf("dismissed by %q", by)
	}
	if r := be.reasons(7); !strings.Contains(r, "2 review comments not dealt with") {
		t.Errorf("readiness = %q", r)
	}
}

func TestUndealtCommentStates(t *testing.T) {
	cases := []struct {
		rc    PRReviewComment
		dealt bool
	}{
		{PRReviewComment{Verdict: VerdictNew}, false},
		{PRReviewComment{Verdict: VerdictSeen}, false},
		{PRReviewComment{Verdict: VerdictAccepted}, false},
		{PRReviewComment{Verdict: VerdictFixing}, false},
		{PRReviewComment{Verdict: VerdictNeedsPerson, Note: "a product call"}, false},
		{PRReviewComment{Verdict: VerdictRejected}, false},
		{PRReviewComment{Verdict: VerdictRejected, Note: "wrong"}, true},
		{PRReviewComment{Verdict: VerdictPartial, Note: "half"}, true},
		{PRReviewComment{Verdict: VerdictFixed}, true},
		{PRReviewComment{Verdict: VerdictVerified}, true},
		{PRReviewComment{Verdict: VerdictDismissed}, true},
		{PRReviewComment{Verdict: VerdictAccepted, Outdated: true}, true},
		{PRReviewComment{Verdict: VerdictSeen, Outdated: true}, false},
		{PRReviewComment{Verdict: VerdictSeen, Gone: true}, true},
		{PRReviewComment{Verdict: VerdictSeen, findingState: FindingFixed}, true},
		{PRReviewComment{Verdict: VerdictSeen, findingState: FindingOpen}, false},
	}
	for _, c := range cases {
		if got := c.rc.dealt(); got != c.dealt {
			t.Errorf("%+v dealt = %v, want %v", c.rc, got, c.dealt)
		}
	}
}

func TestBabysitFailedCheckRounds(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.sg.sm.Lock()
	be.sg.logs["ci / test"] = "ok  \tapp/db\n--- FAIL: TestExport (0.01s)\n    export_test.go:12: got 1, want 2\nFAIL\tapp\n"
	be.sg.sm.Unlock()
	be.ci("7777777aaaa", CheckFailed)
	b := be.babysit()
	jobs := be.jobs()
	if b.State != BabysitWorking || b.Round != 1 || len(jobs) != 1 || b.JobID != jobs[0] {
		t.Fatalf("after a failed check: %+v jobs %v", b, jobs)
	}
	var project int64
	_ = be.s.db.R.QueryRow(`SELECT coalesce(project_id, 0) FROM runner_jobs WHERE id = ?`, jobs[0]).Scan(&project)
	if project != be.p.ID {
		t.Errorf("the round's job works for project %d, want the repo's %d", project, be.p.ID)
	}
	in := be.jobInput(jobs[0])
	if in.CLI != "claude" || in.Model != babysitModel || in.Branch != "add-export" || in.Base != "add-export" || !in.Push || in.Permission != AgentEditRun ||
		!strings.Contains(in.Prompt, "--- FAIL: TestExport") || !strings.Contains(in.Prompt, "Reproduce each here") || !strings.Contains(in.Prompt, "round 1. This is branch") {
		t.Fatalf("job = %+v", in)
	}
	be.step()
	if n := len(be.jobs()); n != 1 {
		t.Fatalf("%d jobs for one head", n)
	}
	be.finish(jobs[0], false)
	b = be.babysit()
	if jobs = be.jobs(); b.State != BabysitWorking || b.Round != 2 || len(jobs) != 2 || b.Rounds[1].Model != escalateModel {
		t.Fatalf("after a round that changed nothing: %+v", b)
	}
	if r := b.Rounds[0]; r.Class != RoundNothingPushed || r.WorkClass != RoundCIConflict || len(r.Ledger) != 1 || r.Ledger[0].ID != "ci / test" {
		t.Errorf("round 1 ledger: %+v", r)
	}
	if r := b.Rounds[1]; r.Class != RoundCIConflict || len(r.Ledger) != 1 {
		t.Errorf("round 2 ledger: %+v", r)
	}
	if in := be.jobInput(jobs[1]); in.Model != escalateModel || !strings.Contains(in.Prompt, "Round 1 didn't fix the checks") ||
		!strings.Contains(in.Prompt, "Round over.") || !strings.Contains(in.Prompt, "--- FAIL: TestExport") {
		t.Fatalf("escalated job = %+v", in)
	}
	be.finish(jobs[1], false)
	b = be.babysit()
	if jobs = be.jobs(); b.State != BabysitWorking || b.Round != 3 || len(jobs) != 3 || !b.Rounds[2].Assessor || b.Rounds[2].Effort != assessorEffort {
		t.Fatalf("after the stronger round changed nothing: %+v", b)
	}
	if in := be.jobInput(jobs[2]); in.Model != escalateModel || in.Effort != assessorEffort || !strings.Contains(in.Prompt, "haven't settled it:\n<untrusted>\nround 2 (done, nothing pushed) left failing ci / test") ||
		!strings.Contains(in.Prompt, "Adds export.") || !strings.Contains(in.Prompt, "Round 1 on 7777777 (sonnet):\n<untrusted>\nGiven failing ci / test; done, nothing pushed.\n") ||
		!strings.Contains(in.Prompt, "--- FAIL: TestExport") {
		t.Fatalf("assessor job = %+v", in)
	}
	var timeout int
	_ = be.s.db.R.QueryRow(`SELECT timeout FROM runner_jobs WHERE id = ?`, jobs[2]).Scan(&timeout)
	if in := be.jobInput(jobs[2]); in.WorkTimeout != 5400 || timeout != 7200 {
		t.Errorf("assessor work timeout %d, job timeout %d", in.WorkTimeout, timeout)
	}
	be.finish(jobs[2], false)
	b = be.babysit()
	if b.State != BabysitResting || !strings.HasPrefix(b.Wait, restWait) || len(be.jobs()) != 3 || !resting(&b.Rounds[2]) {
		t.Fatalf("after the assessor changed nothing: %+v", b)
	}
	if be.notified("is stuck") || be.notified("can't merge until you answer") {
		t.Error("a person was asked about a failing check")
	}
	be.step()
	if len(be.jobs()) != 3 || !resting(lastRound(be.babysit())) {
		t.Fatalf("a resting babysit ran again: %+v", be.babysit())
	}
	be.push("8888888bbbb")
	be.ci("8888888bbbb", CheckFailed)
	if b = be.babysit(); b.Round != 4 || b.State != BabysitWorking || len(be.jobs()) != 4 || b.Rounds[3].Model != "" {
		t.Fatalf("round 4: %+v", b)
	}
	be.finish(b.JobID, true)
	be.push("9999999cccc")
	be.ci("9999999cccc", CheckFailed)
	b = be.babysit()
	if !resting(&b.Rounds[3]) || !b.Active || !strings.Contains(b.Rounds[3].Parked, "ci / test still open after the assessor's round 3") || len(be.jobs()) != 4 {
		t.Fatalf("second assessor on the same check: %+v", b)
	}
	if !be.s.babysitBlocks(be.ctx, pr.ID) {
		t.Error("a parked babysit doesn't hold the merge")
	}
	if _, err := be.s.StopBabysit(be.ctx, be.admin, pr.ID); err != nil {
		t.Fatal(err)
	}
	if be.s.babysitBlocks(be.ctx, pr.ID) {
		t.Error("stopped, it still holds the merge")
	}
}

// A pushed fix that doesn't clear the check: the next round goes to the
// stronger model, told that approach failed, before Waypoint gives up; a
// code-scanning failure says how to scan here (WP-251).
func TestBabysitEscalatesAfterAFixThatFailed(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.sg.setChecks("7777777aaaa", CommitCheck{Name: "forge-checks/codeql", State: CheckFailed})
	be.poll()
	be.rerunFails()
	b := be.babysit()
	if b.Round != 1 || b.Rounds[0].Model != "" {
		t.Fatalf("round 1: %+v", b)
	}
	in := be.jobInput(b.JobID)
	if in.Model != babysitModel || !strings.Contains(in.Prompt, "wp codeql scan") || !strings.Contains(in.Prompt, "Reproduce each here") {
		t.Fatalf("round 1 job = %+v", in)
	}
	be.finish(b.JobID, true)
	be.push("8888888bbbb")
	be.sg.setChecks("8888888bbbb", CommitCheck{Name: "forge-checks/codeql", State: CheckFailed})
	be.runnerOnline()
	be.poll()
	if b = be.babysit(); b.Round != 2 || b.State != BabysitWorking || b.Rounds[1].Model != escalateModel {
		t.Fatalf("round 2: %+v", b)
	}
	if r := b.Rounds[0]; r.Class != RoundCIConflict || r.PushSHA == "" {
		t.Errorf("round 1: class %q push %q", r.Class, r.PushSHA)
	}
	if r := b.Rounds[1]; r.Class != RoundRepeat || len(r.Ledger) != 1 || r.Ledger[0].Round != 1 {
		t.Errorf("round 2 ledger: %+v", r)
	}
	st, err := be.s.GetRoundStats(be.ctx, be.admin, "", 7, false)
	if err != nil || st.Rounds != 2 || st.PRs != 1 || st.Classed != 2 || st.Class[RoundRepeat] != 1 || st.Class[RoundCIConflict] != 1 {
		t.Errorf("round stats: %+v %v", st, err)
	}
	if in := be.jobInput(b.JobID); in.Model != escalateModel || !strings.Contains(in.Prompt, "Earlier rounds pushed fixes for forge-checks/codeql, and it still fails") ||
		!strings.Contains(in.Prompt, "Round 1 on 7777777:\n<untrusted>\n```diff\n+bounded the table\n```\nRound over.") || strings.Contains(in.Prompt, "git log") {
		t.Fatalf("escalated job = %+v", in)
	}
	be.finish(b.JobID, true)
	be.push("9999999cccc")
	be.sg.setChecks("9999999cccc", CommitCheck{Name: "forge-checks/codeql", State: CheckFailed})
	be.runnerOnline()
	be.poll()
	if b = be.babysit(); b.State != BabysitWorking || len(be.jobs()) != 3 || !b.Rounds[2].Assessor {
		t.Fatalf("after the stronger round's fix failed: %+v", b)
	}
	if in := be.jobInput(b.JobID); in.Effort != assessorEffort || !strings.Contains(in.Prompt, "forge-checks/codeql failed 3 rounds in a row") ||
		!strings.Contains(in.Prompt, "Round 2 on 8888888 (opus):\n<untrusted>\nGiven failing forge-checks/codeql; pushed 1 commit.\n") {
		t.Fatalf("assessor job = %+v", in)
	}
	be.finish(b.JobID, true)
	be.push("aaaaaaa1111")
	be.sg.setChecks("aaaaaaa1111", CommitCheck{Name: "forge-checks/codeql", State: CheckFailed})
	be.runnerOnline()
	be.poll()
	be.step()
	if b = be.babysit(); !resting(&b.Rounds[2]) || len(be.jobs()) != 3 || !strings.Contains(b.Rounds[2].Parked, "forge-checks/codeql") {
		t.Fatalf("after the assessor's fix failed: %+v", b)
	}
	if why, _ := be.s.babysitParked(be.ctx, be.pr(7)); why == "" {
		t.Error("resting, it doesn't hold the merge")
	}
}

// The forge cancelled a superseded head's run: no round, no streak (WP-153).
func TestBabysitIgnoresSupersededHeadFailure(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	be.fg.mu.Lock()
	be.fg.prs[mk(be.f, "me/app", 7)].HeadSHA = "8888888bbbb"
	be.fg.mu.Unlock()
	be.ci("7777777aaaa", CheckFailed)
	be.step()
	if b := be.babysit(); b.Round != 0 || len(be.jobs()) != 0 || len(b.streaks) != 0 {
		t.Fatalf("a round for a superseded head: %+v jobs %v", b, be.jobs())
	}
	if got := be.pr(7).HeadSHA; got != "8888888bbbb" {
		t.Errorf("head not followed: %s", got)
	}
}

func TestBabysitRoundLimit(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	be.start(pr)
	if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE babysits SET max_rounds = 1`); err != nil {
		t.Fatal(err)
	}
	be.ci("7777777aaaa", CheckFailed)
	be.finish(be.babysit().JobID, true)
	be.push("8888888bbbb")
	be.sg.setChecks("8888888bbbb", CommitCheck{Name: "ci / lint", State: CheckFailed})
	be.poll()
	be.rerunFails()
	b := be.babysit()
	if b.State != BabysitWorking || !b.Rounds[1].Assessor || !strings.Contains(be.jobInput(b.JobID).Prompt, "1 round, the most a babysit runs, and still failing ci / lint") {
		t.Fatalf("after the last round: %+v", b)
	}
	be.finish(b.JobID, false)
	if b = be.babysit(); !resting(&b.Rounds[1]) || len(be.jobs()) != 2 || !strings.Contains(b.Rounds[1].Parked, "the assessor's round 2 on this head") {
		t.Fatalf("after the assessor past the cap: %+v", b)
	}
}

func TestBabysitCommentsRound(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	be.sg.threads = []ReviewThread{{ID: "T1", Path: "app/export.go", Line: 4, Comments: []ThreadComment{
		{ID: "501", Author: "alice", Body: "This loop never ends when rows is empty.", SHA: "7777777aaaa", At: at}}}}
	be.sg.setNotes(
		PRNote{ID: "601", Kind: "review", Author: "llm-review", Body: "Consider caching the export.", State: "commented", SHA: "7777777aaaa", At: at},
		PRNote{ID: "603", Kind: "comment", Author: "carol", Body: "Should exports include archived rows?", At: at},
	)
	be.sg.sm.Lock()
	be.sg.fileAt["7777777aaaa:app/export.go"] = "package app\n\nfunc Export(rows []Row) {\n\tfor {\n\t\tsend(rows)\n\t}\n}\n"
	be.sg.sm.Unlock()
	pr := be.open()
	be.start(pr)
	be.ci("7777777aaaa", CheckSuccess)
	if m := be.sg.merged(); len(m) != 0 {
		t.Fatalf("merged with comments not dealt with: %+v", m)
	}
	r := be.sg.reacted()
	for _, want := range []string{"inline:501:eyes", "comment:603:eyes"} {
		if !slices.Contains(r, want) {
			t.Errorf("reactions %v lack %s", r, want)
		}
	}
	for _, c := range be.comments() {
		if c.Verdict != VerdictSeen {
			t.Errorf("%s is %s, want seen", c.Ref, c.Verdict)
		}
	}
	b := be.babysit()
	jobs := be.jobs()
	if b.State != BabysitWorking || len(jobs) != 1 {
		t.Fatalf("no round for comments: %+v", b)
	}
	alice, review, carol := be.comment("501"), be.comment("601"), be.comment("603")
	in := be.jobInput(jobs[0])
	for _, want := range []string{alice.Ref + " comment by alice, app/export.go:4", ">     4  \tfor {", "Should exports include archived rows?", review.Ref + " review by llm-review"} {
		if !strings.Contains(in.Prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, in.Prompt)
		}
	}

	if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{alice.Ref}, Verdict: "accept"}); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{review.Ref}, Verdict: VerdictRejected}); !IsInvalid(err) {
		t.Errorf("a rejection without a reason: %v", err)
	}
	if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{review.Ref}, Verdict: VerdictRejected, Note: "It runs once a day; a cache buys nothing."}); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.AckReviewComments(be.ctx, be.admin, AckInput{IDs: []string{carol.Ref}, Verdict: "needs-person", Note: "Whether archived rows are exported is a product call."}); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.AckReviewComments(be.ctx, be.agent, AckInput{IDs: []string{carol.Ref}, Verdict: VerdictAccepted}); !IsInvalid(err) {
		t.Errorf("an agent overruled a person's own verdict: %v", err)
	}
	if _, err := be.s.FixedReviewComments(be.ctx, be.agent, FixedInput{IDs: []string{alice.Ref}, SHA: "8888888bbbb"}); err != nil {
		t.Fatal(err)
	}
	r = be.sg.reacted()
	for _, want := range []string{"inline:501:+1", "inline:501:rocket", "comment:603:confused"} {
		if !slices.Contains(r, want) {
			t.Errorf("reactions %v lack %s", r, want)
		}
	}
	if slices.ContainsFunc(r, func(s string) bool { return strings.HasPrefix(s, "review:") }) {
		t.Errorf("an emoji on a review's body: %v", r)
	}
	if c := be.comment("501"); c.Verdict != VerdictFixing || c.FixSHA != "8888888bbbb" {
		t.Errorf("fixed before the push = %+v", c)
	}
	posted := strings.Join(be.sg.posted, "\n")
	if !strings.Contains(posted, "@llm-review Not changing this: It runs once a day") || !strings.Contains(posted, "> Consider caching the export.") ||
		!strings.Contains(posted, "@carol This needs a person: Whether archived rows") {
		t.Errorf("replies = %q", posted)
	}
	if len(be.sg.replies) != 0 {
		t.Errorf("replied in the thread of an accepted comment: %v", be.sg.replies)
	}

	be.finish(jobs[0], true)
	be.push("8888888bbbb")
	if c := be.comment("501"); c.Verdict != VerdictFixed {
		t.Errorf("after the push = %+v", c)
	}
	if !slices.Contains(be.sg.reacted(), "inline:501:hooray") || !slices.Contains(be.sg.resolved, "T1") || len(be.sg.replies) != 0 {
		t.Errorf("fixed: reactions %v resolved %v replies %v", be.sg.reacted(), be.sg.resolved, be.sg.replies)
	}
	be.ci("8888888bbbb", CheckSuccess)
	b = be.babysit()
	if b.State != BabysitNeedsPerson || !strings.Contains(b.Wait, carol.Ref) || len(be.jobs()) != 1 {
		t.Fatalf("a comment for a person: %+v", b)
	}
	if !strings.Contains(lastOf(be.told()), "need") || !be.notified("needs you") {
		t.Errorf("not told: %v", be.told())
	}
	if _, err := be.s.DismissReviewComment(be.ctx, be.admin, carol.Ref, "Not for this pull request."); err != nil {
		t.Fatal(err)
	}
	be.s.Wait()
	be.step()
	if m := be.sg.merged(); len(m) != 1 {
		t.Fatalf("not merged: %v %q", m, be.reasons(7))
	}
	be.step()
	if b := be.babysit(); b.State != BabysitMerged || b.Active {
		t.Fatalf("after the merge: %+v", b)
	}
	if m := lastOf(be.told()); !strings.Contains(m, "merged") {
		t.Errorf("told %q", m)
	}
	if txt := MessagesText([]*SessionMessage{{Body: "x merged", stepKey: "babysit:1:merged"}}); !strings.HasPrefix(txt, "Waypoint's babysitter: x merged") {
		t.Errorf("message text = %q", txt)
	}
}

func TestBabysitHandsOverWithoutRunner(t *testing.T) {
	be := newBabysitEnv(t)
	pr := be.open()
	be.start(pr)
	be.ci("7777777aaaa", CheckFailed)
	b := be.babysit()
	if b.State != BabysitNeedsPerson || b.Round != 1 || len(be.jobs()) != 0 || len(b.Rounds) != 1 || b.Rounds[0].To != "session" {
		t.Fatalf("without a runner: %+v", b)
	}
	m := lastOf(be.told())
	if !strings.Contains(m, "no runner of") || !strings.Contains(m, "wp pr todo 7") || !strings.Contains(m, "failing ci / test") {
		t.Errorf("told %q", m)
	}
	if !be.notified("wp pr todo 7") {
		t.Error("the owner wasn't notified")
	}
	be.step()
	if n := len(be.told()); n != 1 {
		t.Errorf("told %d times", n)
	}
	be.runnerOnline()
	fork := bsPR
	fork.Number, fork.HeadRepo, fork.HeadSHA = 8, "someone/app", "8888888aaaa"
	be.pullHook("opened", fork)
	be.s.Wait()
	be.drain()
	p8 := be.pr(8)
	if why := be.s.babysitGate(be.ctx, p8); !strings.Contains(why, "fork") {
		t.Errorf("fork gate = %q", why)
	}
}

// A runner that is briefly away (restarting on a new wp, a missed
// heartbeat, a sign-in check that ran slow) gets the round when it is back;
// only one away past the grace hands the round to a person (WP-501).
func TestBabysitWaitsForItsRunner(t *testing.T) {
	away := func(t *testing.T) *bsEnv {
		be := newBabysitEnv(t)
		pr := be.open()
		be.runnerOnline()
		be.online, be.away = false, true
		be.setClock(be.clockNow().Add(2 * time.Minute))
		be.start(pr)
		be.ci("7777777aaaa", CheckFailed)
		b := be.babysit()
		if b.State != BabysitWatching || !strings.Contains(b.Wait, "waiting for a runner of") || len(b.Rounds) != 0 || len(be.jobs()) != 0 {
			t.Fatalf("runner away: %+v", b)
		}
		for _, m := range be.told() {
			if strings.Contains(m, "wp pr todo") {
				t.Fatalf("handed over while the runner was away: %q", m)
			}
		}
		return be
	}
	t.Run("back", func(t *testing.T) {
		be := away(t)
		be.setClock(be.clockNow().Add(5 * time.Minute))
		be.away = false
		be.runnerOnline()
		be.step()
		b := be.babysit()
		if b.State != BabysitWorking || len(b.Rounds) != 1 || b.Rounds[0].To != "runner" || len(be.jobs()) != 1 {
			t.Fatalf("runner back: %+v", b)
		}
	})
	t.Run("signed out", func(t *testing.T) {
		be := away(t)
		no := false
		be.runner(be.admin, "laptop", func(r *RunnerRegistration) { r.CLIs = []RunnerCLI{{Name: "claude", Version: "2.1", SignedIn: &no}} })
		be.step()
		if b := be.babysit(); b.State != BabysitWatching || len(b.Rounds) != 0 {
			t.Fatalf("online but signed out: %+v", b)
		}
	})
	t.Run("gone", func(t *testing.T) {
		be := away(t)
		be.setClock(be.clockNow().Add(babysitRunnerGrace + time.Minute))
		be.step()
		b := be.babysit()
		if b.State != BabysitNeedsPerson || len(b.Rounds) != 1 || b.Rounds[0].To != "session" || !strings.Contains(b.Rounds[0].Why, "waited 30 minutes") {
			t.Fatalf("runner gone: %+v", b)
		}
		if !strings.Contains(lastOf(be.told()), "wp pr todo 7") {
			t.Errorf("told %q", be.told())
		}
	})
}

func TestBabysitVerifiedHeart(t *testing.T) {
	be := newBabysitEnv(t)
	be.llm.answers["opencode/muse-spark-1.3-contributor-free"] = oneFinding
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	be.sg.threads = []ReviewThread{{ID: "T9", Path: "app/export.go", Line: 2, Comments: []ThreadComment{
		{ID: "901", Author: "llm-review", Body: "Close the file.", SHA: "7777777aaaa", At: at}}}}
	pr := be.open()
	be.start(pr)
	list := be.comments()
	var finding *PRReviewComment
	for _, c := range list {
		if c.FindingID != 0 {
			finding = c
		}
	}
	if finding == nil || finding.Author != "waypoint" || !finding.Bot {
		t.Fatalf("Waypoint's finding isn't a review comment: %+v", list)
	}
	if err := be.s.write(be.ctx, SystemActor(), func(t *tx) error {
		_, err := be.s.setFindingState(be.ctx, t, finding.FindingID, FindingFixed, byVerify("m"), "8888888bbbb", "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	be.step()
	if c := be.comment(finding.extID); c.Verdict != VerdictVerified || !slices.Contains(be.sg.reacted(), "inline:"+finding.extID+":heart") {
		t.Errorf("finding fixed = %+v %v", c, be.sg.reacted())
	}
	llm := be.comment("901")
	if _, err := be.s.FixedReviewComments(be.ctx, be.agent, FixedInput{IDs: []string{llm.Ref}, Note: "Closed with defer."}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(be.sg.replies, "\n"), "T9: Fixed in `7777777`: Closed with defer.") {
		t.Errorf("no reply with the note: %v", be.sg.replies)
	}
	be.step()
	if c := be.comment("901"); c.Verdict != VerdictFixed {
		t.Fatalf("verified before a second look: %+v", c)
	}
	be.setClock(be.clockNow().Add(time.Minute))
	if _, err := be.s.db.W.ExecContext(be.ctx, `INSERT INTO pr_events(pr_id, kind, actor, bot, sha, created_at) VALUES (?, 'reviewed', 'llm-review', 1, '8888888bbbb', ?)`,
		pr.ID, be.s.nowMS()); err != nil {
		t.Fatal(err)
	}
	be.step()
	if c := be.comment("901"); c.Verdict != VerdictVerified {
		t.Errorf("not verified after the bot's next review: %+v", c)
	}
	var who string
	_ = be.s.db.R.QueryRowContext(be.ctx, `SELECT by_kind || ':' || by_name FROM review_comment_events WHERE comment_id = ? ORDER BY id DESC LIMIT 1`, llm.id).Scan(&who)
	if who != "verify:llm-review" {
		t.Errorf("verified by %q", who)
	}
}

func TestBabysitStartStop(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	pr := be.open()
	b1 := be.start(pr)
	b2, err := be.s.StartBabysit(be.ctx, be.admin, pr.ID, BabysitInput{Model: "opus"})
	if err != nil || b2.ID != b1.ID || b2.Model != "opus" {
		t.Fatalf("start again = %+v %v", b2, err)
	}
	if _, err := be.s.StartBabysit(be.ctx, be.admin, pr.ID, BabysitInput{Model: "no spaces allowed"}); !IsInvalid(err) {
		t.Errorf("bad model: %v", err)
	}
	viewer := be.user("vic", false)
	if err := be.s.SetMember(be.ctx, be.admin, be.p.Key, viewer.User.Username, "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := be.s.BabysitStatus(be.ctx, viewer, pr.ID); err != nil {
		t.Errorf("viewer status: %v", err)
	}
	if _, err := be.s.StopBabysit(be.ctx, viewer, pr.ID); err == nil {
		t.Error("a viewer stopped it")
	}
	be.ci("7777777aaaa", CheckFailed)
	job := be.babysit().JobID
	v, err := be.s.StopBabysit(be.ctx, be.admin, pr.ID)
	if err != nil || v.State != BabysitStopped || v.Active {
		t.Fatalf("stop = %+v %v", v, err)
	}
	var state string
	_ = be.s.db.R.QueryRowContext(be.ctx, `SELECT state FROM runner_jobs WHERE id = ?`, job).Scan(&state)
	if state != "cancelled" {
		t.Errorf("the round's job is %s", state)
	}
	if !strings.Contains(lastOf(be.told()), "stopped it") {
		t.Errorf("told %v", be.told())
	}
	if _, err := be.s.StopBabysit(be.ctx, be.admin, pr.ID); !IsInvalid(err) {
		t.Errorf("stop twice: %v", err)
	}
	st, err := be.s.BabysitStatus(be.ctx, be.admin, pr.ID)
	if err != nil || st.Babysit == nil || st.Babysit.State != BabysitStopped || !st.CanEdit {
		t.Errorf("status = %+v %v", st, err)
	}
}

func TestBabysitAutomationStep(t *testing.T) {
	be := newBabysitEnv(t)
	if d := actionDef("babysit"); d == nil || !d.Runner {
		t.Fatalf("no babysit action: %+v", d)
	}
	pr := be.open()
	a := &Automation{ID: 1, ProjectID: be.p.ID, Name: "Babysit agents' pull requests", savedBy: be.admin.User.ID}
	actor := &Actor{User: be.admin.User, Automation: a}
	b, err := be.s.StartBabysit(be.ctx, actor, pr.ID, BabysitInput{})
	if err != nil || b.Via != "automation" || b.Owner.ID != be.admin.User.ID {
		t.Fatalf("automation start = %+v %v", b, err)
	}
}

// An armed pull request is always babysat (WP-188): arming starts it on the
// armer's (or the author's) runner, and the minute sweep takes in armed ones
// that failed with no babysit; never a fork's, nor one a person stopped.
func TestBabysitArmedPullRequests(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	author := be.user("claude", false)
	if err := be.s.SetMember(be.ctx, be.admin, be.p.Key, author.User.Username, "editor"); err != nil {
		t.Fatal(err)
	}
	arm := func(pr *PullRequest, by any) {
		t.Helper()
		if _, err := be.s.db.W.ExecContext(be.ctx, `UPDATE pull_requests SET armed_at = ?, armed_by = ?, armed_agent = '' WHERE id = ?`, be.s.nowMS(), by, pr.ID); err != nil {
			t.Fatal(err)
		}
	}
	forget := func(pr *PullRequest) {
		t.Helper()
		if _, err := be.s.db.W.ExecContext(be.ctx, `DELETE FROM babysits WHERE pr_id = ?`, pr.ID); err != nil {
			t.Fatal(err)
		}
	}
	latest := func(pr *PullRequest) *Babysit {
		t.Helper()
		be.s.Wait()
		b, err := be.s.latestBabysit(be.ctx, be.s.db.R, pr.ID)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	pr := be.open()
	forget(pr)

	arm(pr, be.admin.User.ID)
	be.s.maybeAutoMerge(be.ctx, pr.ID)
	if b := latest(pr); b == nil || b.ownerID != be.admin.User.ID || !b.Active || b.StartedBy != "" {
		t.Fatalf("armed by a person: %+v", b)
	}
	forget(pr)
	arm(pr, nil)
	be.s.maybeAutoMerge(be.ctx, pr.ID)
	if b := latest(pr); b == nil || b.ownerID != author.User.ID {
		t.Fatalf("armed with nobody: %+v", b)
	}
	if _, err := be.s.StopBabysit(be.ctx, be.admin, pr.ID); err != nil {
		t.Fatal(err)
	}
	be.s.maybeAutoMerge(be.ctx, pr.ID)
	if b := latest(pr); b.Active {
		t.Fatalf("started again after a stop: %+v", b)
	}

	forget(pr)
	be.s.enrollArmedTroubled(be.ctx)
	if b := latest(pr); b != nil {
		t.Fatalf("enrolled with nothing wrong: %+v", b)
	}
	be.sg.setChecks("7777777aaaa", CommitCheck{Name: "ci / test", State: CheckFailed})
	be.poll()
	forget(pr)
	be.s.enrollArmedTroubled(be.ctx)
	if b := latest(pr); b == nil || !b.Active {
		t.Fatalf("sweep: %+v", b)
	}

	fork := bsPR
	fork.Number, fork.HeadRef, fork.HeadRepo = 8, "fork-branch", "stranger/app"
	be.pullHook("opened", fork)
	be.s.Wait()
	fpr := be.pr(8)
	arm(fpr, be.admin.User.ID)
	be.s.maybeAutoMerge(be.ctx, fpr.ID)
	if b := latest(fpr); b != nil {
		t.Fatalf("a fork's pull request was enrolled: %+v", b)
	}
}

// A reviewer that posts after a round on the same head (CodeRabbit's review
// following its "processing" placeholder) is new work for one more round, not
// a person's: the round never saw it.
func TestBabysitLateCommentRound(t *testing.T) {
	be := newBabysitEnv(t)
	be.runnerOnline()
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	placeholder := PRNote{ID: "601", Kind: "comment", Author: "carol", Body: "Review in progress; findings follow.", At: at}
	be.sg.setNotes(placeholder)
	pr := be.open()
	be.start(pr)
	be.ci("7777777aaaa", CheckSuccess)
	b, jobs := be.babysit(), be.jobs()
	if b.State != BabysitWorking || len(jobs) != 1 {
		t.Fatalf("no round for the comment: %+v", b)
	}
	be.finish(jobs[0], false)
	if b = be.babysit(); b.State != BabysitWorking || len(be.jobs()) != 2 || b.Rounds[1].Model != escalateModel {
		t.Fatalf("after round 1 left it: %+v", b)
	}
	if in := be.jobInput(b.JobID); !strings.Contains(in.Prompt, "Round 1 left this work open") {
		t.Fatalf("round 2 prompt:\n%s", in.Prompt)
	}

	be.sg.setNotes(placeholder, PRNote{ID: "602", Kind: "comment", Author: "carol", Body: "Export never closes the file.", At: at.Add(time.Minute)})
	be.runnerOnline()
	be.poll()
	be.finish(b.JobID, false)
	be.step()
	b, jobs = be.babysit(), be.jobs()
	if b.State != BabysitWorking || b.Round != 3 || len(jobs) != 3 || b.Rounds[2].Model != "" {
		t.Fatalf("a comment after the round: %+v", b)
	}
	late := be.comment("602")
	if in := be.jobInput(jobs[2]); !strings.Contains(in.Prompt, "Export never closes the file.") || !slices.Contains(b.Rounds[2].Comments, late.Ref) {
		t.Fatalf("round 3 = %+v, prompt:\n%s", b.Rounds[2], in.Prompt)
	}
	be.finish(jobs[2], false)
	be.step()
	if b = be.babysit(); b.State != BabysitWorking || len(be.jobs()) != 4 || !b.Rounds[3].Assessor {
		t.Fatalf("after round 3 left it: %+v", b)
	}
}
