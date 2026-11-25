package core

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func agentActor(a *Actor, id string) *Actor {
	c := *a
	c.Agent = id
	return &c
}

func TestGlossaryScopes(t *testing.T) {
	e := newEnv(t)
	org := e.project(e.admin, "Acme", "")
	repo := e.project(e.admin, "Widgets", "")
	parent := org.Key
	if _, err := e.s.UpdateProject(e.ctx, e.admin, repo.Key, ProjectInput{Parent: Some(parent)}); err != nil {
		t.Fatal(err)
	}
	other := e.project(e.admin, "Other", "")
	mk := func(term, project, def string, aliases ...string) *GlossaryEntry {
		t.Helper()
		g, err := e.s.CreateGlossary(e.ctx, e.admin, GlossaryInput{Term: &term, Definition: &def, Project: &project, Aliases: &aliases})
		if err != nil {
			t.Fatalf("create %s: %v", term, err)
		}
		return g
	}
	mk("Claim", "", "Global: an agent's lease on a task.", "lease")
	mk("Claim", org.Key, "Acme: an insurance claim.")
	mk("Widget", repo.Key, "A thing [[Gadget]] makes.")
	gadget := mk("Gadget", org.Key, "Makes widgets.")
	mk("Secret", other.Key, "Not visible from Widgets.")

	full, err := e.s.ListGlossary(e.ctx, e.admin, GlossaryQuery{Project: repo.Key})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, g := range full {
		label := g.Term + "@" + cmp(g.ProjectKey, "global")
		if g.Shadowed {
			label += "(shadowed)"
		}
		got = append(got, label)
	}
	want := "Claim@ACME Claim@global(shadowed) Gadget@ACME Widget@WIDG"
	if strings.Join(got, " ") != want {
		t.Errorf("full glossary = %v, want %s", got, want)
	}
	lk, err := e.s.LookupGlossary(e.ctx, e.admin, "claim", repo.Key)
	if err != nil || len(lk.Matches) != 2 || lk.Matches[0].ProjectKey != org.Key || !lk.Matches[1].Shadowed {
		t.Fatalf("lookup = %+v, %v", lk, err)
	}
	if lk, _ := e.s.LookupGlossary(e.ctx, e.admin, "LEASE", ""); len(lk.Matches) != 1 || lk.Matches[0].Term != "Claim" {
		t.Errorf("alias lookup = %+v", lk.Matches)
	}
	if lk, _ := e.s.LookupGlossary(e.ctx, e.admin, "widg", repo.Key); len(lk.Matches) != 0 || len(lk.Suggestions) == 0 || lk.Suggestions[0].Term != "Widget" {
		t.Errorf("suggestions = %+v", lk)
	}
	links, err := e.s.ItemLinks(e.ctx, e.admin, LinkEnd{Type: KGlossary, ID: gadget.ID})
	if err != nil || len(links) != 1 || links[0].Direction != "in" || links[0].Item.Title != "Widget" || !links[0].Auto {
		t.Errorf("gadget links = %+v, %v", links, err)
	}
	dup := "claim"
	if _, err := e.s.CreateGlossary(e.ctx, e.admin, GlossaryInput{Term: &dup, Project: &org.Key}); !IsInvalid(err) {
		t.Errorf("duplicate = %v", err)
	}
	bob := e.user("bob", false)
	if err := e.s.SetMember(e.ctx, e.admin, repo.Key, "bob", "editor"); err != nil {
		t.Fatal(err)
	}
	empty := ""
	if _, err := e.s.CreateGlossary(e.ctx, bob, GlossaryInput{Term: str("Thing"), Project: &empty}); err == nil {
		t.Error("bob wrote the global glossary")
	}
	vis, _ := e.s.ListGlossary(e.ctx, bob, GlossaryQuery{})
	for _, g := range vis {
		if g.Term == "Secret" {
			t.Error("bob sees another project's glossary")
		}
	}
	full, _ = e.s.ListGlossary(e.ctx, bob, GlossaryQuery{Project: repo.Key})
	if len(full) != 2 {
		t.Errorf("bob's full glossary = %d entries", len(full))
	}
}

func TestGlossaryLocks(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Locks", "")
	agent := agentActor(e.admin, "anthropic/claude-opus-5-5")
	term, def := "Step", "Where an issue is in its workflow."
	g, err := e.s.CreateGlossary(e.ctx, agent, GlossaryInput{Term: &term, Definition: &def, Project: &p.Key})
	if err != nil {
		t.Fatal(err)
	}
	if g.CreatedAgent != "anthropic/claude-opus-5-5" || g.Locked || !g.CanEdit {
		t.Errorf("agent-made entry = %+v", g)
	}
	def2 := "A workflow step."
	if _, err := e.s.UpdateGlossary(e.ctx, agent, g.ID, GlossaryInput{Definition: &def2}); err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := e.s.UpdateGlossary(e.ctx, agent, g.ID, GlossaryInput{Locked: &yes}); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent lock = %v", err)
	}
	if g, err = e.s.UpdateGlossary(e.ctx, e.admin, g.ID, GlossaryInput{Locked: &yes}); err != nil || !g.Locked || g.LockedBy == nil {
		t.Fatalf("lock = %+v, %v", g, err)
	}
	if _, err := e.s.UpdateGlossary(e.ctx, agent, g.ID, GlossaryInput{Definition: &def}); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent edit of locked = %v", err)
	}
	if err := e.s.DeleteGlossary(e.ctx, agent, g.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent delete of locked = %v", err)
	}
	if _, err := e.s.AddItemLink(e.ctx, agent, LinkInput{From: LinkEnd{Type: KGlossary, ID: g.ID}, To: LinkEnd{Ref: "internal/core/agents.go"}}); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent link from locked = %v", err)
	}
	mcp := *e.admin
	mcp.MCP = true
	if _, err := e.s.UpdateGlossary(e.ctx, &mcp, g.ID, GlossaryInput{Definition: &def}); !errors.Is(err, ErrForbidden) {
		t.Errorf("mcp edit of locked = %v", err)
	}
	if _, err := e.s.UpdateGlossary(e.ctx, e.admin, g.ID, GlossaryInput{Definition: &def}); err != nil {
		t.Fatal(err)
	}
	full, err := e.s.GetGlossary(e.ctx, agent, g.ID)
	if err != nil || full.CanEdit || len(full.History) != 4 || full.History[3].Agent != "anthropic/claude-opus-5-5" || full.History[0].Agent != "" {
		t.Errorf("history = %+v, can_edit %v, %v", full.History, full.CanEdit, err)
	}
	no := false
	if _, err := e.s.UpdateGlossary(e.ctx, e.admin, g.ID, GlossaryInput{Locked: &no}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.DeleteGlossary(e.ctx, agent, g.ID); err != nil {
		t.Errorf("agent delete of unlocked = %v", err)
	}
}

func TestDesignApproval(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Design", "")
	agent := agentActor(e.admin, "anthropic/claude-opus-5-5")
	note, err := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("Issue page"), Kind: str("screen"), Body: str("Shows everything about an issue."), Project: &p.Key})
	if err != nil || !note.Approved || note.ApprovedBy == nil || note.Kind != "screen" {
		t.Fatalf("human note = %+v, %v", note, err)
	}
	n, err := e.s.UpdateDesign(e.ctx, agent, note.ID, DesignInput{Body: str("Shows everything, with a big chart.")})
	if err != nil || n.Body != "Shows everything about an issue." || len(n.Proposals) != 1 || n.Notice == "" || n.Pending != 1 {
		t.Fatalf("agent edit = %+v, %v", n, err)
	}
	if _, err := e.s.UpdateDesign(e.ctx, agent, note.ID, DesignInput{Status: str("retired")}); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent retire = %v", err)
	}
	if err := e.s.DeleteDesign(e.ctx, agent, note.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent delete = %v", err)
	}
	n, err = e.s.AddDesignAddition(e.ctx, agent, note.ID, "Mobile: stack the sidebar under the description.")
	if err != nil || len(n.Additions) != 1 || n.Additions[0].State != RevPending || n.Additions[0].Agent == "" {
		t.Fatalf("addition = %+v, %v", n.Additions, err)
	}
	if _, err := e.s.ReviewDesign(e.ctx, agent, n.Additions[0].ID, "approve", ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent review = %v", err)
	}
	q, err := e.s.DesignReview(e.ctx, e.admin, p.Key)
	if err != nil || len(q) != 2 || q[0].Revision.Kind != "edit" || q[1].Revision.Kind != "addition" {
		t.Fatalf("queue = %+v, %v", q, err)
	}
	e.setClock(e.clockNow().Add(time.Hour))
	n, err = e.s.ReviewDesign(e.ctx, e.admin, q[1].Revision.ID, "merge", "")
	if err != nil || !strings.HasSuffix(n.Body, "Mobile: stack the sidebar under the description.") || n.Additions[0].State != RevMerged {
		t.Fatalf("merge = %+v, %v", n, err)
	}
	if !n.Proposals[0].Stale {
		t.Error("the edit was proposed against the old body; should be stale")
	}
	n, err = e.s.ReviewDesign(e.ctx, e.admin, q[0].Revision.ID, "reject", "No charts here.")
	if err != nil || n.Pending != 0 || len(n.Proposals) != 0 || strings.Contains(n.Body, "chart") {
		t.Fatalf("reject = %+v, %v", n, err)
	}
	var merged *DesignRevision
	for i := range n.History {
		if n.History[i].Kind == "edit" && n.History[i].State == RevApproved && n.History[i].Agent != "" {
			merged = &n.History[i]
		}
	}
	if merged == nil || merged.ReviewedBy == nil || merged.ReviewedBy.Username != "robin" {
		t.Errorf("merged revision = %+v", merged)
	}

	prop, err := e.s.CreateDesign(e.ctx, agent, DesignInput{Title: str("Dark mode"), Kind: str("feature"), Body: str("Follow the system."), Project: &p.Key})
	if err != nil || prop.Approved || prop.Notice == "" || !prop.CanEdit {
		t.Fatalf("agent note = %+v, %v", prop, err)
	}
	if prop, err = e.s.UpdateDesign(e.ctx, agent, prop.ID, DesignInput{Body: str("Follow the system; a toggle overrides it.")}); err != nil || !strings.Contains(prop.Body, "toggle") {
		t.Fatalf("agent edit of own proposal = %+v, %v", prop, err)
	}
	if _, err := e.s.ApproveDesign(e.ctx, agent, prop.ID, ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent approve = %v", err)
	}
	if prop, err = e.s.ApproveDesign(e.ctx, e.admin, prop.ID, ""); err != nil || !prop.Approved || prop.ApprovedBy == nil {
		t.Fatalf("approve = %+v, %v", prop, err)
	}
	if prop, _ = e.s.UpdateDesign(e.ctx, agent, prop.ID, DesignInput{Title: str("Themes")}); prop.Title != "Dark mode" || len(prop.Proposals) != 1 {
		t.Errorf("post-approval agent edit = %+v", prop)
	}
	if prop, err = e.s.ReviewDesign(e.ctx, e.admin, prop.Proposals[0].ID, "approve", ""); err != nil || prop.Title != "Themes" {
		t.Errorf("approve edit = %+v, %v", prop, err)
	}
}

func TestSpecAndLinks(t *testing.T) {
	e := newEnv(t)
	e.s.Forges = newMemForge()
	p := e.project(e.admin, "Spec", "")
	agent := agentActor(e.admin, "anthropic/claude-opus-5-5")
	if _, err := e.s.CreateGlossary(e.ctx, e.admin, GlossaryInput{Term: str("Board"), Definition: str("Columns of statuses."), Project: &p.Key}); err != nil {
		t.Fatal(err)
	}
	issue := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Build the board"})
	if _, err := e.s.AddLink(e.ctx, e.admin, issue.Key, "https://github.com/acme/widgets/pull/7", "Board UI"); err != nil {
		t.Fatal(err)
	}
	goal, _ := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("Fast"), Kind: str("goal"), Body: str("Every page under 100 ms."), Project: &p.Key})
	feature, err := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("Board view"), Kind: str("feature"), Project: &p.Key,
		Body:  str("The [[Board]] shows work by status. See [[Fast]]."),
		Links: []string{issue.Key, "web/src/pages/Board.tsx", "https://github.com/acme/widgets/commit/abcdef1234"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("Drag cards"), Kind: str("decision"), Body: str("Cards drag between columns."), Parent: Some(feature.ID)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AddDesignAddition(e.ctx, agent, feature.ID, "Swimlanes by assignee would help."); err != nil {
		t.Fatal(err)
	}
	sp, err := e.s.GetSpec(e.ctx, e.admin, "board view", p.Key)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Note.ID != feature.ID || len(sp.Parts) != 1 || sp.Parts[0].Title != "Drag cards" {
		t.Errorf("spec note/parts = %d %+v", sp.Note.ID, sp.Parts)
	}
	if len(sp.Terms) != 1 || sp.Terms[0].Term != "Board" || len(sp.Related) != 1 || sp.Related[0].Note.ID != goal.ID {
		t.Errorf("terms %+v related %+v", sp.Terms, sp.Related)
	}
	if len(sp.Issues) != 1 || len(sp.Files) != 1 || len(sp.Changes) != 2 {
		t.Errorf("issues %+v files %+v changes %+v", sp.Issues, sp.Files, sp.Changes)
	}
	if len(sp.Note.Additions) != 1 || sp.Note.Additions[0].State != RevPending {
		t.Errorf("additions = %+v", sp.Note.Additions)
	}
	ov, err := e.s.GetSpecOverview(e.ctx, e.admin, p.Key)
	if err != nil || len(ov.Goals) != 1 || len(ov.Index["feature"]) != 1 || ov.Pending != 1 {
		t.Errorf("overview = %+v, %v", ov, err)
	}
	links, err := e.s.ItemLinks(e.ctx, agent, LinkEnd{Type: KIssue, Key: issue.Key})
	if err != nil || len(links) != 1 || links[0].Item.Type != KDesign || links[0].Direction != "in" || links[0].CanRemove {
		t.Fatalf("issue links = %+v, %v", links, err)
	}
	if err := e.s.RemoveItemLink(e.ctx, agent, links[0].ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent removed a person's link: %v", err)
	}
	l, err := e.s.AddItemLink(e.ctx, agent, LinkInput{From: LinkEnd{Type: KDesign, ID: feature.ID}, To: LinkEnd{Ref: "glossary:board"}, Rel: "relates"})
	if err != nil || l.Item.Title != "Board" || l.CreatedAgent == "" || !l.CanRemove {
		t.Fatalf("agent link = %+v, %v", l, err)
	}
	if err := e.s.RemoveItemLink(e.ctx, agent, l.ID); err != nil {
		t.Errorf("agent remove own link = %v", err)
	}
	pg, _ := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Board notes"), Project: Some(p.Key)})
	if _, err := e.s.AddItemLink(e.ctx, e.admin, LinkInput{From: LinkEnd{Type: KIssue, Key: issue.Key}, To: LinkEnd{Type: KPage, ID: pg.ID}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.s.GetPage(e.ctx, e.admin, pg.ID); len(got.Issues) != 1 {
		t.Errorf("page issues = %+v", got.Issues)
	}
	it, err := e.s.Resolve(e.ctx, e.admin, "Board", p.Key)
	if err != nil || it.Type != KGlossary {
		t.Errorf("resolve = %+v, %v", it, err)
	}
	if ds := e.s.DesignNotesFor(e.ctx, e.admin, issue.ID); len(ds) != 1 || ds[0].ID != feature.ID {
		t.Errorf("design notes for issue = %+v", ds)
	}
}

func TestSearchAll(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Search", "")
	open := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Fix the flux capacitor"})
	done := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Flux capacitor prototype", Status: "done"})
	if _, err := e.s.AddComment(e.ctx, e.admin, open.Key, "The flux capacitor needs 1.21 gigawatts."); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AddComment(e.ctx, e.admin, done.Key, "Flux worked once."); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Flux notes"), Body: str("All about the capacitor."), Project: Some(p.Key)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateGlossary(e.ctx, e.admin, GlossaryInput{Term: str("Flux capacitor"), Definition: str("What makes time travel possible."), Project: &p.Key}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("Flux handling"), Kind: str("decision"), Body: str("Never exceed 88 mph."), Project: &p.Key, Status: str("superseded")}); err != nil {
		t.Fatal(err)
	}
	count := func(r *AllResult) map[string]int {
		m := map[string]int{}
		for _, g := range r.Groups {
			m[g.Kind] = len(g.Hits)
		}
		return m
	}
	r, err := e.s.SearchAll(e.ctx, e.admin, AllQuery{Text: "flux", Projects: []string{p.Key}})
	if err != nil {
		t.Fatal(err)
	}
	if c := count(r); c[KIssue] != 1 || c["comment"] != 1 || c[KPage] != 1 || c[KGlossary] != 1 || c[KDesign] != 0 {
		t.Errorf("default counts = %v", c)
	}
	r, err = e.s.SearchAll(e.ctx, e.admin, AllQuery{Text: "flux", Archived: true})
	if err != nil {
		t.Fatal(err)
	}
	if c := count(r); c[KIssue] != 2 || c["comment"] != 2 || c[KDesign] != 1 {
		t.Errorf("archived counts = %v", c)
	}
	r, err = e.s.SearchAll(e.ctx, e.admin, AllQuery{Text: "flux", Kinds: []string{"issues,logs"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Groups) != 2 || r.Groups[0].Kind != KIssue || r.Groups[1].Kind != "comment" {
		t.Errorf("kinds = %+v", r.Kinds)
	}
	if s := strings.ToLower(r.Groups[1].Hits[0].Snippet); !strings.Contains(s, "⟦flux⟧") || strings.Contains(s, "\n") {
		t.Errorf("snippet = %q", r.Groups[1].Hits[0].Snippet)
	}
	if _, err := e.s.SearchAll(e.ctx, e.admin, AllQuery{Text: "flux", Kinds: []string{"bogus"}}); !IsInvalid(err) {
		t.Errorf("bad kind = %v", err)
	}
	bob := e.user("bob", false)
	r, _ = e.s.SearchAll(e.ctx, bob, AllQuery{Text: "flux", Archived: true})
	for _, g := range r.Groups {
		if len(g.Hits) != 0 {
			t.Errorf("bob found %s: %+v", g.Kind, g.Hits)
		}
	}
}

func TestAgentTake(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Take", "")
	human := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Not delegated", Agent: "none"})
	tok, _, _ := e.s.CreateToken(e.ctx, e.admin, TokenSpec{Name: "opus", Agent: "anthropic/claude-opus-5-5"})
	opus, _ := e.s.TokenActor(e.ctx, tok)
	other := agentActor(e.admin, "openai/gpt-6")

	task, err := e.s.AgentTake(e.ctx, opus, human.Key, "")
	if err != nil {
		t.Fatal(err)
	}
	d := task.Issue
	if d.ClaimedBy != "anthropic/claude-opus-5-5" || d.Status.Category != CatInProgress || d.Agent != "anthropic/claude-opus-5-5" || d.ClaimedAt == nil {
		t.Fatalf("taken = by %q, %s, agent %q, at %v", d.ClaimedBy, d.Status.Category, d.Agent, d.ClaimedAt)
	}
	started := *d.ClaimedAt
	e.setClock(e.clockNow().Add(10 * time.Minute))
	if again, err := e.s.AgentNext(e.ctx, opus, AgentQuery{}); err != nil || again == nil || again.Issue.Key != human.Key || !again.Issue.ClaimedAt.Equal(started) {
		t.Fatalf("next after take = %+v, %v", again, err)
	}
	if _, err := e.s.AgentTake(e.ctx, other, human.Key, ""); !IsInvalid(err) {
		t.Errorf("second take = %v", err)
	}
	e.setClock(e.clockNow().Add(20 * time.Minute))
	if _, err := e.s.AgentRelease(e.ctx, opus, human.Key, ReleaseInput{Comment: "Need input"}); err != nil {
		t.Fatal(err)
	}
	if d := e.get(e.admin, human.Key); d.ClaimedBy != "" || d.ClaimedAt != nil {
		t.Errorf("after release: %q %v", d.ClaimedBy, d.ClaimedAt)
	}
	e.setClock(e.clockNow().Add(time.Hour))
	rep, err := e.s.GetTimeReport(e.ctx, e.admin, e.clockNow().Add(-24*time.Hour), e.clockNow(), "")
	if err != nil || rep.AgentSeconds != 30*60 || len(rep.ByAgent) != 1 || rep.TotalSeconds != 0 {
		t.Errorf("report agent %d %+v total %d %v", rep.AgentSeconds, rep.ByAgent, rep.TotalSeconds, err)
	}
	if _, err := e.s.Transition(e.ctx, e.admin, human.Key, "done", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AgentTake(e.ctx, opus, human.Key, ""); !IsInvalid(err) {
		t.Errorf("take closed = %v", err)
	}
}

func TestGlossaryAvoidWords(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Avoid", "")
	g, err := e.s.CreateGlossary(e.ctx, e.admin, GlossaryInput{Term: str("Share"), Definition: str("Access to one issue and everything beneath it."),
		Project: str(p.Key), Aliases: &[]string{"issue share"}, Avoid: &[]string{"invite to issue", "Invite to issue", ""}})
	if err != nil || len(g.Avoid) != 1 || g.Avoid[0] != "invite to issue" {
		t.Fatalf("create: %+v %v", g, err)
	}
	res, err := e.s.LookupGlossary(e.ctx, e.admin, "INVITE TO ISSUE", p.Key)
	if err != nil || len(res.Matches) != 0 || len(res.Instead) != 1 || res.Instead[0].ID != g.ID {
		t.Errorf("lookup avoided: %+v %v", res, err)
	}
	if list, _ := e.s.ListGlossary(e.ctx, e.admin, GlossaryQuery{Project: p.Key, Text: "invite"}); len(list) != 1 {
		t.Errorf("search avoided word: %d", len(list))
	}
	if _, err := e.s.UpdateGlossary(e.ctx, e.admin, g.ID, GlossaryInput{Avoid: &[]string{"issue share"}}); err == nil {
		t.Error("an alias was accepted as a word to avoid")
	}
	g, err = e.s.UpdateGlossary(e.ctx, e.admin, g.ID, GlossaryInput{Avoid: &[]string{"invite", "grant"}})
	if err != nil || len(g.Avoid) != 2 {
		t.Fatalf("update: %+v %v", g, err)
	}
	if full, _ := e.s.GetGlossary(e.ctx, e.admin, g.ID); len(full.History) < 2 || len(full.History[0].Avoid) != 2 {
		t.Errorf("history keeps words to avoid: %+v", full.History)
	}
}
