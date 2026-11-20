package core

import (
	"fmt"
	"strings"
	"testing"
)

func TestStatedDependencies(t *testing.T) {
	got := statedDependencies("Needs WP-12 merged. This is blocked by [[API-3]], and WP-7 must land first.\n" +
		"It blocks APP-40 and comes before APP-41. See WP-99 for background. `after WP-50` is code.")
	want := []string{
		"WP-12 blocked_by maybe Needs WP-12 merged.",
		"API-3 blocked_by sure This is blocked by [[API-3]], and WP-7 must land first.",
		"WP-7 blocked_by sure This is blocked by [[API-3]], and WP-7 must land first.",
		"APP-40 blocks sure It blocks APP-40 and comes before APP-41.",
		"APP-41 blocks maybe It blocks APP-40 and comes before APP-41.",
	}
	var lines []string
	for _, d := range got {
		sure := "maybe"
		if d.sure {
			sure = "sure"
		}
		lines = append(lines, d.key+" "+d.kind+" "+sure+" "+d.sentence)
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// relateFixture is a project with an issue about the Sanborn map harvest
// and the issues around it.
type relateFixture struct {
	member                                *Actor // edits Maps and Other, can't see Secret
	src, alike, sameTitle, waits, done    *IssueDetail
	linked, unrelated, private, otherProj *IssueDetail
}

func newRelateFixture(e *env, settings *ProjectSettings) relateFixture {
	name := "Maps"
	p, err := e.s.CreateProject(e.ctx, e.admin, ProjectInput{Name: &name, Settings: settings})
	if err != nil {
		e.t.Fatal(err)
	}
	var f relateFixture
	for n, topic := range []string{"login form styling", "billing invoice emails", "dark mode colours", "calendar export feed", "avatar upload sizes", "password reset tokens"} {
		e.issue(e.admin, IssueInput{Project: p.Key, Title: fmt.Sprintf("Filler %d: %s", n, topic), Description: "Unrelated work on " + topic + "."})
	}
	f.waits = e.issue(e.admin, IssueInput{Project: p.Key, Title: "Library of Congress rate limits for map downloads"})
	f.alike = e.issue(e.admin, IssueInput{Project: p.Key, Title: "Weekly walk stops early on production",
		Description: "On production the weekly Sanborn walk stops at a campus town and never resumes."})
	f.sameTitle = e.issue(e.admin, IssueInput{Project: p.Key, Title: "Sanborn harvest: campus towns!", Description: "Reported twice."})
	f.done = e.issue(e.admin, IssueInput{Project: p.Key, Title: "Sanborn harvest for campus towns, first try",
		Description: "The Sanborn harvest skipped campus towns; county sheets."})
	done := CatDone
	if _, err := e.s.UpdateIssue(e.ctx, e.admin, f.done.Key, IssuePatch{Status: &done}); err != nil {
		e.t.Fatal(err)
	}
	f.linked = e.issue(e.admin, IssueInput{Project: p.Key, Title: "Sanborn county sheets index", Description: "Index the Sanborn county sheets the harvest found in campus towns."})
	f.unrelated = e.issue(e.admin, IssueInput{Project: p.Key, Title: "Dark mode toggle in settings", Description: "A toggle for dark mode colours."})
	other := "Other"
	op, err := e.s.CreateProject(e.ctx, e.admin, ProjectInput{Name: &other})
	if err != nil {
		e.t.Fatal(err)
	}
	f.otherProj = e.issue(e.admin, IssueInput{Project: op.Key, Title: "Sanborn harvest: campus towns",
		Description: "The Sanborn harvest skips campus towns' county sheets."})
	sam := e.user("sam", false)
	secret := "Secret"
	sp, err := e.s.CreateProject(e.ctx, sam, ProjectInput{Name: &secret})
	if err != nil {
		e.t.Fatal(err)
	}
	f.private = e.issue(sam, IssueInput{Project: sp.Key, Title: "Sanborn harvest skips campus towns", Description: "Sam's own copy of the Sanborn harvest campus towns sheets."})
	f.member = e.user("ana", false)
	for _, key := range []string{p.Key, op.Key} {
		if err := e.s.SetMember(e.ctx, e.admin, key, "ana", "editor"); err != nil {
			e.t.Fatal(err)
		}
	}
	f.src = e.issue(e.admin, IssueInput{Project: p.Key, Title: "Sanborn harvest: campus towns",
		Description: "Production's weekly Sanborn walk reaches a campus town, stops before its county sheets and never resumes. Blocked by " + f.waits.Key + "."})
	return f
}

func TestSuggestRelations(t *testing.T) {
	e := newEnv(t)
	f := newRelateFixture(e, nil)
	if err := e.s.AddRelation(e.ctx, e.admin, f.src.Key, "relates", f.linked.Key); err != nil {
		t.Fatal(err)
	}
	res, err := e.s.SuggestRelations(e.ctx, f.member, f.src.Key)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]RelationSuggestion{}
	var order []string
	for _, s := range res.Suggestions {
		got[s.Key] = s
		order = append(order, s.Key)
	}
	if s := got[f.waits.Key]; s.Kind != "blocked_by" || s.Confidence != "high" || !strings.Contains(s.Reason, "Blocked by "+f.waits.Key) {
		t.Errorf("stated dependency: %+v", s)
	}
	if s := got[f.sameTitle.Key]; s.Kind != "duplicates" || s.Confidence != "high" || s.Reason != "The same title." {
		t.Errorf("same title: %+v", s)
	}
	if s := got[f.alike.Key]; s.Kind != "relates" || s.Reason != "Shares production, stops, walk." {
		t.Errorf("alike: %+v", s)
	}
	// Another project's issue relates; it is not a duplicate.
	if s := got[f.otherProj.Key]; s.Kind != "relates" {
		t.Errorf("other project: %+v", s)
	}
	for _, i := range []*IssueDetail{f.done, f.linked, f.unrelated, f.private, f.src} {
		if s, ok := got[i.Key]; ok {
			t.Errorf("suggested %s %q: %+v", i.Key, i.Title, s)
		}
	}
	if order[0] != f.waits.Key || order[1] != f.sameTitle.Key {
		t.Errorf("stated and same-title links come first: %v", order)
	}
	// Someone who can't see the issue gets nothing.
	if _, err := e.s.SuggestRelations(e.ctx, e.user("kim", false), f.src.Key); err == nil {
		t.Error("an outsider got suggestions")
	}
}

func TestAutoRelateLinksTheSureOnes(t *testing.T) {
	e := newEnv(t)
	f := newRelateFixture(e, &ProjectSettings{AutoRelate: true})
	e.s.Wait()
	rels := map[string]string{}
	for _, r := range e.get(e.admin, f.src.Key).Relations {
		rels[r.Issue.Key] = r.Kind
	}
	if rels[f.waits.Key] != "blocked_by" || rels[f.sameTitle.Key] != "duplicates" {
		t.Errorf("relations = %v", rels)
	}
	// Other projects stay out of it, and so does what isn't sure.
	for _, i := range []*IssueDetail{f.otherProj, f.private, f.unrelated} {
		if _, ok := rels[i.Key]; ok {
			t.Errorf("linked %s: %v", i.Key, rels)
		}
	}
	for k, kind := range rels {
		if kind == "relates" && k != f.alike.Key {
			t.Errorf("linked %s, not a sure one: %v", k, rels)
		}
	}
}
