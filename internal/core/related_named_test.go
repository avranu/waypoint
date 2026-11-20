package core

import (
	"fmt"
	"strings"
	"testing"
)

func relationKinds(d *IssueDetail) map[string]string {
	out := map[string]string{}
	for _, r := range d.Relations {
		out[r.Issue.Key] = r.Kind
	}
	return out
}

// Naming a task relates it in the save itself: what the save returns has it.
func TestNamedTasksRelateOnSave(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Named", "kanban")
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "One"})
	b := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Two"})

	c := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Three", Description: "See " + a.Key + " and `" + b.Key + "`."})
	if got := relationKinds(c); len(got) != 1 || got[a.Key] != "relates" {
		t.Fatalf("on create: %v, want only %s (code doesn't count)", got, a.Key)
	}
	if got := relationKinds(e.get(e.admin, a.Key)); got[c.Key] != "relates" {
		t.Errorf("the other side: %v", got)
	}

	text := "See " + a.Key + ", " + b.Key + " and " + c.Key + "."
	d, err := e.s.UpdateIssue(e.ctx, e.admin, c.Key, IssuePatch{Description: &text})
	if err != nil {
		t.Fatal(err)
	}
	if got := relationKinds(d); len(got) != 2 || got[b.Key] != "relates" {
		t.Errorf("on save: %v (naming itself doesn't count)", got)
	}
}

// A description autosaves as it's typed: a half-typed key's link goes when
// the text stops naming it, and a relation someone chose stays.
func TestNamedRelationFollowsTheText(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Named", "kanban")
	half := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Half"})
	full := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Full"})
	chosen := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Chosen"})
	c := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Text"})
	if err := e.s.AddRelation(e.ctx, e.admin, c.Key, "relates", chosen.Key); err != nil {
		t.Fatal(err)
	}
	save := func(text string) map[string]string {
		t.Helper()
		d, err := e.s.UpdateIssue(e.ctx, e.admin, c.Key, IssuePatch{Description: &text})
		if err != nil {
			t.Fatal(err)
		}
		return relationKinds(d)
	}
	if got := save("See " + half.Key + " and " + chosen.Key); got[half.Key] != "relates" {
		t.Fatalf("half typed: %v", got)
	}
	got := save("See " + full.Key)
	if _, ok := got[half.Key]; ok || got[full.Key] != "relates" {
		t.Errorf("after the key changed: %v", got)
	}
	if got[chosen.Key] != "relates" {
		t.Errorf("a chosen relation went with the text: %v", got)
	}
}

// Removing a link the text made sticks through later saves of that text.
func TestRemovedNamedRelationStaysGone(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Named", "kanban")
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "One"})
	c := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Text", Description: "See " + a.Key})
	if len(c.Relations) != 1 {
		t.Fatalf("relations = %v", c.Relations)
	}
	if err := e.s.RemoveRelation(e.ctx, e.admin, c.Key, c.Relations[0].ID); err != nil {
		t.Fatal(err)
	}
	text := "See " + a.Key + ", with more words"
	d, err := e.s.UpdateIssue(e.ctx, e.admin, c.Key, IssuePatch{Description: &text})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Relations) != 0 {
		t.Errorf("came back: %v", d.Relations)
	}
}

// Only what a person writes relates; agents name tasks in passing. A task
// the writer can't see, or the task's own parent, isn't linked either.
func TestNamedRelationLimits(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Named", "kanban")
	sam := e.user("sam", false)
	secret := "Secret"
	sp, err := e.s.CreateProject(e.ctx, sam, ProjectInput{Name: &secret})
	if err != nil {
		t.Fatal(err)
	}
	private := e.issue(sam, IssueInput{Project: sp.Key, Title: "Private"})
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "One"})
	parent := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Parent"})

	ana := e.user("ana", false)
	if err := e.s.SetMember(e.ctx, e.admin, p.Key, "ana", "editor"); err != nil {
		t.Fatal(err)
	}
	if got := e.issue(ana, IssueInput{Project: p.Key, Title: "Reach", Description: "See " + private.Key}); len(got.Relations) != 0 {
		t.Errorf("reached a private task: %v", got.Relations)
	}
	child := e.issue(e.admin, IssueInput{Project: p.Key, Parent: parent.Key, Title: "Child", Description: "Part of " + parent.Key})
	if len(child.Relations) != 0 {
		t.Errorf("linked its parent: %v", child.Relations)
	}

	agent := *e.admin
	agent.Agent = "anthropic/claude-opus-5-5"
	if got := e.issue(&agent, IssueInput{Project: p.Key, Title: "Filed by an agent", Description: "See " + a.Key}); len(got.Relations) != 0 {
		t.Errorf("an agent's text linked: %v", got.Relations)
	}
	// A person's account writing a model's words, or a forge's mirrored text.
	carried := *e.admin
	carried.textAgent = "waypoint"
	if got := e.issue(&carried, IssueInput{Project: p.Key, Title: "Audit finding", Description: "See " + a.Key}); len(got.Relations) != 0 {
		t.Errorf("text carried for an agent linked: %v", got.Relations)
	}
	mirror := *e.admin
	mirror.forgeSync = true
	if got := e.issue(&mirror, IssueInput{Project: p.Key, Title: "Mirrored", Description: "See " + a.Key}); len(got.Relations) != 0 {
		t.Errorf("mirrored text linked: %v", got.Relations)
	}
	if got := e.issue(SystemActor(), IssueInput{Project: p.Key, Title: "System", Description: "See " + a.Key}); len(got.Relations) != 0 {
		t.Errorf("a system write linked: %v", got.Relations)
	}
}

// Two issues naming each other share one link, which lasts while either
// text still names the other.
func TestNamedRelationLastsWhileEitherNames(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Named", "kanban")
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "One"})
	b := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Two", Description: "See " + a.Key})
	save := func(key, text string) {
		t.Helper()
		if _, err := e.s.UpdateIssue(e.ctx, e.admin, key, IssuePatch{Description: &text}); err != nil {
			t.Fatal(err)
		}
	}
	save(a.Key, "Back to "+b.Key)
	save(b.Key, "Nothing now")
	if got := e.get(e.admin, a.Key).Relations; len(got) != 1 {
		t.Fatalf("after one side dropped the key: %+v", got)
	}
	save(a.Key, "Nothing here either")
	if got := e.get(e.admin, a.Key).Relations; len(got) != 0 {
		t.Errorf("after both dropped it: %+v", got)
	}
}

// A long list relates only its first keys; the rest stay mentions.
func TestNamedRelationsCapped(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Named", "kanban")
	var keys []string
	for n := range namedLinksMax + 3 {
		keys = append(keys, e.issue(e.admin, IssueInput{Project: p.Key, Title: fmt.Sprint("t", n)}).Key)
	}
	c := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Changelog", Description: strings.Join(keys, " ")})
	if len(c.Relations) != namedLinksMax {
		t.Fatalf("relations = %d", len(c.Relations))
	}
	if relationKinds(c)[keys[0]] != "relates" || relationKinds(c)[keys[len(keys)-1]] != "" {
		t.Errorf("not first named first: %v", relationKinds(c))
	}
	more := strings.Join(keys, " ") + " " + e.issue(e.admin, IssueInput{Project: p.Key, Title: "More"}).Key
	d, err := e.s.UpdateIssue(e.ctx, e.admin, c.Key, IssuePatch{Description: &more})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Relations) != namedLinksMax {
		t.Errorf("a later save went past the cap: %d", len(d.Relations))
	}
}

// A link the text made doesn't say the tasks go together: wp batch doesn't
// claim the other task, a session's scope doesn't reach it, and autopilot
// doesn't wait on it. One someone chose does all three.
func TestNamedRelationIsNotSharedWork(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Named", "kanban")
	side := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Side", Agent: "any", Status: "todo"})
	chosen := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Chosen", Agent: "any", Status: "todo"})
	lead := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Lead", Agent: "any", Status: "todo", Priority: "high", Description: "Compare how " + side.Key + " did it."})
	if err := e.s.AddRelation(e.ctx, e.admin, lead.Key, "relates", chosen.Key); err != nil {
		t.Fatal(err)
	}
	load := func(id int64) *Issue {
		t.Helper()
		i, err := e.s.issueByID(e.ctx, e.s.db.R, id, false)
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	if e.s.boundCovers(e.ctx, []string{lead.Key}, load(side.ID)) || !e.s.boundCovers(e.ctx, []string{lead.Key}, load(chosen.ID)) {
		t.Error("a session's scope should reach only the chosen relation")
	}
	if e.s.relatedToAny(e.ctx, load(side.ID), []int64{lead.ID}) || !e.s.relatedToAny(e.ctx, load(chosen.ID), []int64{lead.ID}) {
		t.Error("autopilot should wait only on the chosen relation")
	}
	task, err := e.s.AgentBatch(e.ctx, agentActor(e.admin, "anthropic/claude-opus-5-5"), AgentQuery{Project: p.Key})
	if err != nil || task == nil || task.Issue.Key != lead.Key {
		t.Fatalf("batch = %+v, %v", task, err)
	}
	if len(task.Related) != 1 || task.Related[0].Key != chosen.Key {
		t.Errorf("batch claimed %+v, want only %s", task.Related, chosen.Key)
	}
	if got := e.get(e.admin, side.Key); got.ClaimedBy != "" {
		t.Errorf("%s claimed by %s", side.Key, got.ClaimedBy)
	}
}

// A chosen relation replaces the plain link the text made, and in a project
// with AutoRelate a dependency the text states still becomes one.
func TestChosenRelationReplacesNamed(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Named", "kanban")
	a := e.issue(e.admin, IssueInput{Project: p.Key, Title: "One"})
	c := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Text", Description: "See " + a.Key})
	if err := e.s.AddRelation(e.ctx, e.admin, c.Key, "blocked_by", a.Key); err != nil {
		t.Fatal(err)
	}
	if got := e.get(e.admin, c.Key).Relations; len(got) != 1 || got[0].Kind != "blocked_by" {
		t.Errorf("relations = %+v", got)
	}

	// With AutoRelate, a dependency the text states becomes one, not a plain link.
	name := "Auto"
	ap, err := e.s.CreateProject(e.ctx, e.admin, ProjectInput{Name: &name, Settings: &ProjectSettings{AutoRelate: true}})
	if err != nil {
		t.Fatal(err)
	}
	dep := e.issue(e.admin, IssueInput{Project: ap.Key, Title: "Library rate limits"})
	w := e.issue(e.admin, IssueInput{Project: ap.Key, Title: "Walker", Description: "Blocked by " + dep.Key + "."})
	e.s.Wait()
	if got := relationKinds(e.get(e.admin, w.Key)); got[dep.Key] != "blocked_by" {
		t.Errorf("stated dependency with AutoRelate: %v", got)
	}
}
