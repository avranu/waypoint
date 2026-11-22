package core

import (
	"strings"
	"testing"
	"time"
)

func TestAgentMatches(t *testing.T) {
	id := "anthropic/claude-haiku-5-5"
	for spec, want := range map[string]bool{
		"any": true, "anthropic": true, "claude": true, "haiku": true, "haiku 5.5": true, "Haiku-5.5": true,
		"anthropic/claude-haiku-5-5": true, "sonnet": false, "haiku 4": false, "": false, "openai": false,
	} {
		if got := AgentMatches(spec, id); got != want {
			t.Errorf("AgentMatches(%q, %q) = %v", spec, id, got)
		}
	}
}

func TestAgentLoop(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Agents", "")
	mk := func(title, agent, prio string) *IssueDetail {
		return e.issue(e.admin, IssueInput{Project: p.Key, Title: title, Agent: agent, Priority: prio})
	}
	a := mk("Refactor parser", "claude", "high")
	human := mk("Call the plumber", "none", "urgent")
	blocked := mk("Blocked work", "any", "urgent")
	blocker := mk("Haiku task", "haiku", "medium")
	if err := e.s.AddRelation(e.ctx, e.admin, blocker.Key, "blocks", blocked.Key); err != nil {
		t.Fatal(err)
	}
	tokOpus, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	tokHaiku, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "haiku", Agent: "anthropic/claude-haiku-5-5"})
	opus, _ := e.s.TokenActor(e.ctx, tokOpus)
	haiku, _ := e.s.TokenActor(e.ctx, tokHaiku)

	// Humans only and blocked work are never handed out; priority decides.
	task, err := e.s.AgentNext(e.ctx, opus, AgentQuery{})
	if err != nil || task == nil || task.Issue.Key != a.Key {
		t.Fatalf("opus next = %v, %v", task, err)
	}
	if task.Issue.Status.Category != CatInProgress || task.Issue.ClaimedBy != "anthropic/claude-opus-5-5" {
		t.Errorf("claim: %s %q", task.Issue.Status.Category, task.Issue.ClaimedBy)
	}
	// Asking again resumes the same claim.
	if again, _ := e.s.AgentNext(e.ctx, opus, AgentQuery{}); again == nil || again.Issue.Key != a.Key || !again.Resumed {
		t.Errorf("resume = %+v", again)
	}
	// Another agent gets something else.
	if hk, _ := e.s.AgentNext(e.ctx, haiku, AgentQuery{}); hk == nil || hk.Issue.Key != blocker.Key {
		t.Errorf("haiku next = %+v", hk)
	}
	// Done records usage and finishes the issue.
	res, err := e.s.AgentDone(e.ctx, opus, a.Key, AgentDoneInput{Comment: "Refactored.", Usage: &AgentUsage{InputTokens: 1200, OutputTokens: 300, CostUSD: 0.02}})
	if err != nil || !res.Finished {
		t.Fatalf("done = %+v, %v", res, err)
	}
	d := e.get(e.admin, a.Key)
	if d.Status.Category != CatDone || d.ClaimedBy != "" || len(d.AgentRuns) != 1 || d.AgentRuns[0].InputTokens != 1200 || d.AgentRuns[0].Outcome != "done" {
		t.Errorf("after done: %s %q %+v", d.Status.Category, d.ClaimedBy, d.AgentRuns)
	}
	if h := e.get(e.admin, human.Key); h.ClaimedBy != "" {
		t.Error("a human-only issue was claimed")
	}

	// A workflow: each done moves a step; review must be someone else.
	wf := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Pipeline work", Agent: "claude", Workflow: "Agent pipeline"})
	if wf.Step != "plan" {
		t.Fatalf("step = %q", wf.Step)
	}
	for _, step := range []string{"plan", "verify", "implement"} {
		got, err := e.s.AgentNext(e.ctx, opus, AgentQuery{Step: step})
		if err != nil || got == nil || got.Issue.Key != wf.Key || got.Step == nil || got.Step.Name != step || got.Step.Instructions == "" {
			t.Fatalf("step %s: %+v %v", step, got, err)
		}
		if _, err := e.s.AgentDone(e.ctx, opus, wf.Key, AgentDoneInput{}); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := e.s.AgentNext(e.ctx, opus, AgentQuery{Step: "review"}); got != nil {
		t.Errorf("implementer was given its own review: %s", got.Issue.Key)
	}
	e.s.AgentDone(e.ctx, haiku, blocker.Key, AgentDoneInput{})
	got, _ := e.s.AgentNext(e.ctx, haiku, AgentQuery{Step: "review"})
	if got == nil || got.Issue.Key != wf.Key {
		t.Fatalf("reviewer next = %+v", got)
	}

	// Agents' own issues are for themselves and marked agent-made.
	own, err := e.s.CreateIssue(e.ctx, haiku, IssueInput{Project: p.Key, Title: "Note to self"})
	if err != nil || own.Agent != "anthropic/claude-haiku-5-5" || own.ReporterAgent == "" {
		t.Fatalf("agent-made: %+v %v", own, err)
	}
	for _, k := range e.search(e.admin, "project:"+p.Key+" -is:agent-made") {
		if k == own.Key {
			t.Error("-is:agent-made kept the agent's own issue")
		}
	}
	if keys := e.search(e.admin, "agent:haiku"); !strings.Contains(strings.Join(keys, ","), own.Key) {
		t.Errorf("agent:haiku = %v", keys)
	}
}

func TestAgentLoopReviewFixes(t *testing.T) {
	e := newEnv(t)
	bob := e.user("bob", false)
	a := e.project(e.admin, "Alpha", "")
	b := e.project(e.admin, "Beta", "")
	e.s.SetMember(e.ctx, e.admin, a.Key, "bob", "viewer")
	e.s.SetMember(e.ctx, e.admin, b.Key, "bob", "editor")
	e.issue(e.admin, IssueInput{Project: a.Key, Title: "Read-only for bob", Agent: "any", Priority: "urgent"})
	mine := e.issue(e.admin, IssueInput{Project: b.Key, Title: "Bob's agent can do this", Agent: "any", Priority: "low"})
	tok, _, _ := e.s.CreateToken(e.ctx, bob, TokenSpec{Name: "a", Agent: "anthropic/claude-opus-5-5"})
	agent, _ := e.s.TokenActor(e.ctx, tok)

	// A task the token can only read is skipped, not an error.
	got, err := e.s.AgentNext(e.ctx, agent, AgentQuery{})
	if err != nil || got == nil || got.Issue.Key != mine.Key {
		t.Fatalf("next = %+v, %v", got, err)
	}
	// The token's identity can't be swapped for another model.
	if _, err := e.s.AgentNext(e.ctx, agent, AgentQuery{Agent: "openai/gpt-6"}); err == nil {
		t.Error("agent token impersonated another model")
	}
	// A human closes it and takes it off agents: the claim goes, not resumes.
	none := "none"
	e.s.UpdateIssue(e.ctx, e.admin, mine.Key, IssuePatch{Agent: &none})
	if again, _ := e.s.AgentNext(e.ctx, agent, AgentQuery{}); again != nil {
		t.Errorf("resumed a task no longer delegated: %s", again.Issue.Key)
	}
	if d := e.get(e.admin, mine.Key); d.ClaimedBy != "" {
		t.Errorf("stale claim kept: %q", d.ClaimedBy)
	}
	// done on human-only work a person told it to do is take then done (WP-91).
	if res, err := e.s.AgentDone(e.ctx, agent, mine.Key, AgentDoneInput{}); err != nil || !res.Finished {
		t.Errorf("agent couldn't finish a human-only issue it was told to do: %+v %v", res, err)
	}
	// batch leaves work this task blocks, and backlog, alone.
	lead := e.issue(e.admin, IssueInput{Project: b.Key, Title: "Lead", Agent: "any", Priority: "high"})
	after := e.issue(e.admin, IssueInput{Project: b.Key, Title: "After lead", Agent: "any"})
	e.s.AddRelation(e.ctx, e.admin, lead.Key, "blocks", after.Key)
	bt, err := e.s.AgentBatch(e.ctx, agent, AgentQuery{})
	if err != nil || bt == nil || bt.Issue.Key != lead.Key {
		t.Fatalf("batch = %+v %v", bt, err)
	}
	for _, r := range bt.Related {
		if r.Key == after.Key {
			t.Error("batch claimed an issue the task blocks")
		}
	}
	// A step that vanished from the workflow is an error, not "done".
	wf, _ := e.s.CreateWorkflow(e.ctx, e.admin, b.Key, WorkflowInput{Name: str("Two"), Steps: &[]WorkflowStep{{Name: "a"}, {Name: "b"}}})
	w := e.issue(e.admin, IssueInput{Project: b.Key, Title: "On two", Agent: "any", Workflow: "Two"})
	e.s.UpdateWorkflow(e.ctx, e.admin, wf.ID, WorkflowInput{Steps: &[]WorkflowStep{{Name: "x"}, {Name: "y"}}})
	if _, err := e.s.AgentDone(e.ctx, agent, w.Key, AgentDoneInput{}); err == nil {
		t.Error("done on a missing step finished the issue")
	}
	if d := e.get(e.admin, w.Key); d.Status.Category == CatDone {
		t.Error("issue marked done")
	}
}

func TestPageMoveOutNeedsOwner(t *testing.T) {
	e := newEnv(t)
	alice := e.user("alice", false)
	p := e.project(e.admin, "Wiki", "")
	e.s.SetMember(e.ctx, e.admin, p.Key, "alice", "editor")
	pg, _ := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Team page"), Project: Some(p.Key)})
	if _, err := e.s.UpdatePage(e.ctx, alice, pg.ID, PageInput{Project: Null[string]()}); err == nil {
		t.Error("an editor took a page out of the project")
	}
	moved, err := e.s.UpdatePage(e.ctx, e.admin, pg.ID, PageInput{Project: Null[string]()})
	if err != nil || moved.ProjectID != nil || moved.Owner == nil || moved.Owner.ID != e.admin.User.ID {
		t.Errorf("owner move: %+v %v", moved, err)
	}
}

// In a sprint project new issues start in the backlog, which agents skip;
// one handed to an agent starts in To Do so it gets picked up.
func TestDelegatedIssueSkipsBacklog(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Scrum", "sprints")
	plain := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Someday"})
	delegated := e.issue(e.admin, IssueInput{Project: p.Key, Title: "For an agent", Agent: "any"})
	parked := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Later", Agent: "any", Status: "Backlog"})
	if plain.Status.Category != CatBacklog || delegated.Status.Category != CatTodo || parked.Status.Category != CatBacklog {
		t.Fatalf("statuses: plain %s, delegated %s, parked %s", plain.Status.Category, delegated.Status.Category, parked.Status.Category)
	}
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "bot", Agent: "anthropic/claude-opus-5-5"})
	bot, _ := e.s.TokenActor(e.ctx, tok)
	own := e.issue(bot, IssueInput{Project: p.Key, Title: "My notes"})
	if own.Status.Category != CatBacklog {
		t.Errorf("an agent's own issue moved to %s", own.Status.Category)
	}
	if task, err := e.s.AgentNext(e.ctx, bot, AgentQuery{Peek: true}); err != nil || task == nil || task.Issue.Key != delegated.Key {
		t.Errorf("next = %+v, %v", task, err)
	}
}

// Fixes from the third review round.
func TestAgentLoopRound3(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Gamma", "")
	tokOpus, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	tokHaiku, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "haiku", Agent: "anthropic/claude-haiku-5-5"})
	opus, _ := e.s.TokenActor(e.ctx, tokOpus)
	haiku, _ := e.s.TokenActor(e.ctx, tokHaiku)
	openRuns := func(key string) (n int) {
		e.s.db.R.QueryRow(`SELECT count(*) FROM agent_runs r JOIN issues i ON i.id = r.issue_id
			WHERE i.project_id = ? AND i.number = ? AND r.ended_at IS NULL`, p.ID, e.get(e.admin, key).Number).Scan(&n)
		return n
	}

	// A review step's "Different" rule (any case) holds on done, not just next.
	e.s.CreateWorkflow(e.ctx, e.admin, p.Key, WorkflowInput{Name: str("Reviewed"),
		Steps: &[]WorkflowStep{{Name: "implement"}, {Name: "review", Agent: "Different"}}})
	w := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Feature", Agent: "any", Workflow: "Reviewed", Priority: "urgent"})
	if got, _ := e.s.AgentNext(e.ctx, opus, AgentQuery{}); got == nil || got.Issue.Key != w.Key {
		t.Fatalf("opus next = %+v", got)
	}
	if res, err := e.s.AgentDone(e.ctx, opus, w.Key, AgentDoneInput{}); err != nil || res.Step != "review" {
		t.Fatalf("implement done = %+v %v", res, err)
	}
	if _, err := e.s.AgentDone(e.ctx, opus, w.Key, AgentDoneInput{}); err == nil {
		t.Error("the implementer approved its own review")
	}
	if got, _ := e.s.AgentNext(e.ctx, haiku, AgentQuery{Step: "review"}); got == nil || got.Issue.Key != w.Key {
		t.Errorf("a different agent can't take the review: %+v", got)
	}

	// Closed issues can't be finished; a blocked one a person pointed the agent at can.
	closed := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Closed", Agent: "any", Status: "Done"})
	if _, err := e.s.AgentDone(e.ctx, opus, closed.Key, AgentDoneInput{}); err == nil {
		t.Error("finished a closed issue")
	}
	blocker := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Blocker", Agent: "none"})
	blocked := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Blocked", Agent: "any"})
	e.s.AddRelation(e.ctx, e.admin, blocker.Key, "blocks", blocked.Key)
	// Negative usage is refused on done too, before anything is taken.
	if _, err := e.s.AgentDone(e.ctx, opus, blocked.Key, AgentDoneInput{Usage: &AgentUsage{InputTokens: -5}}); err == nil || !IsInvalid(err) {
		t.Errorf("negative usage: %v", err)
	}
	if d := e.get(e.admin, blocked.Key); d.ClaimedBy != "" {
		t.Errorf("a refused done took the issue: %q", d.ClaimedBy)
	}
	if res, err := e.s.AgentDone(e.ctx, opus, blocked.Key, AgentDoneInput{}); err != nil || !res.Finished {
		t.Errorf("finish a blocked issue a person pointed the agent at: %+v %v", res, err)
	}
	// Usage on a human-only issue the agent never took is recorded as a run.
	if err := e.s.AgentReportUsage(e.ctx, opus, blocker.Key, "", AgentUsage{InputTokens: 10}); err != nil {
		t.Errorf("usage on a task a person named: %v", err)
	}

	// Asking for a batch twice keeps one open run per issue.
	lead := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Lead", Agent: "any", Priority: "high"})
	side := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Side", Agent: "any"})
	e.s.AddRelation(e.ctx, e.admin, lead.Key, "relates", side.Key)
	for range 2 {
		if bt, err := e.s.AgentBatch(e.ctx, opus, AgentQuery{}); err != nil || bt == nil || bt.Issue.Key != lead.Key || len(bt.Related) != 1 {
			t.Fatalf("batch = %+v %v", bt, err)
		}
	}
	if n := openRuns(side.Key); n != 1 {
		t.Errorf("side has %d open runs", n)
	}

	// Peeking doesn't drop a claim, even one that no longer fits.
	none := "none"
	e.s.UpdateIssue(e.ctx, e.admin, lead.Key, IssuePatch{Agent: &none})
	e.s.AgentNext(e.ctx, opus, AgentQuery{Peek: true})
	if d := e.get(e.admin, lead.Key); d.ClaimedBy == "" {
		t.Error("peek dropped a claim")
	}

	// An expired run ends when its (renewed) lease did, not one lease in.
	e.setClock(e.clockNow().Add(time.Hour))
	e.s.AgentReportUsage(e.ctx, opus, side.Key, "", AgentUsage{OutputTokens: 1}) // renews to +3h
	e.setClock(e.clockNow().Add(5 * time.Hour))
	if err := e.s.expireRuns(e.ctx); err != nil {
		t.Fatal(err)
	}
	var secs int64
	var inferred bool
	for _, r := range e.get(e.admin, side.Key).AgentRuns {
		if r.Outcome == "expired" {
			secs, inferred = r.Seconds, r.EndInferred
		}
	}
	if secs != 3*3600 {
		t.Errorf("expired run lasted %ds, want %d", secs, 3*3600)
	}
	// Its end is a bound the server made up, and the run says so (WP-1470).
	if !inferred {
		t.Error("an expired run's end is not marked inferred")
	}
}

// A task held from one checkout isn't handed back to the same agent asking
// from another repo's scope: resuming obeys the repo and query too.
func TestAgentResumeStaysInScope(t *testing.T) {
	e := newEnv(t)
	a := e.project(e.admin, "Alpha", "")
	b := e.project(e.admin, "Beta", "")
	ia := e.issue(e.admin, IssueInput{Project: a.Key, Title: "Alpha work", Agent: "any", Priority: "urgent"})
	ib := e.issue(e.admin, IssueInput{Project: b.Key, Title: "Beta work", Agent: "any"})
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	opus, _ := e.s.TokenActor(e.ctx, tok)
	if t1, err := e.s.AgentNext(e.ctx, opus, AgentQuery{Query: "project:" + a.Key}); err != nil || t1 == nil || t1.Issue.Key != ia.Key {
		t.Fatalf("alpha next = %v, %v", t1, err)
	}
	t2, err := e.s.AgentNext(e.ctx, opus, AgentQuery{Query: "project:" + b.Key})
	if err != nil || t2 == nil || t2.Issue.Key != ib.Key || t2.Resumed {
		t.Errorf("beta next handed back %+v, %v", t2, err)
	}
	// Asking from Alpha's scope again resumes Alpha's.
	if t3, _ := e.s.AgentNext(e.ctx, opus, AgentQuery{Query: "project:" + a.Key}); t3 == nil || t3.Issue.Key != ia.Key || !t3.Resumed {
		t.Errorf("alpha resume = %+v", t3)
	}
}
