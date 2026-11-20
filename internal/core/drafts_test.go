package core

import (
	"errors"
	"strings"
	"testing"
)

// draftWorld is a project with a draft issue (with a draft sub-task, a
// comment, a relation and a linked page), a published issue and a draft
// page, all mentioning "zebra" so every search would find them.
type draftWorld struct {
	e                   *env
	p                   *Project
	draft, child, pub   *IssueDetail
	draftPage, linkPage *Page
}

func newDraftWorld(t *testing.T) *draftWorld {
	e := newEnv(t)
	w := &draftWorld{e: e, p: e.project(e.admin, "Drafts", "kanban")}
	w.pub = e.issue(e.admin, IssueInput{Project: w.p.Key, Title: "Published zebra work", Agent: "claude"})
	w.draft = e.issue(e.admin, IssueInput{Project: w.p.Key, Title: "Secret zebra idea", Description: "half a zebra thought",
		Agent: "claude", Draft: true})
	if !w.draft.Draft {
		t.Fatal("a draft should say so to its person")
	}
	w.child = e.issue(e.admin, IssueInput{Parent: w.draft.Key, Title: "Zebra sub-task", Agent: "claude"})
	if !w.child.Draft {
		t.Fatal("a sub-issue of a draft should be a draft")
	}
	if _, err := e.s.AddComment(e.ctx, e.admin, w.draft.Key, "zebra stripes comment"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.AddRelation(e.ctx, e.admin, w.pub.Key, "relates", w.draft.Key); err != nil {
		t.Fatal(err)
	}
	title, body, proj := "Zebra draft plan", "zebra body", w.p.Key
	var err error
	if w.draftPage, err = e.s.CreatePage(e.ctx, e.admin, PageInput{Title: &title, Body: &body, Project: Opt[string]{Set: true, Value: &proj}, Draft: true}); err != nil {
		t.Fatal(err)
	}
	ltitle, issues := "Zebra published notes", []string{w.draft.Key, w.pub.Key}
	if w.linkPage, err = e.s.CreatePage(e.ctx, e.admin, PageInput{Title: &ltitle, Body: &body, Project: Opt[string]{Set: true, Value: &proj}, Issues: &issues}); err != nil {
		t.Fatal(err)
	}
	return w
}

// agents are every kind of actor that must not see drafts: agent tokens,
// a person's token with an agent header, MCP, a person's token from inside
// an agent's shell (the agent "forgot" --agent, or said none), a request
// from an agent session, and automations.
func (w *draftWorld) agents() map[string]*Actor {
	u := w.e.admin.User
	return map[string]*Actor{
		"agent":      {User: u, Agent: "anthropic/claude-opus-5-5"},
		"mcp":        {User: u, MCP: true},
		"agent-env":  {User: u, AgentEnv: "anthropic/claude-code"},
		"session":    {User: u, Session: 7},
		"automation": {User: u, Automation: &Automation{ID: 1, Name: "auto", ProjectID: w.p.ID}},
	}
}

func TestDraftsHiddenFromAgents(t *testing.T) {
	w := newDraftWorld(t)
	e := w.e
	hidden := []string{w.draft.Key, w.child.Key}
	leaks := func(t *testing.T, where, text string) {
		t.Helper()
		for _, k := range append(hidden, "Secret zebra", "Zebra sub-task", "Zebra draft plan", "stripes") {
			if strings.Contains(text, k) {
				t.Errorf("%s leaks %q: %s", where, k, text)
			}
		}
	}
	for name, a := range w.agents() {
		t.Run(name, func(t *testing.T) {
			// show, comments, history: as if it didn't exist.
			for _, k := range hidden {
				if _, err := e.s.GetIssue(e.ctx, a, k); !errors.Is(err, ErrNotFound) {
					t.Errorf("show %s: %v, want not found", k, err)
				}
				if _, err := e.s.ListComments(e.ctx, a, k); !errors.Is(err, ErrNotFound) {
					t.Errorf("comments %s: %v", k, err)
				}
				if _, err := e.s.History(e.ctx, a, k, 0); !errors.Is(err, ErrNotFound) {
					t.Errorf("history %s: %v", k, err)
				}
			}
			// ls and search, with and without asking for drafts.
			for _, q := range []string{"", "zebra", "is:draft", w.draft.Key, "is:children"} {
				res, err := e.s.Search(e.ctx, a, SearchParams{Query: q, Project: w.p.Key})
				if err != nil {
					t.Fatal(err)
				}
				for _, i := range res.Issues {
					leaks(t, "ls "+q, i.Key+" "+i.Title)
					if i.Parent != nil {
						leaks(t, "ls parent", i.Parent.Key)
					}
				}
			}
			all, err := e.s.SearchAll(e.ctx, a, AllQuery{Text: "zebra"})
			if err != nil {
				t.Fatal(err)
			}
			for _, g := range all.Groups {
				for _, h := range g.Hits {
					leaks(t, "search "+g.Kind, h.Key+" "+h.Title+" "+h.Snippet)
				}
			}
			// links: relations, linked pages and the issues a page links to.
			pub, err := e.s.GetIssue(e.ctx, a, w.pub.Key)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range pub.Relations {
				leaks(t, "relations", r.Issue.Key)
			}
			for _, pg := range pub.Pages {
				leaks(t, "issue pages", pg.Title)
			}
			lp, err := e.s.GetPage(e.ctx, a, w.linkPage.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, i := range lp.Issues {
				leaks(t, "page issues", i.Key)
			}
			// pages
			if _, err := e.s.GetPage(e.ctx, a, w.draftPage.ID); !errors.Is(err, ErrNotFound) {
				t.Errorf("draft page: %v", err)
			}
			pages, err := e.s.ListPages(e.ctx, a, PageQuery{Project: w.p.Key})
			if err != nil {
				t.Fatal(err)
			}
			for _, pg := range pages {
				leaks(t, "page ls", pg.Title)
			}
			// the board, activity and the change feed
			b, err := e.s.GetBoard(e.ctx, a, w.p.Key, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range b.Columns {
				for _, i := range c.Issues {
					leaks(t, "board", i.Key)
				}
			}
			evs, err := e.s.ProjectActivity(e.ctx, a, w.p.Key, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, ev := range evs {
				leaks(t, "activity", ev.IssueKey+" "+ev.New)
			}
			ch, err := e.s.RecentChanges(e.ctx, a, ChangeQuery{})
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range ch.Changes {
				leaks(t, "changes", c.Ref+" "+c.Title)
			}
			// Nor can they act on a draft, or publish one.
			if _, err := e.s.AddComment(e.ctx, a, w.draft.Key, "hi"); !errors.Is(err, ErrNotFound) {
				t.Errorf("comment on a draft: %v", err)
			}
			if _, err := e.s.CreateIssue(e.ctx, a, IssueInput{Parent: w.draft.Key, Title: "under it"}); !errors.Is(err, ErrNotFound) {
				t.Errorf("sub-issue of a draft: %v", err)
			}
			if _, err := e.s.PublishIssue(e.ctx, a, w.draft.Key); !errors.Is(err, ErrForbidden) {
				t.Errorf("publish: %v, want forbidden", err)
			}
			if _, err := e.s.PublishPage(e.ctx, a, w.draftPage.ID); !errors.Is(err, ErrForbidden) {
				t.Errorf("publish page: %v, want forbidden", err)
			}
		})
	}
	// next and batch: the draft is delegated to claude but never handed out.
	claude := w.agents()["agent"]
	for range 3 {
		task, err := e.s.AgentBatch(e.ctx, claude, AgentQuery{Agent: "anthropic/claude-opus-5-5"})
		if err != nil {
			t.Fatal(err)
		}
		if task == nil {
			break
		}
		leaks(t, "next", task.Issue.Key)
		for _, r := range task.Related {
			leaks(t, "batch", r.Key)
		}
		for _, c := range task.Issue.Children {
			leaks(t, "next children", c.Key)
		}
		if _, err := e.s.AgentDone(e.ctx, claude, task.Issue.Key, AgentDoneInput{}); err != nil {
			t.Fatal(err)
		}
	}
	// Even a person can't hand a draft to an agent (a runner acts as its owner).
	if _, err := e.s.AgentTake(e.ctx, e.admin, w.draft.Key, "anthropic/claude-opus-5-5"); err == nil {
		t.Error("a draft was handed to an agent")
	}
}

func TestDraftsForPeople(t *testing.T) {
	w := newDraftWorld(t)
	e := w.e
	// People see drafts, marked, and can list just them.
	res, err := e.s.Search(e.ctx, e.admin, SearchParams{Query: "is:draft", Project: w.p.Key})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Issues) != 2 || !res.Issues[0].Draft {
		t.Fatalf("is:draft = %d issues", len(res.Issues))
	}
	if d := e.get(e.admin, w.draft.Key); len(d.Children) != 1 || len(d.Relations) != 1 {
		t.Errorf("a person sees the draft's children and relations: %+v", d)
	}
	pages, _ := e.s.ListPages(e.ctx, e.admin, PageQuery{Project: w.p.Key})
	if len(pages) != 2 {
		t.Errorf("a person sees draft pages: %d", len(pages))
	}

	// An agent may file a draft for its person; it gets back what it wrote,
	// and then can't see it.
	agent := w.agents()["agent-env"]
	d, err := e.s.CreateIssue(e.ctx, agent, IssueInput{Project: w.p.Key, Title: "Robin's idea", Draft: true})
	if err != nil || d.Key == "" || !d.Draft {
		t.Fatalf("agent's draft: %+v %v", d, err)
	}
	if _, err := e.s.GetIssue(e.ctx, agent, d.Key); !errors.Is(err, ErrNotFound) {
		t.Errorf("agent sees its draft: %v", err)
	}

	// Publishing makes the draft and its sub-tasks visible to agents.
	if _, err := e.s.PublishIssue(e.ctx, e.admin, w.pub.Key); err == nil {
		t.Error("publishing a published issue should fail")
	}
	pub, err := e.s.PublishIssue(e.ctx, e.admin, w.draft.Key)
	if err != nil || pub.Draft {
		t.Fatalf("publish: %v", err)
	}
	for _, k := range []string{w.draft.Key, w.child.Key} {
		if _, err := e.s.GetIssue(e.ctx, agent, k); err != nil {
			t.Errorf("after publishing, %s: %v", k, err)
		}
	}
	if _, err := e.s.PublishPage(e.ctx, e.admin, w.draftPage.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.GetPage(e.ctx, agent, w.draftPage.ID); err != nil {
		t.Errorf("published page: %v", err)
	}
}

// Live events about drafts reach people, not agents, and set nothing else
// off (no notifications).
func TestDraftEventsStayQuiet(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Quiet", "kanban")
	person, agent := e.s.Hub.Subscribe(e.admin.User.ID), e.s.Hub.Subscribe(e.admin.User.ID)
	agent.HidesDrafts = true
	defer e.s.Hub.Unsubscribe(person)
	defer e.s.Hub.Unsubscribe(agent)
	bob := e.user("bob", false)
	if err := e.s.SetMember(e.ctx, e.admin, p.Key, "bob", "editor"); err != nil {
		t.Fatal(err)
	}
	d := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Quiet thought", Draft: true, Assignees: []string{"bob"}})
	e.s.Wait()
	got := func(sub *Subscription) bool {
		for {
			select {
			case ev := <-sub.C:
				if ev.IssueKey == d.Key {
					return true
				}
			default:
				return false
			}
		}
	}
	if !got(person) {
		t.Error("people watching live should see a draft change")
	}
	if got(agent) {
		t.Error("an agent's live stream got a draft event")
	}
	if ns, _, _ := e.s.ListNotifications(e.ctx, bob, false, 0); len(ns) != 0 {
		t.Errorf("a draft notified: %+v", ns)
	}
}

// An issue can go back to being a draft, with what's under it; agents then
// lose sight of it, but not while one is working on it.
func TestDraftAgain(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Again", "kanban")
	parent := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Unfinished idea", Agent: "claude"})
	child := e.issue(e.admin, IssueInput{Parent: parent.Key, Title: "Part of it"})
	agent := &Actor{User: e.admin.User, AgentEnv: "anthropic/claude-code"}
	d, err := e.s.DraftIssue(e.ctx, agent, parent.Key) // hiding leaks nothing, so an agent may
	if err != nil || !d.Draft {
		t.Fatalf("draft: %+v %v", d, err)
	}
	for _, k := range []string{parent.Key, child.Key} {
		if _, err := e.s.GetIssue(e.ctx, agent, k); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s still visible: %v", k, err)
		}
	}
	if _, err := e.s.DraftIssue(e.ctx, e.admin, parent.Key); err == nil {
		t.Error("drafting a draft should fail")
	}
	if _, err := e.s.PublishIssue(e.ctx, e.admin, parent.Key); err != nil {
		t.Fatal(err)
	}
	claude := &Actor{User: e.admin.User, Agent: "anthropic/claude-opus-5-5"}
	if _, err := e.s.AgentTake(e.ctx, claude, parent.Key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.DraftIssue(e.ctx, e.admin, parent.Key); err == nil || !strings.Contains(err.Error(), "being worked on") {
		t.Errorf("drafting claimed work: %v", err)
	}
}

// Publishing reaches drafts under a published sub-issue too, and an agent
// can't learn a draft exists by filing under it.
func TestPublishReachesNestedDrafts(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Nest", "kanban")
	top := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Top", Draft: true})
	mid := e.issue(e.admin, IssueInput{Parent: top.Key, Title: "Mid"})
	low := e.issue(e.admin, IssueInput{Parent: mid.Key, Title: "Low"})
	agent := &Actor{User: e.admin.User, Agent: "anthropic/claude-opus-5-5"}
	if _, err := e.s.CreateIssue(e.ctx, agent, IssueInput{Parent: top.Key, Title: "probe", Draft: true}); !errors.Is(err, ErrNotFound) {
		t.Errorf("an agent filed under a draft it can't see: %v", err)
	}
	// Top a draft, mid published, low a draft again.
	if _, err := e.s.PublishIssue(e.ctx, e.admin, mid.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.DraftIssue(e.ctx, e.admin, low.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.PublishIssue(e.ctx, e.admin, top.Key); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{top.Key, mid.Key, low.Key} {
		if _, err := e.s.GetIssue(e.ctx, agent, k); err != nil {
			t.Errorf("%s after publishing: %v", k, err)
		}
	}
}
