package core

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Regressions from the WP-D10 adversarial review.

func TestDesignMoveGuards(t *testing.T) {
	e := newEnv(t)
	alpha := e.project(e.admin, "Alpha", "")
	beta := e.project(e.admin, "Beta", "")
	agent := agentActor(e.admin, "x/y")
	parent, _ := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("Parent"), Project: &alpha.Key})
	child, _ := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("Child"), Parent: Some(parent.ID)})
	top, _ := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("Top"), Project: &alpha.Key})
	// Agents can't move approved notes, however the request is phrased.
	if _, err := e.s.UpdateDesign(e.ctx, agent, child.ID, DesignInput{Project: &beta.Key, Parent: Some(parent.ID)}); !errors.Is(err, ErrForbidden) && !IsInvalid(err) {
		t.Errorf("agent move with same parent = %v", err)
	}
	if _, err := e.s.UpdateDesign(e.ctx, agent, top.ID, DesignInput{Project: &beta.Key, Parent: Null[int64]()}); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent move with null parent = %v", err)
	}
	if n, _ := e.s.GetDesign(e.ctx, e.admin, top.ID); n.ProjectKey != alpha.Key {
		t.Errorf("top moved to %s", n.ProjectKey)
	}
	// A person's move can't leave a sub-note under a parent elsewhere.
	if _, err := e.s.UpdateDesign(e.ctx, e.admin, child.ID, DesignInput{Project: &beta.Key, Parent: Some(parent.ID)}); !IsInvalid(err) {
		t.Errorf("move keeping a parent in another project = %v", err)
	}
	n, err := e.s.UpdateDesign(e.ctx, e.admin, child.ID, DesignInput{Project: &beta.Key})
	if err != nil || n.ProjectKey != beta.Key || n.ParentID != nil || n.UpdatedBy == nil {
		t.Fatalf("person move = %+v, %v", n, err)
	}
	// Viewers of Alpha don't see notes in Beta through children lists.
	bob := e.user("bob", false)
	if err := e.s.SetMember(e.ctx, e.admin, alpha.Key, "bob", "viewer"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.UpdateDesign(e.ctx, e.admin, child.ID, DesignInput{Project: &alpha.Key, Parent: Some(parent.ID)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.UpdateDesign(e.ctx, e.admin, parent.ID, DesignInput{}); err != nil {
		t.Fatal(err)
	}
	// Force the inconsistent state an old bug could leave, then read as bob.
	if _, err := e.s.db.W.Exec(`UPDATE design_notes SET project_id = ? WHERE id = ?`, beta.ID, child.ID); err != nil {
		t.Fatal(err)
	}
	if p, _ := e.s.GetDesign(e.ctx, bob, parent.ID); len(p.Children) != 0 {
		t.Errorf("bob sees children in Beta: %+v", p.Children)
	}
}

func TestCommentSearchCapAfterAccess(t *testing.T) {
	e := newEnv(t)
	secret := e.project(e.admin, "Secret", "")
	mine := e.project(e.admin, "Mine", "")
	bob := e.user("bob", false)
	if err := e.s.SetMember(e.ctx, e.admin, mine.Key, "bob", "editor"); err != nil {
		t.Fatal(err)
	}
	hidden := e.issue(e.admin, IssueInput{Project: secret.Key, Title: "hidden"})
	first, err := e.s.AddComment(e.ctx, e.admin, hidden.Key, "zebra zebra zebra")
	if err != nil {
		t.Fatal(err)
	}
	// 304 copies of it, more than the search's cap of 300 with the first. In
	// one statement: through AddComment, each scans its text for mentions, and
	// under -race the 305 took five to seven minutes (comment_search is filled
	// by a trigger either way).
	if _, err := e.s.db.W.ExecContext(e.ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 304)
		INSERT INTO comments(issue_id, author_id, agent, body, created_at, updated_at)
		SELECT c.issue_id, c.author_id, c.agent, c.body, c.created_at, c.updated_at FROM comments c, n WHERE c.id = ?`, first.ID); err != nil {
		t.Fatal(err)
	}
	vis := e.issue(e.admin, IssueInput{Project: mine.Key, Title: "visible"})
	if _, err := e.s.AddComment(e.ctx, e.admin, vis.Key, "a zebra here"); err != nil {
		t.Fatal(err)
	}
	r, err := e.s.SearchAll(e.ctx, bob, AllQuery{Text: "zebra", Kinds: []string{"comments"}})
	if err != nil || r.Groups[0].Total != 1 || r.Groups[0].Hits[0].Key != vis.Key {
		t.Errorf("bob's comment search = %+v, %v", r.Groups, err)
	}
}

func TestWikilinkScopeAndProvenance(t *testing.T) {
	e := newEnv(t)
	alpha := e.project(e.admin, "Alpha", "")
	beta := e.project(e.admin, "Beta", "")
	agent := agentActor(e.admin, "x/y")
	// A term only in Beta isn't what [[Orbit]] in Alpha means.
	if _, err := e.s.CreateGlossary(e.ctx, e.admin, GlossaryInput{Term: str("Orbit"), Project: &beta.Key}); err != nil {
		t.Fatal(err)
	}
	pg, _ := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("Alpha page"), Body: str("About [[Orbit]]."), Project: Some(alpha.Key)})
	if links, _ := e.s.ItemLinks(e.ctx, e.admin, LinkEnd{Type: KPage, ID: pg.ID}); len(links) != 0 {
		t.Errorf("cross-project wikilink: %+v", links)
	}
	// Typed forward references connect when their target appears; links a
	// locked entry gains are credited to its author, not the agent.
	yes := true
	locked, err := e.s.CreateGlossary(e.ctx, e.admin, GlossaryInput{Term: str("Locked"), Project: &alpha.Key, Locked: &yes,
		Definition: str("See [[Later]], [[glossary:Typed]] and [[design:Dn|the note]]; Later is plain text here.")})
	if err != nil {
		t.Fatal(err)
	}
	for _, term := range []string{"Later", "Typed"} {
		if _, err := e.s.CreateGlossary(e.ctx, agent, GlossaryInput{Term: str(term), Project: &alpha.Key}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.s.CreateDesign(e.ctx, agent, DesignInput{Title: str("Dn"), Project: &alpha.Key}); err != nil {
		t.Fatal(err)
	}
	links, _ := e.s.ItemLinks(e.ctx, e.admin, LinkEnd{Type: KGlossary, ID: locked.ID})
	out := 0
	for _, l := range links {
		if l.Direction == "out" {
			out++
			if l.CreatedAgent != "" || l.CreatedBy == nil {
				t.Errorf("link from a person's locked entry credited to %q", l.CreatedAgent)
			}
		}
	}
	if out != 3 {
		t.Errorf("locked entry links = %d, want 3: %+v", out, links)
	}
	// Re-saving keeps the links' dates.
	before := links[0].CreatedAt
	e.setClock(e.clockNow().Add(time.Hour))
	if _, err := e.s.UpdateGlossary(e.ctx, e.admin, locked.ID, GlossaryInput{Definition: str("See [[Later]], [[glossary:Typed]] and [[design:Dn|the note]]. Edited.")}); err != nil {
		t.Fatal(err)
	}
	if again, _ := e.s.ItemLinks(e.ctx, e.admin, LinkEnd{Type: KGlossary, ID: locked.ID}); !again[0].CreatedAt.Equal(before) {
		t.Errorf("resync rewrote link dates: %v → %v", before, again[0].CreatedAt)
	}
	// Pending agent additions don't add links; approved ones do.
	note, _ := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("Host"), Project: &alpha.Key})
	n, _ := e.s.AddDesignAddition(e.ctx, agent, note.ID, "Relates to [[Later]].")
	if l, _ := e.s.ItemLinks(e.ctx, e.admin, LinkEnd{Type: KDesign, ID: note.ID}); len(l) != 0 {
		t.Errorf("pending addition linked: %+v", l)
	}
	if _, err := e.s.ReviewDesign(e.ctx, e.admin, n.Additions[0].ID, "approve", ""); err != nil {
		t.Fatal(err)
	}
	if l, _ := e.s.ItemLinks(e.ctx, e.admin, LinkEnd{Type: KDesign, ID: note.ID}); len(l) != 1 {
		t.Errorf("approved addition links = %+v", l)
	}
	// Resolve doesn't reveal private projects.
	bob := e.user("bob", false)
	_, err1 := e.s.Resolve(e.ctx, bob, "nothing", alpha.Key)
	_, err2 := e.s.Resolve(e.ctx, bob, "nothing", "NOPE")
	if err1 == nil || err2 == nil || strings.Contains(err1.Error(), "nothing") {
		t.Errorf("resolve errors differ: %v / %v", err1, err2)
	}
}

func TestPageIssueLinksAgents(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Pages", "")
	agent := agentActor(e.admin, "x/y")
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "t"})
	pg, _ := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("p"), Project: Some(p.Key), Issues: &[]string{i.Key}})
	if err := e.s.LinkPage(e.ctx, agent, pg.ID, i.Key, false); !errors.Is(err, ErrForbidden) {
		t.Errorf("agent unlinked a person's page link: %v", err)
	}
	if l, _ := e.s.ItemLinks(e.ctx, agent, LinkEnd{Type: KIssue, Key: i.Key}); len(l) != 1 || l[0].CanRemove {
		t.Errorf("agent's view = %+v", l)
	}
	pg2, _ := e.s.CreatePage(e.ctx, e.admin, PageInput{Title: str("p2"), Project: Some(p.Key)})
	if err := e.s.LinkPage(e.ctx, agent, pg2.ID, i.Key, true); err != nil {
		t.Fatal(err)
	}
	if err := e.s.LinkPage(e.ctx, agent, pg2.ID, i.Key, false); err != nil {
		t.Errorf("agent removing its own link: %v", err)
	}
}

func TestStaleEditApproval(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Stale", "")
	agent := agentActor(e.admin, "x/y")
	n, _ := e.s.CreateDesign(e.ctx, e.admin, DesignInput{Title: str("N"), Body: str("v1"), Project: &p.Key})
	tick := func() { e.setClock(e.clockNow().Add(time.Minute)) }
	tick()
	e1, _ := e.s.UpdateDesign(e.ctx, agent, n.ID, DesignInput{Body: str("agent v2")})
	tick()
	if _, err := e.s.UpdateDesign(e.ctx, e.admin, n.ID, DesignInput{Body: str("human v3")}); err != nil {
		t.Fatal(err)
	}
	tick()
	e3, _ := e.s.UpdateDesign(e.ctx, agent, n.ID, DesignInput{Body: str("agent v4")})
	if !e3.Proposals[0].Stale || e3.Proposals[1].Stale {
		t.Fatalf("before: %+v", e3.Proposals)
	}
	tick()
	got, err := e.s.ReviewDesign(e.ctx, e.admin, e1.Proposals[0].ID, "approve", "")
	if err != nil || got.Body != "agent v2" {
		t.Fatalf("approve old = %v %v", got.Body, err)
	}
	if len(got.Proposals) != 1 || !got.Proposals[0].Stale {
		t.Errorf("v4 was based on v3, now replaced; should be stale: %+v", got.Proposals)
	}
}

func TestApproveVersionAndTakeSteps(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Ver", "")
	agent := agentActor(e.admin, "x/y")
	n, _ := e.s.CreateDesign(e.ctx, agent, DesignInput{Title: str("Prop"), Body: str("one"), Project: &p.Key})
	seen := n.UpdatedAt.Format(time.RFC3339Nano)
	e.setClock(e.clockNow().Add(time.Minute))
	if _, err := e.s.UpdateDesign(e.ctx, agent, n.ID, DesignInput{Body: str("two")}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ApproveDesign(e.ctx, e.admin, n.ID, seen); !errors.Is(err, ErrConflict) {
		t.Errorf("approve stale view = %v", err)
	}
	cur, _ := e.s.GetDesign(e.ctx, e.admin, n.ID)
	if got, err := e.s.ApproveDesign(e.ctx, e.admin, n.ID, cur.UpdatedAt.Format(time.RFC3339Nano)); err != nil || !got.Approved {
		t.Errorf("approve current = %v", err)
	}
	// A workflow step for another agent can't be taken by name.
	w, err := e.s.CreateWorkflow(e.ctx, e.admin, p.Key, WorkflowInput{Name: str("W"), Steps: &[]WorkflowStep{{Name: "a", Agent: "openai"}, {Name: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	i := e.issue(e.admin, IssueInput{Project: p.Key, Title: "stepped", Workflow: fmt.Sprint(w.ID)})
	opus := agentActor(e.admin, "anthropic/claude-opus-5-5")
	if _, err := e.s.AgentTake(e.ctx, opus, i.Key, ""); !IsInvalid(err) || !strings.Contains(err.Error(), "openai") {
		t.Errorf("take another agent's step = %v", err)
	}
}

func TestAgentTimeOverlapsAndLapses(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Time", "")
	opus := agentActor(e.admin, "anthropic/claude-opus-5-5")
	start := e.clockNow()
	var keys []string
	for i := 0; i < 3; i++ {
		keys = append(keys, e.issue(e.admin, IssueInput{Project: p.Key, Title: fmt.Sprint("t", i)}).Key)
	}
	for _, k := range keys {
		if _, err := e.s.AgentTake(e.ctx, opus, k, ""); err != nil {
			t.Fatal(err)
		}
	}
	e.setClock(e.clockNow().Add(30 * time.Minute))
	for _, k := range keys {
		if _, err := e.s.AgentRelease(e.ctx, opus, k, ReleaseInput{}); err != nil {
			t.Fatal(err)
		}
	}
	rep, _ := e.s.GetTimeReport(e.ctx, e.admin, start.Add(-time.Hour), e.clockNow().Add(time.Hour), "")
	if rep.AgentSeconds != 30*60 {
		t.Errorf("three parallel runs over 30 min = %d s", rep.AgentSeconds)
	}
	// Coming back after the lease lapsed doesn't count the gap.
	e.setClock(e.clockNow().Add(time.Hour))
	if _, err := e.s.AgentTake(e.ctx, opus, keys[0], ""); err != nil {
		t.Fatal(err)
	}
	e.setClock(e.clockNow().Add(ClaimLease + time.Hour)) // lapsed, sweep not run
	if _, err := e.s.AgentTake(e.ctx, opus, keys[0], ""); err != nil {
		t.Fatal(err)
	}
	e.setClock(e.clockNow().Add(10 * time.Minute))
	rep, _ = e.s.GetTimeReport(e.ctx, e.admin, start.Add(-time.Hour), e.clockNow().Add(time.Hour), "")
	want := int64(30*60 + ClaimLease.Seconds() + 10*60)
	if rep.AgentSeconds != want {
		t.Errorf("with a lapse = %d s, want %d", rep.AgentSeconds, want)
	}
	// The sweep clears lapsed claims.
	e.setClock(e.clockNow().Add(ClaimLease + time.Minute))
	if err := e.s.expireRuns(e.ctx); err != nil {
		t.Fatal(err)
	}
	if d := e.get(e.admin, keys[0]); d.ClaimedBy != "" || d.ClaimedAt != nil {
		t.Errorf("lapsed claim still shown: %q", d.ClaimedBy)
	}
}
