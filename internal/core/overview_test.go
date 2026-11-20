package core

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// overviewEnv is a project with a forge repo, ana (editor) and her agent's
// sessions, and bob who is not in the project.
type overviewEnv struct {
	*prEnv
	ana, bob *Actor
	agent    *Actor
}

func newOverviewEnv(t *testing.T) *overviewEnv {
	pe := newPREnv(t, "github")
	o := &overviewEnv{prEnv: pe}
	o.ana, o.bob = pe.user("ana", false), pe.user("bob", false)
	if err := pe.s.SetMember(pe.ctx, pe.admin, pe.p.Key, "ana", "editor"); err != nil {
		t.Fatal(err)
	}
	o.agent = agentActor(o.ana, "anthropic/claude-code")
	return o
}

// session registers an agent session through its hooks.
func (o *overviewEnv) session(machine, cli, branch string, evs ...string) int64 {
	o.t.Helper()
	var last int64
	events := []HookEvent{hook(machine, cli, "SessionStart", func(h *HookEvent) { h.Repo, h.Branch, h.Model = "me/app", branch, "claude-opus-5-5" })}
	for _, ev := range evs {
		ev := ev
		events = append(events, hook(machine, cli, ev, func(h *HookEvent) {
			h.Repo, h.Branch = "me/app", branch
			switch ev {
			case "UserPromptSubmit":
				h.Prompt, h.Title = "Fix the parser", "Parser fix"
			case "Stop":
				h.LastMessage = "Parser fixed; tests pass.\nMore detail"
			}
		}))
	}
	res, err := o.s.SessionHook(o.ctx, o.agent, events)
	if err != nil {
		o.t.Fatal(err)
	}
	last = res.Session
	return last
}

func TestAgentOverviewGathersEverything(t *testing.T) {
	o := newOverviewEnv(t)
	s, ctx := o.s, o.ctx
	working := o.session("box", "cli-working", "feat/parser", "UserPromptSubmit")
	idle := o.session("lap", "cli-idle", "main", "UserPromptSubmit", "Stop")
	_ = idle
	agent := *o.agent
	agent.Session = working

	// A question waits on ana; a second one is snoozed.
	ask := func(title string) *Request {
		r, _, err := s.CreateRequest(ctx, &agent, RequestInput{Kind: RequestQuestion, Title: title, Questions: choiceQ()})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	ask("Which database?")
	settled := ask("Use tabs?")
	if _, err := s.DecideRequest(ctx, o.ana, settled.Ref, RequestDecision{Decision: "answer", Answers: []Answer{{Choices: []string{"sqlite"}}}}); err != nil {
		t.Fatal(err)
	}
	later := ask("Rename the package?")
	until := o.clockNow().Add(24 * time.Hour)
	if _, err := s.SnoozeRequest(ctx, o.ana, later.Ref, &until); err != nil {
		t.Fatal(err)
	}

	// Queue: two ready tasks for claude, one for haiku, one backlog.
	mk := func(title, agentSpec, prio, status string) *IssueDetail {
		return o.issue(o.admin, IssueInput{Project: o.p.Key, Title: title, Agent: agentSpec, Priority: prio, Status: status})
	}
	done := mk("Urgent parser bug", "claude", "urgent", "")
	mk("Tidy docs", "claude", "low", "")
	mk("Haiku chore", "haiku", "", "")
	mk("Someday", "claude", "", "backlog")

	// An agent finishes a task and files another for later.
	tok, _, _ := s.CreateToken(ctx, o.admin, TokenSpec{Name: "code", Agent: "anthropic/claude-code"})
	code, _ := s.TokenActor(ctx, tok)
	ship := mk("Ship the thing", "claude", "high", "")
	if task, err := s.AgentNext(ctx, code, AgentQuery{}); err != nil || task == nil || task.Issue.Key != done.Key {
		t.Fatalf("next = %+v, %v", task, err)
	}
	if _, err := s.AgentDone(ctx, code, done.Key, AgentDoneInput{Comment: "done"}); err != nil {
		t.Fatal(err)
	}
	filed, err := s.CreateIssue(ctx, code, IssueInput{Project: o.p.Key, Title: "Follow up on parser", Status: "backlog"})
	if err != nil {
		t.Fatal(err)
	}
	// And one it holds right now.
	held := mk("In flight", "claude", "urgent", "")
	if task, err := s.AgentTake(ctx, code, held.Key, ""); err != nil || task == nil {
		t.Fatalf("take = %+v, %v", task, err)
	}

	// Knowledge by the agent.
	pg, err := s.CreatePage(ctx, &agent, PageInput{Title: str("Parser notes"), Category: str("note"), Project: Some(o.p.Key)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateGlossary(ctx, &agent, GlossaryInput{Term: str("Token"), Definition: str("A lexeme."), Project: &o.p.Key}); err != nil {
		t.Fatal(err)
	}

	// Pull requests: one open (by the agent), one merged.
	body := "Fixes the parser.\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\nCo-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
	open := ForgePR{Number: 12, Title: "Parser fix", Author: "robin", HeadRef: "feat/parser", HeadSHA: "aaaaaaa1111", State: "open", Body: body}
	o.fg.setPR(o.f, "me/app", open, sampleDiff)
	o.send("pull_request", prPayload("opened", open, nil))
	o.drain()
	merged := ForgePR{Number: 13, Title: "Docs", Author: "robin", HeadRef: "docs", HeadSHA: "bbbbbbb2222", State: "merged", Body: body}
	o.fg.setPR(o.f, "me/app", merged, sampleDiff)
	o.send("pull_request", prPayload("closed", merged, map[string]any{"merged_at": "2026-10-05T11:00:00Z", "merged_by": map[string]string{"login": "alex"}}))
	o.drain()

	ov, err := s.AgentOverview(ctx, o.ana, OverviewQuery{Days: 7})
	if err != nil {
		t.Fatal(err)
	}

	// Waiting on you.
	if len(ov.Waiting.Requests) != 1 || ov.Waiting.Requests[0].Title != "Which database?" || ov.Waiting.Total < 1 {
		t.Errorf("waiting: %+v", ov.Waiting)
	}
	// Sessions: both live, the working one first by recency or at least present with its place.
	if ov.SessionsTotal != 2 || len(ov.Sessions) != 2 {
		t.Fatalf("sessions: %d/%d", len(ov.Sessions), ov.SessionsTotal)
	}
	var w *AgentSession
	for _, x := range ov.Sessions {
		if x.ID == working {
			w = x
		}
	}
	if w == nil || w.State != SessionWorking || w.Title != "Parser fix" || w.Machine != "box" || w.Branch != "feat/parser" || w.URL != fmt.Sprintf("/agents/sessions/%d", working) || w.OpenRequests != 1 {
		t.Errorf("working session: %+v", w)
	}
	// Up next: claude's head in the order next hands out; haiku has its own.
	byAgent := map[string]*OverviewNext{}
	for _, n := range ov.Next {
		byAgent[n.Agent] = n
	}
	if n := byAgent["claude"]; n == nil || len(n.Items) < 2 || n.Items[0].Key != ship.Key || n.Items[0].Why != "High priority" || n.Items[0].Position != 1 {
		t.Errorf("claude's head: %+v", n)
	}
	if n := byAgent["haiku"]; n == nil || len(n.Items) != 1 || n.Items[0].Title != "Haiku chore" {
		t.Errorf("haiku's head: %+v", n)
	}
	for _, n := range ov.Next {
		for _, it := range n.Items {
			if it.Title == "Someday" {
				t.Error("a backlog issue is up next")
			}
		}
	}
	if len(ov.Claims) != 1 || ov.Claims[0].Key != held.Key {
		t.Errorf("claims: %+v", ov.Claims)
	}
	// Pull requests.
	if ov.Pulls.Total != 1 || len(ov.Pulls.Open) != 1 || ov.Pulls.Open[0].Number != 12 {
		t.Errorf("pulls: %+v", ov.Pulls)
	}
	// Branches: the pull request's and the session's, with the session on its branch.
	var parser *OverviewBranch
	for i := range ov.Branches {
		if ov.Branches[i].Branch.Branch == "feat/parser" {
			parser = &ov.Branches[i]
		}
	}
	if parser == nil || parser.PR == nil || parser.PR.Number != 12 || len(parser.Sessions) != 1 || parser.Sessions[0].ID != working {
		t.Errorf("branches: %+v", ov.Branches)
	}
	// Changes: the page, the term and the issues, newest first, with the agent named.
	if len(ov.Changes.Changes) != ovChanges || ov.Changes.NextBefore != 0 && ov.Changes.NextBefore > o.clockNow().UnixMilli() {
		t.Errorf("changes: %d, next %d", len(ov.Changes.Changes), ov.Changes.NextBefore)
	}
	kinds := map[string]bool{}
	every, err := s.RecentChanges(ctx, o.ana, ChangeQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range every.Changes {
		kinds[c.Kind] = true
	}
	if !kinds[ChangePage] || !kinds[ChangeGlossary] || !kinds[ChangeIssue] {
		t.Errorf("changes: %+v", every.Changes)
	}
	// The summary.
	sum := ov.Summary
	if sum.Doing.Count < 2 || sum.Doing.Items[0].Title != "Parser fix" || !strings.HasPrefix(sum.Doing.Items[0].URL, "/agents/sessions/") {
		t.Errorf("doing: %+v", sum.Doing)
	}
	if sum.Done.Count != 2 || !hasPart(sum.Done, "issue", 1) || !hasPart(sum.Done, "pr", 1) {
		t.Errorf("done: %+v", sum.Done)
	}
	if !hasPart(sum.Deferred, "issue", 1) || !hasPart(sum.Deferred, "request", 1) || !hasPart(sum.Deferred, "pr", 1) {
		t.Errorf("deferred: %+v", sum.Deferred)
	}
	if sum.Deferred.Items[0].Kind == "" || !hasItem(sum.Deferred, filed.Key) {
		t.Errorf("deferred items: %+v", sum.Deferred.Items)
	}
	if sum.Waiting.Count != ov.Waiting.Total || !hasPart(sum.Waiting, "request", 1) {
		t.Errorf("waiting: %+v (total %d)", sum.Waiting, ov.Waiting.Total)
	}
	// A request a person answered is theirs: named for them, kept out of what the agents did.
	if sum.Resolved.Count != 1 || len(ov.Resolved) != 1 || ov.Resolved[0].By != "ana" || ov.Resolved[0].Detail != "Answered by ana" || ov.Resolved[0].Agent != "anthropic/claude-code" {
		t.Errorf("resolved: %+v %+v", sum.Resolved, ov.Resolved)
	}
	for _, it := range ov.Done {
		if it.Kind == "request" {
			t.Errorf("a request counted as the agents' work: %+v", it)
		}
	}
	if len(ov.Done) == 0 || ov.Done[0].At.Before(ov.Done[len(ov.Done)-1].At) {
		t.Errorf("done list: %+v", ov.Done)
	}
	_ = pg

	// An agent filter narrows every part.
	only, err := s.AgentOverview(ctx, o.ana, OverviewQuery{Agent: "haiku", Days: 7})
	if err != nil {
		t.Fatal(err)
	}
	if only.SessionsTotal != 0 || len(only.Waiting.Requests) != 0 || len(only.Pulls.Open) != 0 || len(only.Changes.Changes) != 0 || only.Summary.Done.Count != 0 {
		t.Errorf("haiku sees claude's work: sessions %d, waiting %d, pulls %d, changes %d, done %d",
			only.SessionsTotal, len(only.Waiting.Requests), len(only.Pulls.Open), len(only.Changes.Changes), only.Summary.Done.Count)
	}
	if len(only.Next) != 1 || only.Next[0].Agent != "haiku" {
		t.Errorf("haiku's next: %+v", only.Next)
	}
	// A project filter, and a stranger's view.
	if _, err := s.AgentOverview(ctx, o.bob, OverviewQuery{Project: o.p.Key}); !IsForbiddenOrNotFound(err) {
		t.Errorf("a non-member read the project's overview: %v", err)
	}
	bobs, err := s.AgentOverview(ctx, o.bob, OverviewQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if bobs.SessionsTotal != 0 || len(bobs.Pulls.Open) != 0 || len(bobs.Changes.Changes) != 0 || len(bobs.Branches) != 0 || bobs.Summary.Done.Count != 0 || bobs.Summary.Doing.Count != 0 {
		t.Errorf("bob sees the project: %+v", bobs.Summary)
	}
	inProject, err := s.AgentOverview(ctx, o.ana, OverviewQuery{Project: o.p.Key})
	if err != nil || inProject.Project != o.p.Key || inProject.SessionsTotal != 2 {
		t.Errorf("project filter: %v %+v", err, inProject.SessionsTotal)
	}
	if _, err := s.AgentOverview(ctx, &agent, OverviewQuery{}); err != nil {
		t.Errorf("agent view: %v", err)
	}
}

func hasPart(l SummaryList, kind string, n int) bool {
	return slices.ContainsFunc(l.Parts, func(p SummaryPart) bool { return p.Kind == kind && p.Count == n })
}

func hasItem(l SummaryList, ref string) bool {
	return slices.ContainsFunc(l.Items, func(it OverviewItem) bool { return it.Ref == ref })
}

// IsForbiddenOrNotFound: the project is hidden from this person.
func IsForbiddenOrNotFound(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "forbidden") || strings.Contains(err.Error(), "not found"))
}

func TestRecentChangesPagingKindsAndVisibility(t *testing.T) {
	o := newOverviewEnv(t)
	s, ctx := o.s, o.ctx
	secret := o.project(o.admin, "Secret", "")
	for n := 0; n < 5; n++ {
		o.setClock(o.clockNow().Add(time.Minute))
		o.issue(o.agent, IssueInput{Project: o.p.Key, Title: fmt.Sprintf("Agent issue %d", n)})
	}
	o.setClock(o.clockNow().Add(time.Minute))
	if _, err := s.CreateGlossary(ctx, o.agent, GlossaryInput{Term: str("Lexeme"), Definition: str("x"), Project: &o.p.Key}); err != nil {
		t.Fatal(err)
	}
	o.setClock(o.clockNow().Add(time.Minute))
	if _, err := s.CreateDesign(ctx, o.agent, DesignInput{Title: str("Parser screen"), Kind: str("screen"), Body: str("x"), Project: &o.p.Key}); err != nil {
		t.Fatal(err)
	}
	o.setClock(o.clockNow().Add(time.Minute))
	o.issue(o.admin, IssueInput{Project: secret.Key, Title: "Hidden from ana"})
	o.setClock(o.clockNow().Add(time.Minute))
	if _, err := s.CreatePage(ctx, o.ana, PageInput{Title: str("Ana's diary"), Category: str("note")}); err != nil {
		t.Fatal(err)
	}

	// Newest first across kinds; her own personal page, nothing of Secret.
	all, err := s.RecentChanges(ctx, o.ana, ChangeQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if all.Changes[0].Title != "Ana's diary" || all.Changes[0].Kind != ChangePage {
		t.Errorf("newest: %+v", all.Changes[0])
	}
	for _, c := range all.Changes {
		if c.Title == "Hidden from ana" {
			t.Error("a change in a project ana isn't in")
		}
	}
	// Kinds, and paging by time without repeats or gaps.
	design, _ := s.RecentChanges(ctx, o.ana, ChangeQuery{Kind: ChangeDesign})
	if len(design.Changes) != 1 || design.Changes[0].Verb != "proposed an edit to" && design.Changes[0].Verb != "wrote" || design.Changes[0].Agent != "anthropic/claude-code" || design.Changes[0].URL == "" {
		t.Errorf("design: %+v", design.Changes)
	}
	var seen []string
	var before int64
	for range 10 {
		pg, err := s.RecentChanges(ctx, o.ana, ChangeQuery{Kind: ChangeIssue, Limit: 2, Before: before})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range pg.Changes {
			seen = append(seen, c.Ref+" "+c.Verb)
		}
		if pg.NextBefore == 0 {
			break
		}
		before = pg.NextBefore
	}
	if len(seen) != 5 || len(slices.Compact(slices.Clone(seen))) != 5 {
		t.Errorf("paged issue changes: %v", seen)
	}
	// By agent: only what agents made.
	byAgent, _ := s.RecentChanges(ctx, o.ana, ChangeQuery{Agent: "claude", Limit: 100})
	for _, c := range byAgent.Changes {
		if c.Agent == "" {
			t.Errorf("a person's change in an agent's list: %+v", c)
		}
	}
	if len(byAgent.Changes) != 7 { // five issues, the term, the note
		t.Errorf("agent changes: %d", len(byAgent.Changes))
	}
	if none, _ := s.RecentChanges(ctx, o.ana, ChangeQuery{Agent: "haiku"}); len(none.Changes) != 0 {
		t.Errorf("haiku: %+v", none.Changes)
	}
	if _, err := s.RecentChanges(ctx, o.ana, ChangeQuery{Kind: "nope"}); !IsInvalid(err) {
		t.Errorf("bad kind: %v", err)
	}
	if _, err := s.RecentChanges(ctx, o.bob, ChangeQuery{Project: o.p.Key}); err == nil {
		t.Error("bob read the project's changes")
	}
}

// A busy server: the overview stays one pass of batched queries.
func TestAgentOverviewIsQuickOnABusyDatabase(t *testing.T) {
	o := newOverviewEnv(t)
	s, ctx := o.s, o.ctx
	for n := 0; n < 150; n++ {
		o.session(fmt.Sprintf("box%d", n%5), fmt.Sprintf("cli-%d", n), fmt.Sprintf("feat/b%d", n%20), "UserPromptSubmit")
	}
	for n := 0; n < 300; n++ {
		spec := ""
		if n%2 == 0 {
			spec = "claude"
		}
		o.issue(o.admin, IssueInput{Project: o.p.Key, Title: fmt.Sprintf("Issue %d", n), Agent: spec})
	}
	if _, err := s.AgentOverview(ctx, o.ana, OverviewQuery{Days: 30}); err != nil { // warm up
		t.Fatal(err)
	}
	start := time.Now()
	ov, err := s.AgentOverview(ctx, o.ana, OverviewQuery{Days: 30})
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("overview with %d sessions and 300 issues: %v", ov.SessionsTotal, took)
	if ov.SessionsTotal != 150 || len(ov.Sessions) != ovSessions {
		t.Errorf("sessions %d/%d", len(ov.Sessions), ov.SessionsTotal)
	}
	if !raceEnabled && took > 750*time.Millisecond {
		t.Errorf("overview took %v", took)
	}
}
