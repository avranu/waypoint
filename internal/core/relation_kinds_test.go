package core

import (
	"slices"
	"strings"
	"testing"
)

// WP-1036: relations say what kind they are (tension, supersedes, a
// follow-up's fix or expansion) and who stands behind them.
func TestRelationKinds(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Kinds", "")
	simple := e.issue(e.admin, IssueInput{Project: p.Key, Title: "A simple screen for executives"})
	detail := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Every detail on screen for engineers"})
	old := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Keep sessions in memory"})
	repl := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Keep sessions in SQLite"})
	first := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Export to CSV"})
	later := e.issue(e.admin, IssueInput{Project: p.Key, Title: "CSV export drops quotes"})
	find := func(key, other string) *Relation {
		t.Helper()
		for _, r := range e.get(e.admin, key).Relations {
			if r.Issue.Key == other {
				return &r
			}
		}
		return nil
	}
	rows := func(a, b *IssueDetail) int {
		t.Helper()
		var n int
		if err := e.s.db.R.QueryRow(`SELECT count(*) FROM issue_relations WHERE (source_id = ? AND target_id = ?) OR (source_id = ? AND target_id = ?)`,
			a.ID, b.ID, b.ID, a.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if err := e.s.AddRelation(e.ctx, e.admin, simple.Key, "tension", detail.Key); err != nil {
		t.Fatal(err)
	}
	if err := e.s.AddRelation(e.ctx, e.admin, detail.Key, "tension", simple.Key); err != nil {
		t.Fatal(err)
	}
	if r := find(simple.Key, detail.Key); r == nil || r.Kind != "tension" {
		t.Errorf("simple → detail: %+v", r)
	}
	if r := find(detail.Key, simple.Key); r == nil || r.Kind != "tension" {
		t.Errorf("detail → simple: %+v", r)
	}
	if n := rows(simple, detail); n != 1 {
		t.Errorf("a symmetric kind stored %d times", n)
	}

	if err := e.s.AddRelation(e.ctx, e.admin, old.Key, "superseded_by", repl.Key); err != nil {
		t.Fatal(err)
	}
	if r := find(repl.Key, old.Key); r == nil || r.Kind != "supersedes" {
		t.Errorf("replacement: %+v", r)
	}
	if r := find(old.Key, repl.Key); r == nil || r.Kind != "superseded_by" {
		t.Errorf("replaced: %+v", r)
	}

	if _, err := e.s.Relate(e.ctx, e.admin, later.Key, RelationInput{Kind: "follows", Issue: first.Key, Subkind: "Fix"}); err != nil {
		t.Fatal(err)
	}
	if r := find(later.Key, first.Key); r == nil || r.Kind != "follows" || r.Subkind != "fix" {
		t.Errorf("fix: %+v", r)
	}
	if r := find(first.Key, later.Key); r == nil || r.Kind != "followed_by" || r.Subkind != "fix" {
		t.Errorf("fix from the other end: %+v", r)
	}
	if _, err := e.s.Relate(e.ctx, e.admin, first.Key, RelationInput{Kind: "followed_by", Issue: later.Key, Subkind: "expansion"}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.AddRelation(e.ctx, e.admin, later.Key, "follows", first.Key); err != nil {
		t.Fatal(err)
	}
	if r := find(later.Key, first.Key); r == nil || r.Subkind != "expansion" {
		t.Errorf("changed subkind: %+v", r)
	}
	if n := rows(first, later); n != 1 {
		t.Errorf("saying it again added a row: %d", n)
	}
	if _, err := e.s.Relate(e.ctx, e.admin, later.Key, RelationInput{Kind: "follows", Issue: first.Key, Subkind: "rewrite"}); err == nil || !strings.Contains(err.Error(), "fix or expansion") {
		t.Errorf("an unknown subkind: %v", err)
	}
	if _, err := e.s.Relate(e.ctx, e.admin, simple.Key, RelationInput{Kind: "blocks", Issue: repl.Key, Subkind: "fix"}); err == nil {
		t.Error("blocks took a subkind")
	}
	if err := e.s.AddRelation(e.ctx, e.admin, simple.Key, "contradicts", repl.Key); err == nil || !strings.Contains(err.Error(), "tension") {
		t.Errorf("an unknown kind: %v", err)
	}

	hist, err := e.s.History(e.ctx, e.admin, first.Key, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(hist, func(h Event) bool { return h.Field == "relation" && h.New == "followed_by (expansion) "+later.Key }) {
		t.Errorf("history: %+v", hist)
	}

	if r := find(simple.Key, detail.Key); r.Basis != "person" || r.CreatedBy == nil || r.CreatedBy.ID != e.admin.User.ID || r.CreatedAt.IsZero() {
		t.Errorf("a person's relation: %+v", r)
	}
	agent := agentActor(e.admin, "anthropic/claude-opus-5-5")
	if err := e.s.AddRelation(e.ctx, agent, repl.Key, "relates", first.Key); err != nil {
		t.Fatal(err)
	}
	if r := find(first.Key, repl.Key); r == nil || r.Basis != "agent" || r.CreatedAgent != "anthropic/claude-opus-5-5" {
		t.Errorf("an agent's relation: %+v", r)
	}
	if err := e.s.AddRelation(e.ctx, aiActorNamed("Auto relate", p.ID, ""), detail.Key, "relates", repl.Key); err != nil {
		t.Fatal(err)
	}
	if r := find(detail.Key, repl.Key); r == nil || r.Basis != "inferred" {
		t.Errorf("Waypoint's relation: %+v", r)
	}

	got := e.search(e.admin, "is:tension")
	slices.Sort(got)
	want := []string{simple.Key, detail.Key}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("is:tension = %v, want %v", got, want)
	}
	if got := e.search(e.admin, "is:superseded"); !slices.Equal(got, []string{old.Key}) {
		t.Errorf("is:superseded = %v", got)
	}
}

// Prose that states a tension or a replacement suggests it, never sure
// enough to link by itself; words that deny it, a question, or a clause about
// another issue don't.
func TestStatedRelations(t *testing.T) {
	got := statedDependencies("This is in tension with WP-3. It replaces WP-4, and is replaced by [[WP-5]]. It's not blocked by WP-6. Conflicts with WP-7.")
	kinds := map[string]string{}
	for _, d := range got {
		kinds[d.key] = d.kind
	}
	want := map[string]string{"WP-3": "tension", "WP-4": "supersedes", "WP-5": "superseded_by", "WP-7": "tension"}
	for k, v := range want {
		if kinds[k] != v {
			t.Errorf("%s = %q, want %q (all: %v)", k, kinds[k], v, kinds)
		}
	}
	if _, ok := kinds["WP-6"]; ok {
		t.Errorf("a denied dependency counted: %v", kinds)
	}
	for _, d := range got {
		if d.sure {
			t.Errorf("%s %s is sure enough to link", d.key, d.kind)
		}
	}
	for _, text := range []string{
		"I don't think this is in tension with WP-3.",
		"Does this conflict with WP-7?",
		"Unlike WP-3, which replaces WP-4, this keeps both.",
		"Someone said it isn't really at odds with WP-5 at all.",
		"Not a blocker: needs WP-6 only for the docs.",
	} {
		if got := statedDependencies(text); len(got) != 0 {
			t.Errorf("%q states %+v", text, got)
		}
	}
	if got := statedDependencies("We noticed this after WP-9 shipped."); len(got) != 1 || got[0].sure {
		t.Errorf("after: %+v", got)
	}
}

// Work linked as in tension with a goal doesn't serve it: the goal counts
// it apart, and the task's brief says so.
func TestGoalTension(t *testing.T) {
	e := newEnv(t)
	p := e.project(e.admin, "Goals", "")
	g := e.goal(e.admin, p.Key, "Executives read the board at a glance")
	task := e.issue(e.admin, IssueInput{Project: p.Key, Title: "Show every field on the board"})
	if _, err := e.s.AddItemLink(e.ctx, e.admin, LinkInput{From: LinkEnd{Type: KIssue, Key: task.Key}, To: LinkEnd{Type: KDesign, ID: g.ID}, Rel: "tension"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AddItemLink(e.ctx, e.admin, LinkInput{From: LinkEnd{Type: KIssue, Key: task.Key}, To: LinkEnd{Ref: "https://example.com"}, Rel: "tension"}); err == nil {
		t.Error("tension to a URL")
	}
	o := e.goals(e.admin, p.Key)
	var view *GoalView
	for _, v := range o.Goals {
		if v.Note.ID == g.ID {
			view = v
		}
	}
	if view == nil || view.Coverage.Tension != 1 || view.Coverage.Items != 0 {
		t.Fatalf("goal coverage: %+v", view)
	}
	if gr := e.grounding(task.Key); strings.HasSuffix(gr, "_goal") {
		t.Errorf("a task in tension with a goal traces to it: %s", gr)
	}
	refs := e.s.DesignNotesFor(e.ctx, e.admin, task.ID)
	if len(refs) != 1 || refs[0].ID != g.ID || !refs[0].Tension {
		t.Errorf("brief's notes: %+v", refs)
	}
	e.serve(e.admin, task.Key, g.ID)
	if refs := e.s.DesignNotesFor(e.ctx, e.admin, task.ID); len(refs) != 1 || refs[0].Tension {
		t.Errorf("served and in tension: %+v", refs)
	}
}
