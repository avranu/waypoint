package core

import (
	"strings"
	"testing"
	"time"
)

// Tasks an agent files for another repo (WP-378).

type crossEnv struct {
	*env
	a, b *Project
}

func newCrossEnv(t *testing.T) *crossEnv {
	e := newEnv(t)
	e.s.Forges = newMemForge()
	f := e.forge("forgejo", "https://git.example.com", false)
	ce := &crossEnv{env: e, a: e.project(e.admin, "Alpha", "kanban"), b: e.project(e.admin, "Beta", "kanban")}
	for p, repo := range map[*Project]string{ce.a: "me/a", ce.b: "me/b"} {
		if _, err := e.s.AddProjectRepo(e.ctx, e.admin, p.Key, f.ID, repo, false); err != nil {
			t.Fatal(err)
		}
	}
	return ce
}

// session starts an agent session working in repo.
func (ce *crossEnv) session(cli, repo string) *Actor {
	a, _ := ce.start(cli, repo)
	return a
}

// start is session, with what the session was told as it started.
func (ce *crossEnv) start(cli, repo string) (*Actor, *HookResult) {
	ce.t.Helper()
	a := *ce.admin
	a.Agent = "anthropic/claude-opus-5-5"
	res := ce.hookOn(&a, cli, HookEvent{Event: "SessionStart", Repo: repo})
	a.Session = ce.s.SessionForHeader(ce.ctx, ce.admin.User.ID, cli, "m1")
	if a.Session == 0 {
		ce.t.Fatal("no session")
	}
	return &a, res
}

func (ce *crossEnv) hookOn(a *Actor, cli string, ev HookEvent) *HookResult {
	ce.t.Helper()
	ev.SessionID, ev.Machine, ev.CLI = cli, "m1", "claude"
	res, err := ce.s.SessionHook(ce.ctx, a, []HookEvent{ev})
	if err != nil {
		ce.t.Fatal(err)
	}
	ce.s.Wait()
	return res
}

func TestCrossFiledTaskIsMarkedAndDeclined(t *testing.T) {
	ce := newCrossEnv(t)
	inA := ce.session("cli-a", "me/a")
	inB := ce.session("cli-b", "me/b")

	filed := ce.issue(inA, IssueInput{Project: ce.b.Key, Title: "Add a dashboard"})
	own := ce.issue(inB, IssueInput{Project: ce.b.Key, Title: "Tidy logs"})
	ce.s.Wait()

	if f := ce.get(inA, filed.Key).FiledFrom; f == nil || !f.Here {
		t.Fatalf("the filer reads it as its own side's: %+v", f)
	}
	f := ce.get(inB, filed.Key).FiledFrom
	if f == nil || f.Here || f.Project != ce.a.Key || f.Repo != "me/a" {
		t.Fatalf("B's agents should see it came from A: %+v", f)
	}
	if ce.get(inB, own.Key).FiledFrom != nil || ce.get(inA, own.Key).FiledFrom != nil {
		t.Error("a task filed in its own repo isn't cross-filed")
	}

	if _, err := ce.s.DeclineIssue(ce.ctx, inB, filed.Key, "", " "); err == nil {
		t.Error("a decline without a reason")
	}
	if _, err := ce.s.DeclineIssue(ce.ctx, inB, own.Key, "", "not mine"); err == nil || !strings.Contains(err.Error(), "wp release") {
		t.Errorf("declined a task filed here: %v", err)
	}
	i, err := ce.s.DeclineIssue(ce.ctx, inB, filed.Key, "", "B has no UI; the dashboard belongs in A")
	if err != nil {
		t.Fatal(err)
	}
	ce.s.Wait()
	if i.Status.Category != CatCanceled {
		t.Errorf("declined but %s", i.Status.Name)
	}
	if d := ce.get(ce.admin, filed.Key).FiledFrom; d == nil || d.DeclinedAt == nil || !strings.Contains(d.Declined, "no UI") {
		t.Errorf("the decline isn't on the task: %+v", d)
	}
	if _, err := ce.s.DeclineIssue(ce.ctx, inB, filed.Key, "", "again"); err == nil {
		t.Error("declined twice")
	}

	// The filer hears with its next turn, not by being woken at a stop.
	if res := ce.hookOn(inA, "cli-a", HookEvent{Event: "Stop"}); res.Text != "" {
		t.Fatalf("a decline woke the filer: %q", res.Text)
	}
	res := ce.hookOn(inA, "cli-a", HookEvent{Event: "UserPromptSubmit", Prompt: "go on"})
	if !strings.Contains(res.Text, filed.Key) || !strings.Contains(res.Text, "no UI") {
		t.Fatalf("the filer wasn't told: %q", res.Text)
	}
	if res := ce.hookOn(inA, "cli-a", HookEvent{Event: "UserPromptSubmit", Prompt: "and?"}); strings.Contains(res.Text, filed.Key) {
		t.Errorf("told twice: %q", res.Text)
	}
	if ce.hookOn(inB, "cli-b", HookEvent{Event: "UserPromptSubmit", Prompt: "next"}).Text != "" {
		t.Error("the decliner's side was told its own decline")
	}
}

func TestCrossFiledDeclineWaitsForTheNextSessionThere(t *testing.T) {
	ce := newCrossEnv(t)
	inA := ce.session("cli-a", "me/a")
	filed := ce.issue(inA, IssueInput{Project: ce.b.Key, Title: "Add a dashboard"})
	ce.s.Wait()
	ce.setClock(ce.clockNow().Add(25 * time.Hour)) // the filer is long gone
	inB := ce.session("cli-b", "me/b")
	if _, err := ce.s.DeclineIssue(ce.ctx, inB, filed.Key, "", "belongs in A"); err != nil {
		t.Fatal(err)
	}
	ce.s.Wait()
	// The next agent in A's repo hears it once; one in B doesn't.
	if _, res := ce.start("cli-b2", "me/b"); strings.Contains(res.Text, filed.Key) {
		t.Errorf("told in B: %q", res.Text)
	}
	next, res := ce.start("cli-a2", "me/a")
	if !strings.Contains(res.Text, filed.Key) || !strings.Contains(res.Text, "belongs in A") {
		t.Fatalf("the next session in A wasn't told: %q", res.Text)
	}
	if res := ce.hookOn(next, "cli-a2", HookEvent{Event: "UserPromptSubmit", Prompt: "hi"}); strings.Contains(res.Text, filed.Key) {
		t.Errorf("told twice: %q", res.Text)
	}
	if _, res := ce.start("cli-a3", "me/a"); strings.Contains(res.Text, filed.Key) {
		t.Errorf("told another session too: %q", res.Text)
	}
}

func TestCrossFiledNewsReachesWhoNamedIt(t *testing.T) {
	ce := newCrossEnv(t)
	inA := ce.session("cli-a", "me/a")
	filed := ce.issue(inA, IssueInput{Project: ce.b.Key, Title: "Add a dashboard"})
	ce.s.Wait()
	// Another session in A says it waits on it; the filer's ended.
	ce.setClock(ce.clockNow().Add(25 * time.Hour))
	waiter := ce.session("cli-w", "me/a")
	ce.hookOn(waiter, "cli-w", HookEvent{Event: "UserPromptSubmit", Prompt: "carry on once " + strings.ToLower(filed.Key) + " is in, i mean " + filed.Key})
	inB := ce.session("cli-b", "me/b")
	done := CatDone
	if _, err := ce.s.UpdateIssue(ce.ctx, inB, filed.Key, IssuePatch{Status: &done}); err != nil {
		t.Fatal(err)
	}
	ce.s.Wait()
	res := ce.hookOn(waiter, "cli-w", HookEvent{Event: "UserPromptSubmit", Prompt: "status?"})
	if !strings.Contains(res.Text, filed.Key) || !strings.Contains(res.Text, "is done") {
		t.Fatalf("the session that named it wasn't told: %q", res.Text)
	}
	// Done is news only for who still runs: nothing left for later.
	if _, res := ce.start("cli-a2", "me/a"); strings.Contains(res.Text, filed.Key) {
		t.Errorf("done left as a notice: %q", res.Text)
	}
}

func TestCrossFiledDeclineReachesWhatItBlocks(t *testing.T) {
	ce := newCrossEnv(t)
	inA := ce.session("cli-a", "me/a")
	filed := ce.issue(inA, IssueInput{Project: ce.b.Key, Title: "Add an export API"})
	mine := ce.issue(inA, IssueInput{Project: ce.a.Key, Title: "Use the export API"})
	if err := ce.s.AddRelation(ce.ctx, inA, filed.Key, "blocks", mine.Key); err != nil {
		t.Fatal(err)
	}
	// The filer holds the task that waits on it.
	if _, err := ce.s.db.W.ExecContext(ce.ctx, `UPDATE agent_sessions SET issue_id = ? WHERE id = ?`, mine.ID, inA.Session); err != nil {
		t.Fatal(err)
	}
	ce.s.Wait()
	inB := ce.session("cli-b", "me/b")
	if _, err := ce.s.DeclineIssue(ce.ctx, inB, filed.Key, "", "B won't expose that"); err != nil {
		t.Fatal(err)
	}
	ce.s.Wait()
	var blocking bool
	var batch *int64
	if err := ce.s.db.R.QueryRowContext(ce.ctx, `SELECT passive = 0, batch_until FROM session_messages WHERE session_id = ? AND step_key LIKE 'crossrepo:%'`, inA.Session).Scan(&blocking, &batch); err != nil {
		t.Fatal(err)
	}
	if !blocking || batch == nil {
		t.Errorf("news its held task waits on should be batched to wake it: active %v, batch %v", blocking, batch)
	}
	found := false
	for _, c := range ce.comments(mine.Key) {
		if strings.Contains(c.Body, filed.Key) && strings.Contains(c.Body, "won't expose") {
			found = true
		}
	}
	if !found {
		t.Error("the blocked task wasn't told")
	}
}
