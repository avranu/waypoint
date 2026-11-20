package core

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// WP-D11: agents' own timers, pausing, provenance, review, page codes.

func asAgent(a *Actor, id string) *Actor { return &Actor{User: a.User, Agent: id} }

func TestAgentTimersAreTheirOwnAndStopAfterADay(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Timers", "")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Work"})
	agent := asAgent(e.admin, "anthropic/claude-opus-5-5")
	if _, err := e.s.StartTimer(e.ctx, e.admin, i.Key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.StartTimer(e.ctx, agent, i.Key, ""); err != nil {
		t.Fatal(err)
	}
	mine, _ := e.s.RunningTimer(e.ctx, e.admin)
	theirs, _ := e.s.RunningTimer(e.ctx, agent)
	if mine == nil || theirs == nil || mine.ID == theirs.ID || theirs.Agent != "anthropic/claude-opus-5-5" || mine.Agent != "" {
		t.Fatalf("an agent's timer must run beside its person's: %+v %+v", mine, theirs)
	}
	e.setClock(e.clockNow().Add(30 * time.Hour))
	if err := e.s.expireRuns(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.s.RunningTimer(e.ctx, agent); got != nil {
		t.Errorf("the agent's timer still runs after 30h: %+v", got)
	}
	if got, _ := e.s.RunningTimer(e.ctx, e.admin); got == nil {
		t.Error("a person's timer was stopped by the agent limit")
	}
	list, _ := e.s.ListTimeEntries(e.ctx, e.admin, TimeQuery{})
	for _, te := range list {
		if te.Agent != "" && te.Seconds != int64(AgentTimerLimit/time.Second) {
			t.Errorf("the agent's entry should end at the limit: %ds", te.Seconds)
		}
	}
}

func TestAgentPauseAndResume(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Pausing", "")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Long job", Agent: "claude"})
	agent := asAgent(e.admin, "anthropic/claude-opus-5-5")
	if task, err := e.s.AgentNext(e.ctx, agent, AgentQuery{}); err != nil || task == nil || task.Issue.Key != i.Key {
		t.Fatalf("next: %v %v", task, err)
	}
	if _, err := e.s.StartTimer(e.ctx, agent, i.Key, "on it"); err != nil {
		t.Fatal(err)
	}
	e.setClock(e.clockNow().Add(10 * time.Minute))
	if _, err := e.s.AgentPause(e.ctx, agent, i.Key, ""); err != nil {
		t.Fatal(err)
	}
	d := e.get(e.admin, i.Key)
	if d.ClaimedBy == "" || d.ClaimWorkingSince != nil || d.ClaimWorked != 600 {
		t.Errorf("paused: claimed %q, since %v, worked %d", d.ClaimedBy, d.ClaimWorkingSince, d.ClaimWorked)
	}
	if got, _ := e.s.RunningTimer(e.ctx, agent); got != nil {
		t.Error("the agent's timer should pause with it")
	}
	// Idle for three hours: the claim lapses and the sweep clears it.
	e.setClock(e.clockNow().Add(3 * time.Hour))
	if err := e.s.expireRuns(e.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AgentResume(e.ctx, agent, i.Key, ""); err != nil {
		t.Fatalf("resume after the claim lapsed: %v", err)
	}
	e.setClock(e.clockNow().Add(5 * time.Minute))
	d = e.get(e.admin, i.Key)
	if d.ClaimedBy != "anthropic/claude-opus-5-5" || d.ClaimWorkingSince == nil {
		t.Fatalf("resumed: claimed %q since %v", d.ClaimedBy, d.ClaimWorkingSince)
	}
	if got, _ := e.s.RunningTimer(e.ctx, agent); got == nil || got.Note != "on it" {
		t.Errorf("the agent's timer should resume with it: %+v", got)
	}
	var worked int64
	for _, r := range d.AgentRuns {
		worked += r.Seconds
	}
	if worked != 15*60 {
		t.Errorf("worked %ds over runs %+v, want 15 minutes (idle time isn't work)", worked, d.AgentRuns)
	}
	if _, err := e.s.AgentDone(e.ctx, agent, i.Key, AgentDoneInput{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.s.RunningTimer(e.ctx, agent); got != nil {
		t.Error("done should stop the agent's timer on the task")
	}
	if _, err := e.s.AgentResume(e.ctx, agent, i.Key, ""); err == nil {
		t.Error("resuming a finished task should fail")
	}
	// Someone else's claim isn't paused or taken over.
	other := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Theirs", Agent: "any"})
	haiku := asAgent(e.admin, "anthropic/claude-haiku-5-5")
	if _, err := e.s.AgentTake(e.ctx, haiku, other.Key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AgentPause(e.ctx, agent, other.Key, ""); err == nil {
		t.Error("paused another agent's claim")
	}
	if _, err := e.s.AgentResume(e.ctx, agent, other.Key, ""); err == nil {
		t.Error("resume took over another agent's claim")
	}
}

func TestAgentRunStopsAfterADay(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Runs", "")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Forever", Agent: "any"})
	agent := asAgent(e.admin, "anthropic/claude-opus-5-5")
	if _, err := e.s.AgentNext(e.ctx, agent, AgentQuery{}); err != nil {
		t.Fatal(err)
	}
	// The agent keeps renewing its claim, but never stops the clock.
	for range 26 {
		e.setClock(e.clockNow().Add(time.Hour))
		if err := e.s.AgentReportUsage(e.ctx, agent, i.Key, "", AgentUsage{InputTokens: 1}); err != nil {
			t.Fatal(err)
		}
		if err := e.s.expireRuns(e.ctx); err != nil {
			t.Fatal(err)
		}
	}
	d := e.get(e.admin, i.Key)
	if len(d.AgentRuns) != 1 || d.AgentRuns[0].Outcome != "timeout" || d.AgentRuns[0].Seconds != int64(AgentTimerLimit/time.Second) {
		t.Errorf("runs: %+v", d.AgentRuns)
	}
}

func TestProvenanceOfIssuesAndComments(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Prov", "")
	agent := asAgent(e.admin, "anthropic/claude-opus-5-5")
	i := e.issue(agent, IssueInput{Project: p.Key, Title: "Agent idea", Description: "Written by a model"})
	if !i.Unreviewed || i.TextAgent != "anthropic/claude-opus-5-5" || i.ReporterAgent == "" {
		t.Fatalf("agent-made issue: %+v", i.Issue)
	}
	// A person's issue is theirs; an agent's edit makes it agent text.
	h := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Mine"})
	if h.Unreviewed || h.TextAgent != "" {
		t.Errorf("person's issue marked: %+v", h.Issue)
	}
	if _, err := e.s.UpdateIssue(e.ctx, agent, h.Key, IssuePatch{Description: str("The agent's rewrite")}); err != nil {
		t.Fatal(err)
	}
	if d := e.get(e.admin, h.Key); !d.Unreviewed || d.TextAgent == "" || d.TextBy == nil {
		t.Errorf("after an agent's edit: %+v", d.Issue)
	}
	// Changing other fields isn't writing the text.
	if _, err := e.s.UpdateIssue(e.ctx, agent, i.Key, IssuePatch{Priority: "high"}); err != nil {
		t.Fatal(err)
	}
	// Closing records who closed it; reopening clears it.
	done := CatDone
	if _, err := e.s.UpdateIssue(e.ctx, agent, i.Key, IssuePatch{Status: &done}); err != nil {
		t.Fatal(err)
	}
	if d := e.get(e.admin, i.Key); d.ResolvedAgent != "anthropic/claude-opus-5-5" || d.ResolvedBy == nil || d.ResolvedBy.ID != e.admin.User.ID {
		t.Errorf("closed by: %q %+v", d.ResolvedAgent, d.ResolvedBy)
	}
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, i.Key, IssuePatch{Status: str("todo")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, i.Key, IssuePatch{Status: &done}); err != nil {
		t.Fatal(err)
	}
	if d := e.get(e.admin, i.Key); d.ResolvedAgent != "" || d.ResolvedBy == nil {
		t.Errorf("closed by a person: %q %+v", d.ResolvedAgent, d.ResolvedBy)
	}
	// Comments say which agent wrote them; agents can't touch people's.
	ca, err := e.s.AddComment(e.ctx, agent, i.Key, "Agent note")
	if err != nil || ca.Agent == "" || !ca.Unreviewed {
		t.Fatalf("agent comment: %+v %v", ca, err)
	}
	cp, _ := e.s.AddComment(e.ctx, e.admin, i.Key, "Person note")
	if err := e.s.UpdateComment(e.ctx, agent, cp.ID, "overwritten"); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent edited a person's comment: %v", err)
	}
	if err := e.s.DeleteComment(e.ctx, agent, cp.ID); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent deleted a person's comment: %v", err)
	}
	// MCP clients without a name still count as agents.
	mcp := &Actor{User: e.admin.User, MCP: true}
	if c, _ := e.s.AddComment(e.ctx, mcp, i.Key, "From MCP"); c == nil || c.Agent != "mcp" {
		t.Errorf("MCP comment: %+v", c)
	}

	// The review queue, and reviewing (people only).
	items, err := e.s.ListUnreviewed(e.ctx, e.admin, p.Key, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, it := range items {
		kinds[it.Type]++
	}
	if kinds[ReviewIssue] != 2 || kinds[ReviewComment] != 2 {
		t.Errorf("queue: %+v", items)
	}
	if _, err := e.s.Review(e.ctx, agent, ReviewIssue, i.Key, ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("an agent reviewed: %v", err)
	}
	if n, err := e.s.Review(e.ctx, e.admin, ReviewIssue, i.Key, ""); err != nil || n != 3 {
		t.Errorf("review issue = %d, %v (its text and two agent comments)", n, err)
	}
	d := e.get(e.admin, i.Key)
	if d.Unreviewed || d.ReviewedBy == nil {
		t.Errorf("after review: %+v", d.Issue)
	}
	comments, _ := e.s.ListComments(e.ctx, e.admin, i.Key)
	for _, c := range comments {
		if c.Unreviewed {
			t.Errorf("comment still unreviewed: %+v", c)
		}
	}
	// A later agent edit needs review again; a person's edit makes it theirs.
	if _, err := e.s.UpdateIssue(e.ctx, agent, i.Key, IssuePatch{Title: str("Agent idea, revised")}); err != nil {
		t.Fatal(err)
	}
	if d := e.get(e.admin, i.Key); !d.Unreviewed {
		t.Error("an agent's edit after review should need review again")
	}
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, i.Key, IssuePatch{Title: str("My idea now")}); err != nil {
		t.Fatal(err)
	}
	if d := e.get(e.admin, i.Key); d.Unreviewed || d.TextAgent != "" || d.ReporterAgent == "" {
		t.Errorf("after a person's rewrite: %+v", d.Issue)
	}
	// History names the agent.
	ev, _ := e.s.History(e.ctx, e.admin, h.Key, 100)
	found := false
	for _, x := range ev {
		if x.Field == "description" && x.ViaAutomation == "anthropic/claude-opus-5-5" {
			found = true
		}
	}
	if !found {
		t.Errorf("history doesn't name the agent: %+v", ev)
	}
}

func TestPageCodes(t *testing.T) {
	e := newEnv(t)
	ul := e.project(e.admin, "Harbor", "")
	rd := e.project(e.admin, "Quarry", "")
	mk := func(a *Actor, project, title, cat string) *Page {
		t.Helper()
		pg, err := e.s.CreatePage(e.ctx, a, PageInput{Title: &title, Category: &cat, Project: Some(project)})
		if err != nil {
			t.Fatalf("page %q: %v", title, err)
		}
		return pg
	}
	a := mk(e.admin, ul.Key, "Roadmap", "plan")
	b := mk(e.admin, ul.Key, "Second plan", "plan")
	c := mk(e.admin, ul.Key, "An idea", "idea")
	if a.Code != "PL1" || b.Code != "PL2" || c.Code != "I1" {
		t.Errorf("codes: %s %s %s", a.Code, b.Code, c.Code)
	}
	// Numbered per project; an imported title brings its code.
	r := mk(e.admin, rd.Key, "PL13: A provider could refuse", "plan")
	if r.Code != "PL13" || r.Title != "A provider could refuse" {
		t.Errorf("imported: %q %q", r.Code, r.Title)
	}
	if n := mk(e.admin, rd.Key, "Next plan", "plan"); n.Code != "PL14" {
		t.Errorf("after PL13 comes %s", n.Code)
	}
	// A code from a retired prefix stays as an alias.
	tk := mk(e.admin, rd.Key, "T7: basemap syncs suspended", "reference")
	if tk.Code != "R1" || !strings.HasPrefix(tk.Title, "T7:") {
		t.Errorf("T7 page: %q %q", tk.Code, tk.Title)
	}
	if id, err := e.s.ResolvePage(e.ctx, e.admin, "t7", rd.Key); err != nil || id != tk.ID {
		t.Errorf("T7 -> %d, %v", id, err)
	}
	// Resolving: in a project, qualified, and ambiguous without one.
	for ref, want := range map[string]int64{"PL1": a.ID, strings.ToLower(ul.Key) + "-pl2": b.ID, rd.Key + "-PL13": r.ID, "I1": c.ID} {
		if id, err := e.s.ResolvePage(e.ctx, e.admin, ref, ul.Key); err != nil || id != want {
			t.Errorf("%s -> %d, %v; want %d", ref, id, err, want)
		}
	}
	if id, err := e.s.ResolvePage(e.ctx, e.admin, "PL13", ""); err != nil || id != r.ID {
		t.Errorf("PL13 is only in Quarry: %d %v", id, err)
	}
	// A new category gives a new code; the old one still leads there.
	if pg, err := e.s.UpdatePage(e.ctx, e.admin, b.ID, PageInput{Category: str("decision")}); err != nil || pg.Code != "D1" || !slices.Contains(pg.OtherCodes, "PL2") {
		t.Errorf("recategorised: %+v %v", pg, err)
	}
	if id, _ := e.s.ResolvePage(e.ctx, e.admin, "PL2", ul.Key); id != b.ID {
		t.Error("the old code no longer resolves")
	}
	if n := mk(e.admin, ul.Key, "Third plan", "plan"); n.Code != "PL3" {
		t.Errorf("codes are never reused: got %s", n.Code)
	}
	// Explicit codes: free ones only, and they set the category.
	if pg, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Imported X"), Code: str("X31"), Project: Some(ul.Key)}); err != nil || pg.Code != "X31" || pg.Category != "experiment" {
		t.Errorf("explicit code: %+v %v", pg, err)
	}
	if _, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Clash"), Code: str("PL1"), Project: Some(ul.Key)}); err == nil {
		t.Error("took a code another page has")
	}
	// Moving to another project gives a code there.
	if pg, err := e.s.UpdatePage(e.ctx, e.admin, a.ID, PageInput{Project: Some(rd.Key)}); err != nil || pg.Code != "PL15" || !slices.Contains(pg.OtherCodes, ul.Key+"-PL1") {
		t.Errorf("moved: %+v %v", pg, err)
	}
	// Deleted pages keep their codes from being reused.
	if err := e.s.DeletePage(e.ctx, e.admin, c.ID); err != nil {
		t.Fatal(err)
	}
	if n := mk(e.admin, ul.Key, "Another idea", "idea"); n.Code != "I2" {
		t.Errorf("a deleted page's code was reused: %s", n.Code)
	}
	if _, err := e.s.ResolvePage(e.ctx, e.admin, "I1", ul.Key); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted page's code: %v", err)
	}
	// Reserving codes used elsewhere before.
	next, err := e.s.ReservePageCodes(e.ctx, e.admin, ul.Key, PageCodeNext{"PL": 12, "N": 50})
	if err != nil || next["PL"] != 12 || next["N"] != 50 || next["I"] != 3 {
		t.Errorf("reserve: %v %v", next, err)
	}
	if n := mk(e.admin, ul.Key, "After the reservation", "plan"); n.Code != "PL12" {
		t.Errorf("after reserving: %s", n.Code)
	}
	if _, err := e.s.ReservePageCodes(e.ctx, asAgent(e.admin, "x"), ul.Key, PageCodeNext{"PL": 99}); err == nil {
		t.Error("an agent reserved codes")
	}
	// Search finds a page by its code; wikilinks reach pages by code.
	if id, err := e.s.ResolvePage(e.ctx, e.admin, "PL1", ""); err != nil || id != a.ID {
		t.Errorf("PL1 (the moved page's old code) -> %d, %v", id, err)
	}
	mk(e.admin, ul.Key, "APP's own PL13", "plan")
	if _, err := e.s.ResolvePage(e.ctx, e.admin, "PL13", ""); err == nil || !strings.Contains(err.Error(), "several projects") {
		t.Errorf("PL13 is in both projects now: %v", err)
	}
	if id, _ := e.s.ResolvePage(e.ctx, e.admin, "PL13", rd.Key); id != r.ID {
		t.Error("a project settles which PL13")
	}
	if pages, _ := e.s.ListPages(e.ctx, e.admin, PageQuery{Text: rd.Key + "-PL13"}); len(pages) != 1 || pages[0].ID != r.ID {
		t.Errorf("search by code: %+v", pages)
	}
	note := mk(e.admin, rd.Key, "Notes", "note")
	if _, err := e.s.UpdatePage(e.ctx, e.admin, note.ID, PageInput{Body: str("See [[PL13]] and [[" + ul.Key + "-PL12]].")}); err != nil {
		t.Fatal(err)
	}
	links, err := e.s.ItemLinks(e.ctx, e.admin, LinkEnd{Type: KPage, ID: note.ID})
	if err != nil || len(links) != 2 {
		t.Errorf("wikilinks by code: %+v %v", links, err)
	}
	for _, l := range links {
		if l.Item.Key == "" {
			t.Errorf("linked page without its code: %+v", l.Item)
		}
	}
}

func TestPageProvenanceAndBackfill(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Docs", "")
	agent := asAgent(e.admin, "anthropic/claude-opus-5-5")
	pg, err := e.s.CreatePage(e.ctx, agent, PageInput{Title: str("Agent plan"), Category: str("plan"), Project: Some(p.Key)})
	if err != nil || !pg.Unreviewed || pg.CreatedAgent == "" || pg.TextAgent == "" {
		t.Fatalf("agent page: %+v %v", pg, err)
	}
	if n, err := e.s.Review(e.ctx, e.admin, ReviewPage, pg.Code, p.Key); err != nil || n != 1 {
		t.Errorf("review page: %d %v", n, err)
	}
	if got, _ := e.s.GetPage(e.ctx, e.admin, pg.ID); got.Unreviewed || got.ReviewedBy == nil {
		t.Errorf("after review: %+v", got)
	}
	// Moving or recategorising isn't writing.
	if got, _ := e.s.UpdatePage(e.ctx, agent, pg.ID, PageInput{Category: str("idea")}); got.Unreviewed {
		t.Error("a new category made the text unreviewed")
	}
	if got, _ := e.s.UpdatePage(e.ctx, agent, pg.ID, PageInput{Body: str("new text")}); !got.Unreviewed {
		t.Error("an agent's new text should wait for review")
	}

	// Pages from before codes get them at start, adopting their titles' codes.
	p = e.project(e.admin, "Imported", "")
	for _, title := range []string{"Untitled plan", "PL2: Second", "PL1: First"} {
		if _, err := e.s.db.W.ExecContext(e.ctx, `INSERT INTO pages(project_id, owner_id, title, category, created_at, updated_at) VALUES (?,?,?,?,0,0)`,
			p.ID, e.admin.User.ID, title, "plan"); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.s.AssignPageCodes(e.ctx); err != nil {
		t.Fatal(err)
	}
	pages, _ := e.s.ListPages(e.ctx, e.admin, PageQuery{Project: p.Key, Category: "plan"})
	got := map[string]string{}
	for _, x := range pages {
		got[x.Title] = x.Code
	}
	if got["First"] != "PL1" || got["Second"] != "PL2" || got["Untitled plan"] != "PL3" {
		t.Errorf("backfill: %v", got)
	}
	if err := e.s.AssignPageCodes(e.ctx); err != nil {
		t.Errorf("a second run: %v", err)
	}
}

func TestDropPageCode(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Drops", "")
	pg, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Old decision"), Category: str("decision"), Project: Some(p.Key)})
	if err != nil || pg.Code != "D1" {
		t.Fatalf("%+v %v", pg, err)
	}
	if err := e.s.DropPageCode(e.ctx, e.admin, pg.ID, "D1"); err == nil {
		t.Error("dropped the current code")
	}
	if _, err := e.s.ReservePageCodes(e.ctx, e.admin, p.Key, PageCodeNext{"D": 27}); err != nil {
		t.Fatal(err)
	}
	if pg, err = e.s.UpdatePage(e.ctx, e.admin, pg.ID, PageInput{Code: str("D27")}); err != nil || pg.Code != "D27" {
		t.Fatalf("recode: %+v %v", pg, err)
	}
	if err := e.s.DropPageCode(e.ctx, asAgent(e.admin, "x"), pg.ID, "D1"); err == nil {
		t.Error("an agent dropped a code")
	}
	if err := e.s.DropPageCode(e.ctx, e.admin, pg.ID, "D1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ResolvePage(e.ctx, e.admin, "D1", p.Key); !errors.Is(err, ErrNotFound) {
		t.Errorf("D1 still resolves: %v", err)
	}
	if got, _ := e.s.GetPage(e.ctx, e.admin, pg.ID); len(got.OtherCodes) != 0 {
		t.Errorf("other codes: %v", got.OtherCodes)
	}
	if _, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Reuse?"), Code: str("D1"), Project: Some(p.Key)}); err == nil {
		t.Error("a dropped code was reused")
	}
}

func TestReviewFindings(t *testing.T) {
	e := newEnv(t)
	bob := e.user("bob", false)
	hidden := e.project(e.admin, "Hidden", "")
	mine := e.project(bob, "Bobs", "")
	agent := asAgent(e.admin, "anthropic/claude-opus-5-5")
	bobAgent := asAgent(bob, "anthropic/claude-opus-5-5")
	e.issue(bobAgent, IssueInput{Project: mine.Key, Title: "Bob's agent work"})
	for range 3 {
		// Agent-written issues start a safety screen in the background, which
		// reads the clock: let it finish before moving the clock on.
		e.s.Wait()
		e.setClock(e.clockNow().Add(time.Minute))
		e.issue(agent, IssueInput{Project: hidden.Key, Title: "Newer, hidden"})
	}
	// Hidden items don't crowd a person's queue out.
	if items, err := e.s.ListUnreviewed(e.ctx, bob, "", 2); err != nil || len(items) != 1 {
		t.Errorf("bob's queue: %+v %v", items, err)
	}
	// An agent learns nothing about comments it can't see.
	h := e.issue(e.admin, IssueInput{Project: hidden.Key, Title: "Secret"})
	c, _ := e.s.AddComment(e.ctx, e.admin, h.Key, "person's words")
	if err := e.s.UpdateComment(e.ctx, bobAgent, c.ID, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("hidden comment: %v", err)
	}
	// A page moved to a project numbered alike resolves to itself.
	a := e.project(e.admin, "Alpha", "")
	b := e.project(e.admin, "Bravo", "")
	pg, _ := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Moving plan"), Category: str("plan"), Project: Some(a.Key)})
	if pg, _ = e.s.UpdatePage(e.ctx, e.admin, pg.ID, PageInput{Project: Some(b.Key)}); pg.Code != "PL1" {
		t.Fatalf("moved: %s", pg.Code)
	}
	if id, err := e.s.ResolvePage(e.ctx, e.admin, "PL1", ""); err != nil || id != pg.ID {
		t.Errorf("PL1 -> %d, %v", id, err)
	}
	// Code-shaped words still search the text.
	if _, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Racing notes"), Body: str("The F1 calendar"), Project: Some(a.Key)}); err != nil {
		t.Fatal(err)
	}
	if pages, _ := e.s.ListPages(e.ctx, e.admin, PageQuery{Text: "F1"}); len(pages) != 1 {
		t.Errorf("F1 text search: %d pages", len(pages))
	}
}

func TestLooseReferences(t *testing.T) {
	e := newEnv(t)
	orgName, org := "Harbor", "APPORG"
	o, err := e.s.CreateProject(e.ctx, e.admin, ProjectInput{Name: &orgName, Key: &org})
	if err != nil {
		t.Fatal(err)
	}
	appName, app := "Harbor", "APP"
	p, err := e.s.CreateProject(e.ctx, e.admin, ProjectInput{Name: &appName, Key: &app})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.UpdateProject(e.ctx, e.admin, "APP", ProjectInput{Parent: Some("APPORG")}); err != nil {
		t.Fatal(err)
	}
	// Project references: key, path of keys and names, any case.
	for _, ref := range []string{"APP", "app", "Harbor/Harbor", "APPORG/Harbor", "apporg/app", "Harbor/APP", "harbor / app"} {
		got, err := e.s.projectByRef(e.ctx, e.s.db.R, ref)
		if err != nil || got.ID != p.ID {
			t.Errorf("project %q: %v %v", ref, got, err)
		}
		found, err := e.s.FindProjects(e.ctx, e.admin, ref, false)
		if err != nil || len(found) == 0 || found[0].ID != p.ID {
			t.Errorf("find %q: %v %v", ref, found, err)
		}
	}
	// "Harbor" alone is two projects' name.
	if _, err := e.s.projectByRef(e.ctx, e.s.db.R, "Harbor"); err == nil || !strings.Contains(err.Error(), "could be") {
		t.Errorf("ambiguous name: %v", err)
	}
	if got, err := e.s.projectByRef(e.ctx, e.s.db.R, "apporg"); err != nil || got.ID != o.ID {
		t.Errorf("org by key: %v %v", got, err)
	}
	// Issue keys with or without the dash, any case.
	i := e.issue(e.admin, IssueInput{Project: "APP", Title: "Twelve"})
	for _, ref := range []string{i.Key, strings.ToLower(i.Key), "APP1", "app1"} {
		if got, err := e.s.GetIssue(e.ctx, e.admin, ref); err != nil || got.ID != i.ID {
			t.Errorf("issue %q: %v", ref, err)
		}
	}
	if keys := e.search(e.admin, "app1"); len(keys) != 1 || keys[0] != i.Key {
		t.Errorf("search app1: %v", keys)
	}
	// A key that ends in a digit, run into the number, is ambiguous.
	ab, ab2 := "AB", "AB2"
	for _, k := range []*string{&ab, &ab2} {
		if _, err := e.s.CreateProject(e.ctx, e.admin, ProjectInput{Name: k, Key: k}); err != nil {
			t.Fatal(err)
		}
	}
	for n := 0; n < 21; n++ {
		e.issue(e.admin, IssueInput{Project: "AB", Title: "a"})
	}
	e.issue(e.admin, IssueInput{Project: "AB2", Title: "b"})
	if _, err := e.s.GetIssue(e.ctx, e.admin, "AB21"); err == nil || !strings.Contains(err.Error(), "add the dash") {
		t.Errorf("AB21 should be ambiguous: %v", err)
	}
	// Page codes without the dash.
	pg, err := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Plan"), Category: str("plan"), Project: Some("APP")})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"APP-PL1", "apppl1", "APPPL1", "pl1"} {
		if id, err := e.s.ResolvePage(e.ctx, e.admin, ref, ""); err != nil || id != pg.ID {
			t.Errorf("page %q: %d %v", ref, id, err)
		}
	}
	if pages, _ := e.s.ListPages(e.ctx, e.admin, PageQuery{Text: "apppl1"}); len(pages) != 1 {
		t.Errorf("search apppl1: %d", len(pages))
	}
}

// An agent's close of an issue waits for a person's review, without
// keeping the issue open (WP-272).
func TestAgentCloseWaitsForReview(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Closes", "")
	agent := asAgent(e.admin, "anthropic/claude-opus-5-5")
	done, wont := CatDone, CatCanceled
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Person's task"})
	j := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Another"})
	mine := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Closed by a person"})
	for _, k := range []string{i.Key, j.Key} {
		if _, err := e.s.UpdateIssue(e.ctx, agent, k, IssuePatch{Status: &done}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, mine.Key, IssuePatch{Status: &wont}); err != nil {
		t.Fatal(err)
	}
	d := e.get(e.admin, i.Key)
	if !d.CloseUnreviewed || d.Status.Category != CatDone || d.Unreviewed {
		t.Fatalf("agent's close: %+v", d.Issue)
	}
	if d := e.get(e.admin, mine.Key); d.CloseUnreviewed {
		t.Errorf("a person's close needs no review: %+v", d.Issue)
	}
	if got := e.search(e.admin, "is:close-unreviewed"); strings.Join(got, " ") != j.Key+" "+i.Key && strings.Join(got, " ") != i.Key+" "+j.Key {
		t.Errorf("is:close-unreviewed = %v", got)
	}
	if got := e.search(e.admin, "is:agent-closed"); len(got) != 2 {
		t.Errorf("is:agent-closed = %v", got)
	}
	items, err := e.s.ListUnreviewed(e.ctx, e.admin, p.Key, 0)
	if err != nil || len(items) != 2 || items[0].Type != ReviewClose || items[0].Agent != "anthropic/claude-opus-5-5" || items[0].Excerpt != "Done" {
		t.Fatalf("queue: %+v %v", items, err)
	}
	if w, _ := e.s.WaitingOnYou(e.ctx, e.admin); w.Closes != 2 || w.Review != 0 || w.Total != 0 {
		t.Errorf("waiting: closes %d review %d total %d", w.Closes, w.Review, w.Total)
	}

	// Only people review; reviewing the issue covers its close.
	if _, err := e.s.Review(e.ctx, agent, ReviewClose, i.Key, ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("an agent reviewed a close: %v", err)
	}
	if n, err := e.s.Review(e.ctx, e.admin, ReviewIssue, i.Key, ""); err != nil || n != 1 {
		t.Errorf("review issue = %d, %v", n, err)
	}
	d = e.get(e.admin, i.Key)
	if d.CloseUnreviewed || d.CloseReviewedBy == nil || d.CloseReviewedBy.ID != e.admin.User.ID || d.CloseReviewedAt == nil {
		t.Errorf("after review: %+v by %+v", d.Issue, d.CloseReviewedBy)
	}
	if _, err := e.s.ReviewCloses(e.ctx, agent, []string{j.Key}); !errors.Is(err, ErrForbidden) {
		t.Errorf("an agent bulk-reviewed: %v", err)
	}
	if n, err := e.s.ReviewCloses(e.ctx, e.admin, []string{j.Key, i.Key, mine.Key}); err != nil || n != 1 {
		t.Errorf("review closes = %d, %v", n, err)
	}
	if got := e.search(e.admin, "is:close-unreviewed"); len(got) != 0 {
		t.Errorf("still waiting: %v", got)
	}

	// Reopened, the review goes with the close; a person's close is theirs.
	if _, err := e.s.UpdateIssue(e.ctx, agent, i.Key, IssuePatch{Status: str("todo")}); err != nil {
		t.Fatal(err)
	}
	if d := e.get(e.admin, i.Key); d.CloseUnreviewed || d.CloseReviewedBy != nil {
		t.Errorf("reopened: %+v", d.Issue)
	}
	if _, err := e.s.UpdateIssue(e.ctx, agent, i.Key, IssuePatch{Status: &done}); err != nil {
		t.Fatal(err)
	}
	if d := e.get(e.admin, i.Key); !d.CloseUnreviewed || d.CloseReviewedBy != nil {
		t.Errorf("an agent's second close: %+v", d.Issue)
	}
	if n, err := e.s.Review(e.ctx, e.admin, ReviewClose, i.Key, ""); err != nil || n != 1 {
		t.Errorf("review close = %d, %v", n, err)
	}

	// An agent filing an issue already closed is a close too.
	k := e.issue(agent, IssueInput{Project: p.Key, Title: "Filed done", Status: "Done"})
	if !k.CloseUnreviewed {
		t.Errorf("filed closed by an agent: %+v", k.Issue)
	}
}

// An automation closing an agent's work (the merge rule) is the agent's
// close and waits for review; closing a person's work needs none (WP-500).
func TestAutomationCloseOfAgentWorkWaitsForReview(t *testing.T) {
	e := newEnv(t)
	e.s.Forges = newMemForge() // its GitHub link is refreshed, not from the real API (WP-1065)
	p := e.project(e.admin, "Merges", "")
	agent := asAgent(e.admin, "anthropic/claude-opus-5-5")
	worked := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Agent's work"})
	if _, err := e.s.AgentTake(e.ctx, agent, worked.Key, ""); err != nil {
		t.Fatal(err)
	}
	linked := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Agent's pull request"})
	if _, err := e.s.AddLink(e.ctx, agent, linked.Key, "https://github.com/o/r/pull/7", ""); err != nil {
		t.Fatal(err)
	}
	own := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Person's work"})
	rule := &Actor{User: e.admin.User, Automation: &Automation{Name: "Move to Done when a PR merges"}}
	done := CatDone
	for _, k := range []string{worked.Key, linked.Key, own.Key} {
		if _, err := e.s.UpdateIssue(e.ctx, rule, k, IssuePatch{Status: &done}); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []string{worked.Key, linked.Key} {
		if d := e.get(e.admin, k); !d.CloseUnreviewed || d.ResolvedAgent != "anthropic/claude-opus-5-5" {
			t.Errorf("%s closed by the merge rule: unreviewed %v agent %q", k, d.CloseUnreviewed, d.ResolvedAgent)
		}
	}
	if d := e.get(e.admin, own.Key); d.CloseUnreviewed {
		t.Errorf("a person's work closed by a rule: %+v", d.Issue)
	}
	// A person closing an agent's work decides it themselves.
	again := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Closed by hand"})
	if _, err := e.s.AgentTake(e.ctx, agent, again.Key, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, again.Key, IssuePatch{Status: &done}); err != nil {
		t.Fatal(err)
	}
	if d := e.get(e.admin, again.Key); d.CloseUnreviewed {
		t.Errorf("a person's close of agent work: %+v", d.Issue)
	}
}
