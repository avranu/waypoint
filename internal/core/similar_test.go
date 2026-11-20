package core

import (
	"strings"
	"testing"
)

// Text about to be filed is checked against the open issues (WP-104).
func TestSimilarIssues(t *testing.T) {
	e := newEnv(t)
	f := newRelateFixture(e, nil)
	again := SimilarQuery{Project: f.src.ProjectKey, Title: f.src.Title, Description: f.src.Description}
	res, err := e.s.SimilarIssues(e.ctx, f.member, again)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]SimilarIssue{}
	var order []string
	for _, m := range res {
		got[m.Key] = m
		order = append(order, m.Key)
	}
	// The project's issues with the title come first and are the same work.
	if len(order) < 2 || order[0] != f.src.Key || order[1] != f.sameTitle.Key {
		t.Fatalf("order = %v", order)
	}
	if m := got[f.src.Key]; !m.Same || m.Confidence != "high" || m.Reason != "The same title." || m.Status != "To Do" || m.Score != 1 {
		t.Errorf("filing it again: %+v", m)
	}
	// Another project's copy is like it, not the same work here.
	if m, ok := got[f.otherProj.Key]; !ok || m.Same || m.SameProject {
		t.Errorf("other project: %+v (listed %v)", m, ok)
	}
	if m, ok := got[f.alike.Key]; ok && !strings.HasPrefix(m.Reason, "Shares ") {
		t.Errorf("alike: %+v", m)
	}
	for _, i := range []*IssueDetail{f.done, f.unrelated, f.private} {
		if m, ok := got[i.Key]; ok {
			t.Errorf("listed %s %q: %+v", i.Key, i.Title, m)
		}
	}
	// A follow-up names what it follows: like it, not the same work.
	followUp := again
	followUp.Description += " Follow-up of " + f.src.Key + "."
	if res, err := e.s.SimilarIssues(e.ctx, f.member, followUp); err != nil || len(res) == 0 || res[0].Key != f.src.Key || res[0].Same {
		t.Errorf("follow-up: %+v %v", res, err)
	}
	if len(res) > maxSimilarIssues {
		t.Errorf("%d listed", len(res))
	}

	// Under a parent: the parent stays out, and its other parts read alike
	// without being the same work.
	plan := e.issue(e.admin, IssueInput{Project: f.src.ProjectKey, Title: "Sanborn campus towns plan"})
	part := e.issue(e.admin, IssueInput{Parent: plan.Key, Title: "Sanborn campus towns: harvest the county sheets"})
	res, err = e.s.SimilarIssues(e.ctx, f.member, SimilarQuery{Parent: plan.Key, Title: "Sanborn campus towns: harvest the county sheets", Description: "Part two."})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range res {
		switch m.Key {
		case plan.Key:
			t.Errorf("the parent is listed: %+v", m)
		case part.Key:
			if !m.Sibling || m.Same {
				t.Errorf("sibling: %+v", m)
			}
		}
	}
	if len(res) == 0 || res[0].Key != part.Key {
		t.Errorf("the sibling with the title comes first: %+v", res)
	}

	// No title, nothing to compare; someone outside the project is refused.
	if res, err := e.s.SimilarIssues(e.ctx, f.member, SimilarQuery{Project: f.src.ProjectKey}); err != nil || len(res) != 0 {
		t.Errorf("no title: %v %v", res, err)
	}
	if _, err := e.s.SimilarIssues(e.ctx, e.user("kim", false), again); err == nil {
		t.Error("an outsider got similar issues")
	}
}
